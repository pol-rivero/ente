import { ensureLocalUser } from "ente-accounts/services/user";
import { isNamedError } from "ente-base/error";
import log from "ente-base/log";
import { wait } from "ente-utils/promise";
import { encryptJSONBox } from "./crypto";
import {
    collectionCheckedTimeKey,
    collectionRecheckKeys,
    collectionSinceTimeKey,
    collectionsRecheckKeys,
    collectionsSinceTimeKey,
    trashRecheckKeys,
    trashSinceTimeKey,
    type RecheckKeys,
} from "./db";
import {
    clearMirror,
    collectionKeyOf,
    currentMirrorUserID,
    loadMirror,
    mirrorCollection,
    mirrorCollections,
    mirrorMeta,
    MirrorResetError,
    publishMirror,
    reloadMirror,
    withPublishesHeld,
    writeMirror,
} from "./mirror";
import {
    collectionDetails,
    isSameCollectionRecord,
    storedCollection,
    storedFile,
    storedTrashItem,
    type StoredCollection,
    type StoredFile,
    type StoredTrashItem,
} from "./records";
import {
    isDriveApiError,
    isNetworkError,
    isUnauthorizedError,
} from "./remote/errors";
import { driveRequestJSON } from "./remote/request";
import {
    CollectionsResponse,
    DiffResponse,
    parseItems,
    RemoteCollection,
    RemoteFile,
    RemoteTrashItem,
} from "./remote/schemas";
import { ensureDriveServerSupported } from "./server-capability";
import { driveState, updateSyncState } from "./store";
import { onOtherTabMessage, withTabLock } from "./tabs";

// Museum picks a row's updationTime before its transaction commits, so a
// slower transaction can commit a row older than one we already have and a
// cursor would skip it. After a cursor moves, the rows from just before it
// are fetched again once such commits must have landed, and in the syncs
// that follow operations (which look for their own writes). Re-fetched rows
// that didn't change are ignored.
const overlapMicroseconds = 30 * 1000 * 1000;
const recheckDelayMs = 30 * 1000;
const diffConcurrency = 4;
const publishIntervalMs = 1500;

const maxTime = (
    items: readonly unknown[] | null | undefined,
    field: string,
) => {
    let max = 0;
    for (const item of items ?? []) {
        const value = (item as Record<string, unknown> | null)?.[field];
        if (typeof value == "number" && value > max) max = value;
    }
    return max;
};

const isRecheckWanted = (keys: RecheckKeys, isAfterOperation: boolean) => {
    const at = mirrorMeta(keys.at) ?? 0;
    return at > 0 && (isAfterOperation || Date.now() >= at);
};

// Where a fetch from `cursor` starts, and the pending recheck (stored at
// `keys`) to write along with each page.
const beginRecheck = (
    keys: RecheckKeys,
    cursor: number,
    isAfterOperation: boolean,
) => {
    const pendingSince = mirrorMeta(keys.since) ?? 0;
    const pendingAt = mirrorMeta(keys.at) ?? 0;
    const isDue = pendingAt > 0 && Date.now() >= pendingAt;
    let opened: { since: number; at: number } | undefined;
    return {
        sinceTime: isRecheckWanted(keys, isAfterOperation)
            ? Math.min(cursor, pendingSince)
            : cursor,
        // After a page that moved the cursor from `from` to `to`.
        meta: (from: number, to: number, isDone: boolean) => {
            if (to > from)
                opened = {
                    since: Math.min(
                        opened?.since ?? Infinity,
                        Math.max(0, to - overlapMicroseconds),
                    ),
                    at: Date.now() + recheckDelayMs,
                };
            const kept =
                pendingAt > 0 && !(isDone && isDue)
                    ? { since: pendingSince, at: pendingAt }
                    : undefined;
            const at = Math.max(kept?.at ?? 0, opened?.at ?? 0);
            const since = Math.min(
                kept?.since ?? Infinity,
                opened?.since ?? Infinity,
            );
            return { [keys.since]: at ? since : 0, [keys.at]: at };
        },
    };
};

const syncCollections = async (isAfterOperation: boolean) => {
    const storedSinceTime = mirrorMeta(collectionsSinceTimeKey) ?? 0;
    const recheck = beginRecheck(
        collectionsRecheckKeys,
        storedSinceTime,
        isAfterOperation,
    );
    const { collections } = await driveRequestJSON(
        "GET",
        "/collections/v2",
        CollectionsResponse,
        { query: { sinceTime: recheck.sinceTime } },
    );

    const putCollections: StoredCollection[] = [];
    const deleteCollections: number[] = [];
    const clearCollections: number[] = [];
    let cursor = Math.max(
        storedSinceTime,
        maxTime(collections, "updationTime"),
    );
    for (const c of parseItems(RemoteCollection, collections, "collection")) {
        if (c.isDeleted) {
            clearCollections.push(c.id);
            // Deleted folders keep their key: trashed files are still
            // wrapped with it.
            if (c.encryptedKey)
                putCollections.push(storedCollection(c, undefined));
            else deleteCollections.push(c.id);
            continue;
        }
        const record = storedCollection(c, undefined);
        const stored = mirrorCollection(c.id);
        if (stored?.details && isSameCollectionRecord(stored, record)) continue;
        try {
            const key = await collectionKeyOf(record);
            record.details = await encryptJSONBox(collectionDetails(c), key);
        } catch (e) {
            if (e instanceof MirrorResetError) throw e;
            log.error(`Failed to open the key of collection ${c.id}`, e);
            // Fetch it again next time, when its key may open.
            cursor = Math.min(cursor, c.updationTime - 1);
        }
        putCollections.push(record);
    }

    cursor = Math.max(0, cursor);
    await writeMirror({
        putCollections,
        deleteCollections,
        clearCollections,
        meta: {
            [collectionsSinceTimeKey]: cursor,
            ...recheck.meta(storedSinceTime, cursor, true),
        },
    });
};

const syncCollectionFiles = async (
    collection: StoredCollection,
    isAfterOperation: boolean,
) => {
    const { id } = collection;
    const storedSinceTime = mirrorMeta(collectionSinceTimeKey(id)) ?? 0;
    const recheck = beginRecheck(
        collectionRecheckKeys(id),
        storedSinceTime,
        isAfterOperation,
    );
    let sinceTime = recheck.sinceTime;
    let cursor = storedSinceTime;
    for (;;) {
        let page;
        try {
            page = await driveRequestJSON(
                "GET",
                "/collections/v2/diff",
                DiffResponse,
                { query: { collectionID: id, sinceTime } },
            );
        } catch (e) {
            if (isDriveApiError(e, 404) && e.body != "text") {
                log.info(`Dropping the contents of deleted collection ${id}`);
                await writeMirror({
                    putCollections: [{ ...collection, isDeleted: true }],
                    clearCollections: [id],
                });
                return;
            }
            throw e;
        }

        const putFiles: StoredFile[] = [];
        const deleteFiles: [number, number][] = [];
        for (const f of parseItems(RemoteFile, page.diff, "file")) {
            if (f.collectionID != id) continue;
            const record = f.isDeleted ? undefined : storedFile(f);
            if (record) putFiles.push(record);
            else deleteFiles.push([f.id, id]);
        }
        // Pages never split rows that share a timestamp, so the max
        // updationTime received is a safe cursor.
        const pageMax = maxTime(page.diff, "updationTime");
        const nextSinceTime = Math.max(sinceTime, pageMax);
        const isDone = !page.hasMore || nextSinceTime == sinceTime;
        if (page.hasMore && nextSinceTime == sinceTime)
            log.warn(`File diff of collection ${id} did not advance`);
        const previousCursor = cursor;
        cursor = Math.max(cursor, pageMax);
        await writeMirror({
            putFiles,
            deleteFiles,
            meta: {
                [collectionSinceTimeKey(id)]: cursor,
                ...recheck.meta(previousCursor, cursor, isDone),
                ...(isDone && {
                    [collectionCheckedTimeKey(id)]: collection.updationTime,
                }),
            },
        });
        if (isDone) return;
        sinceTime = nextSinceTime;
    }
};

const needsFileSync = (c: StoredCollection, isAfterOperation: boolean) =>
    !c.isDeleted &&
    (c.updationTime >
        Math.max(
            mirrorMeta(collectionSinceTimeKey(c.id)) ?? 0,
            mirrorMeta(collectionCheckedTimeKey(c.id)) ?? 0,
        ) ||
        isRecheckWanted(collectionRecheckKeys(c.id), isAfterOperation));

const syncTrash = async (isAfterOperation: boolean) => {
    const storedSinceTime = mirrorMeta(trashSinceTimeKey) ?? 0;
    const recheck = beginRecheck(
        trashRecheckKeys,
        storedSinceTime,
        isAfterOperation,
    );
    let sinceTime = recheck.sinceTime;
    let cursor = storedSinceTime;
    for (;;) {
        const page = await driveRequestJSON(
            "GET",
            "/trash/v2/diff",
            DiffResponse,
            { query: { sinceTime } },
        );
        const putTrash: StoredTrashItem[] = [];
        const deleteTrash: number[] = [];
        for (const t of parseItems(RemoteTrashItem, page.diff, "trash item")) {
            const record =
                t.isDeleted || t.isRestored ? undefined : storedTrashItem(t);
            if (record) putTrash.push(record);
            else deleteTrash.push(t.file.id);
        }
        const nextSinceTime = Math.max(
            sinceTime,
            maxTime(page.diff, "updatedAt"),
        );
        const isDone = !page.hasMore || nextSinceTime == sinceTime;
        const previousCursor = cursor;
        cursor = Math.max(cursor, nextSinceTime);
        await writeMirror({
            putTrash,
            deleteTrash,
            meta: {
                [trashSinceTimeKey]: cursor,
                ...recheck.meta(previousCursor, cursor, isDone),
            },
        });
        if (isDone) return;
        sinceTime = nextSinceTime;
    }
};

// Errors that stop the whole sync rather than one folder.
const isSyncWideError = (e: unknown) =>
    e instanceof MirrorResetError ||
    isNetworkError(e) ||
    isUnauthorizedError(e) ||
    isNamedError(e, "drive_server_unsupported");

// Runs `task` over `items`, at most `concurrency` at a time. After a
// failure, no new tasks start, and the first error is thrown once the
// running ones are done.
const runConcurrently = async <T>(
    items: readonly T[],
    concurrency: number,
    task: (item: T) => Promise<void>,
) => {
    let next = 0;
    let failure: { error: unknown } | undefined;
    const worker = async () => {
        while (!failure && next < items.length) {
            const item = items[next++]!;
            try {
                await task(item);
            } catch (error) {
                failure ??= { error };
            }
        }
    };
    await Promise.all(
        Array.from({ length: Math.min(concurrency, items.length) }, worker),
    );
    if (failure) throw failure.error;
};

const publishInBackground = () =>
    publishMirror({ ifStale: true }).catch((e: unknown) => {
        if (!(e instanceof MirrorResetError))
            log.error("Failed to publish the Drive state", e);
    });

interface SyncOptions {
    isAfterOperation: boolean;
    showsProgress: boolean;
}

// Returns the IDs of folders whose files failed to sync.
const syncFiles = async ({ isAfterOperation, showsProgress }: SyncOptions) => {
    const pending = mirrorCollections().filter((c) =>
        needsFileSync(c, isAfterOperation),
    );
    if (!pending.length) return [];
    const failed: number[] = [];
    let done = 0;
    const total = pending.length;
    const reportProgress = () => {
        if (showsProgress) updateSyncState({ progress: { done, total } });
    };
    reportProgress();
    let lastPublish = Date.now();
    const syncOne = async (
        c: StoredCollection,
        publishPeriodically: boolean,
    ) => {
        try {
            await syncCollectionFiles(c, isAfterOperation);
        } catch (e) {
            if (isSyncWideError(e)) throw e;
            log.error(`Failed to sync the files of collection ${c.id}`, e);
            failed.push(c.id);
        }
        done++;
        reportProgress();
        if (
            publishPeriodically &&
            Date.now() - lastPublish > publishIntervalMs
        ) {
            lastPublish = Date.now();
            void publishInBackground();
        }
    };

    // Changes to folders we already have are published together, so that a
    // file moved between two of them is never shown half moved. Folders we
    // have never fetched (a new device, a newly shared folder) are published
    // as they arrive.
    const isNew = (c: StoredCollection) =>
        mirrorMeta(collectionCheckedTimeKey(c.id)) === undefined;
    const known = pending.filter((c) => !isNew(c));
    const fresh = pending.filter(isNew);
    await withPublishesHeld(() =>
        runConcurrently(known, diffConcurrency, (c) => syncOne(c, false)),
    );
    if (fresh.length) {
        await publishMirror({ ifStale: true });
        lastPublish = Date.now();
        await runConcurrently(fresh, diffConcurrency, (c) => syncOne(c, true));
    }
    return failed.sort((a, b) => a - b);
};

const hydrate = async (userID: number) => {
    isChangedElsewhere = false;
    try {
        await loadMirror(userID);
        await publishMirror();
    } catch (e) {
        if (e instanceof MirrorResetError) throw e;
        log.error("Failed to load the local Drive state", e);
    }
    updateSyncState({ isHydrated: true });
};

// Other tabs

// Set when another tab wrote to the database since this one read it.
let isChangedElsewhere = false;
let isHoldingSyncLock = false;
let reloading: Promise<void> | undefined;
// Lets a burst of writes in another tab end before reading them.
const reloadDelayMs = 300;

const reloadChanges = async (userID: number, delayMs = 0) => {
    while (isChangedElsewhere && currentMirrorUserID() == userID) {
        if (delayMs) await wait(delayMs);
        isChangedElsewhere = false;
        try {
            await reloadMirror(userID);
            await publishMirror({ ifStale: true });
        } catch (e) {
            if (e instanceof MirrorResetError) throw e;
            log.error("Failed to reload the local Drive state", e);
        }
    }
};

const isHidden = () =>
    typeof document != "undefined" && document.visibilityState == "hidden";

// Shows what another tab wrote. Waits while this tab is hidden (until it is
// shown) or syncing (until it is done, having reloaded before writing).
const reloadInBackground = () => {
    const userID = currentMirrorUserID();
    if (!isChangedElsewhere || userID === undefined) return;
    if (reloading || isHoldingSyncLock || isHidden()) return;
    reloading = reloadChanges(userID, reloadDelayMs)
        .catch(() => undefined)
        .finally(() => (reloading = undefined));
};

const syncLocked = async (userID: number, options: SyncOptions) => {
    await reloading;
    await reloadChanges(userID);
    await ensureDriveServerSupported();
    await syncCollections(options.isAfterOperation);
    await publishMirror({ ifStale: true });
    const failedFolderIDs = await syncFiles(options);
    await syncTrash(options.isAfterOperation);
    return failedFolderIDs;
};

// Only one tab syncs at a time; the others read what it wrote. Reading the
// local state doesn't wait for the lock.
const syncOnce = async (options: SyncOptions) => {
    const userID = ensureLocalUser().id;
    if (currentMirrorUserID() != userID) await hydrate(userID);
    try {
        return await withTabLock(`ente-drive-sync-${userID}`, async () => {
            isHoldingSyncLock = true;
            try {
                return await syncLocked(userID, options);
            } finally {
                isHoldingSyncLock = false;
            }
        });
    } finally {
        await publishInBackground();
        reloadInBackground();
    }
};

// Scheduling

const syncInterval = 30 * 1000;
const minRetryDelay = 5 * 1000;
const maxRetryDelay = 5 * 60 * 1000;
// Focus and visibility changes come in pairs.
const minAutoSyncGap = 2 * 1000;

let inFlight: Promise<boolean> | undefined;
let queued: Promise<boolean> | undefined;
let isQueuedQuiet = true;
let isQueuedAfterOperation = false;
let failures = 0;
let retryTimer: ReturnType<typeof setTimeout> | undefined;
let retryAt = 0;
let lastFinishedAt = 0;
let stopped = true;
// Set by errors that syncing again won't fix (an expired session, an
// unsupported server): no automatic syncs until syncing is restarted.
let isHalted = false;
// Waiting for the next successful sync (or for syncing to halt).
const syncListeners = new Set<() => void>();

const notifySyncListeners = () => {
    for (const notify of [...syncListeners]) notify();
};

const scheduleRetry = () => {
    clearTimeout(retryTimer);
    const delay = Math.min(maxRetryDelay, minRetryDelay * 2 ** failures);
    failures++;
    retryAt = Date.now() + delay;
    retryTimer = setTimeout(() => void syncDrive(), delay);
};

// Resolves to whether the sync succeeded.
const runSync = async (isQuiet: boolean, isAfterOperation: boolean) => {
    if (!isQuiet || driveState().sync.status != "idle")
        updateSyncState({ status: "syncing" });
    const showsProgress = driveState().sync.status == "syncing";
    try {
        const failedFolderIDs = await syncOnce({
            isAfterOperation,
            showsProgress,
        });
        failures = 0;
        retryAt = 0;
        clearTimeout(retryTimer);
        updateSyncState({
            status: "idle",
            error: undefined,
            failedFolderIDs,
            lastSyncedAt: Date.now(),
            progress: undefined,
        });
        notifySyncListeners();
        return true;
    } catch (e) {
        updateSyncState({ progress: undefined });
        if (e instanceof MirrorResetError) return false;
        if (isNetworkError(e)) {
            log.warn("Drive sync failed: offline", e);
            updateSyncState({ status: "offline", error: undefined });
            if (!stopped) scheduleRetry();
        } else if (isUnauthorizedError(e)) {
            log.error("Drive sync failed: unauthorized", e);
            haltSync("unauthorized");
        } else if (isNamedError(e, "drive_server_unsupported")) {
            haltSync("unsupported_server");
        } else {
            log.error("Drive sync failed", e);
            updateSyncState({ status: "error", error: "failed" });
            if (!stopped) scheduleRetry();
        }
        return false;
    } finally {
        lastFinishedAt = Date.now();
    }
};

const haltSync = (error: "unauthorized" | "unsupported_server") => {
    isHalted = true;
    clearTimeout(retryTimer);
    updateSyncState({ status: "error", error });
    notifySyncListeners();
};

// Operations report an expired session they ran into.
export const reportUnauthorized = () => haltSync("unauthorized");

const requestSync = (
    isQuiet: boolean,
    isAfterOperation: boolean,
): Promise<boolean> => {
    if (isQuiet && isHalted) return Promise.resolve(false);
    if (!inFlight) {
        inFlight = runSync(isQuiet, isAfterOperation).finally(
            () => (inFlight = undefined),
        );
        return inFlight;
    }
    isQueuedQuiet &&= isQuiet;
    isQueuedAfterOperation ||= isAfterOperation;
    queued ??= inFlight.then(() => {
        queued = undefined;
        const quiet = isQueuedQuiet;
        const afterOperation = isQueuedAfterOperation;
        isQueuedQuiet = true;
        isQueuedAfterOperation = false;
        return requestSync(quiet, afterOperation);
    });
    return queued;
};

// Single flight: a request made while a sync runs is served by one more
// sync that starts after it, so it sees every change made before the call.
export const syncDrive = async (): Promise<void> => {
    await requestSync(false, false);
};

// The sync after an operation, which shows as the operation's pending state
// rather than as a global "syncing" status. Resolves to whether it succeeded.
export const syncAfterOperation = () => requestSync(true, true);

// Resolves after the next sync that succeeds, or after `timeoutMs`, or
// when syncing halts.
export const nextSuccessfulSync = (timeoutMs: number) =>
    new Promise<void>((resolve) => {
        if (isHalted) {
            resolve();
            return;
        }
        const done = () => {
            clearTimeout(timer);
            syncListeners.delete(done);
            resolve();
        };
        const timer = setTimeout(done, timeoutMs);
        syncListeners.add(done);
    });

const autoSync = () => {
    if (isHalted || inFlight || Date.now() < retryAt) return;
    if (Date.now() - lastFinishedAt < minAutoSyncGap) return;
    void syncDrive();
};

let stopActiveSync: (() => void) | undefined;

export const stopDriveSync = () => stopActiveSync?.();

export const startDriveSync = () => {
    stopActiveSync?.();
    stopped = false;
    isHalted = false;
    void syncDrive();
    const onVisibilityChange = () => {
        if (document.visibilityState != "visible") return;
        reloadInBackground();
        autoSync();
    };
    const onOnline = () => {
        if (!isHalted) void syncDrive();
    };
    window.addEventListener("focus", autoSync);
    window.addEventListener("online", onOnline);
    document.addEventListener("visibilitychange", onVisibilityChange);
    const interval = setInterval(() => {
        if (document.visibilityState == "visible") autoSync();
    }, syncInterval);
    const unsubscribe = onOtherTabMessage((message) => {
        if (message.type == "changed") {
            isChangedElsewhere = true;
            reloadInBackground();
        } else {
            stop();
            window.location.replace("/login");
        }
    });
    const stop = () => {
        if (stopActiveSync == stop) stopActiveSync = undefined;
        stopped = true;
        clearInterval(interval);
        clearTimeout(retryTimer);
        unsubscribe();
        window.removeEventListener("focus", autoSync);
        window.removeEventListener("online", onOnline);
        document.removeEventListener("visibilitychange", onVisibilityChange);
    };
    stopActiveSync = stop;
    return stop;
};

export const clearDriveLocalState = async () => {
    stopActiveSync?.();
    await clearMirror();
};
