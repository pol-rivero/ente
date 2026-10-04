import { isNamedError } from "ente-base/error";
import { wait } from "ente-utils/promise";
import { collectionKeyOf, mirrorCollection } from "../mirror";
import {
    itemKey,
    type DriveFile,
    type DriveItemRef,
    type Folder,
    type PendingDriveOp,
    type PendingDriveOpKind,
} from "../model/types";
import { normalizeItemName } from "../names";
import {
    isDriveApiError,
    isNetworkError,
    isRateLimitedError,
    isRouteMissingError,
    isTreeBusyError,
    isUnauthorizedError,
} from "../remote/errors";
import { writeRequestCount } from "../remote/request";
import { ensureDriveServerSupported } from "../server-capability";
import { addPendingOp, driveState, removePendingOp } from "../store";
import {
    nextSuccessfulSync,
    reportUnauthorized,
    syncAfterOperation,
} from "../sync";

export type DriveOperationErrorReason =
    | "invalid_name"
    | "not_found"
    | "not_owner"
    | "invalid_target"
    | "folder_deleted"
    | "cycle"
    | "max_depth"
    | "file_in_trash"
    | "conflict"
    | "busy"
    | "rate_limited"
    // The server predates the endpoint (plain-text 404), or Drive.
    | "server_outdated"
    | "has_children"
    | "not_empty"
    | "subtree_too_large"
    // No response: the operation may or may not have happened.
    | "offline"
    | "unauthorized";

export type DriveErrorReason = DriveOperationErrorReason | "unknown";

export class DriveOperationError extends Error {
    readonly reason: DriveOperationErrorReason;
    // Set when a folder was created but couldn't be moved into its parent.
    createdFolderID?: number;

    constructor(reason: DriveOperationErrorReason, cause?: unknown) {
        super(`Drive operation failed: ${reason}`, { cause });
        this.name = "DriveOperationError";
        this.reason = reason;
    }
}

export const isDriveOperationError = (
    e: unknown,
    reason?: DriveOperationErrorReason,
): e is DriveOperationError =>
    e instanceof DriveOperationError && (!reason || e.reason == reason);

const mapError = (
    e: unknown,
    codes: Record<string, DriveOperationErrorReason>,
) => {
    if (e instanceof DriveOperationError) return e;
    if (isNetworkError(e)) return new DriveOperationError("offline", e);
    if (isUnauthorizedError(e))
        return new DriveOperationError("unauthorized", e);
    if (isNamedError(e, "drive_server_unsupported") || isRouteMissingError(e))
        return new DriveOperationError("server_outdated", e);
    if (isDriveApiError(e)) {
        const reason = e.code ? codes[e.code] : undefined;
        if (reason) return new DriveOperationError(reason, e);
        if (isTreeBusyError(e)) return new DriveOperationError("busy", e);
        if (isRateLimitedError(e))
            return new DriveOperationError("rate_limited", e);
        if (e.status == 403) return new DriveOperationError("not_owner", e);
    }
    return e;
};

// Maps errors shared by most endpoints; `codes` adds endpoint-specific ones.
export const operationError = (
    e: unknown,
    codes: Record<string, DriveOperationErrorReason> = {},
) => {
    const mapped = mapError(e, codes);
    if (mapped != e && isDriveOperationError(mapped, "unauthorized"))
        reportUnauthorized();
    return mapped;
};

export const driveErrorReason = (e: unknown): DriveErrorReason => {
    const mapped = mapError(e, {});
    return mapped instanceof DriveOperationError ? mapped.reason : "unknown";
};

// Every write first confirms that the server supports Drive (plan C5), since
// older servers would put the data in the user's Photos library.
export const beginWrite = async () => {
    try {
        await ensureDriveServerSupported();
    } catch (e) {
        throw operationError(e);
    }
};

interface QueuedOperation {
    keys: ReadonlySet<string>;
    settled: Promise<void>;
}

// Operations in the order they were called, until they settle.
const queue: QueuedOperation[] = [];
// Operations past their wait for earlier ones.
const startedOpIDs = new Set<number>();

// Without a successful sync after it, an operation stops being pending
// after this long (the sync's failure shows in the sync state).
const settleTimeoutMs = 2 * 60 * 1000;

const operationKeys = (
    items: readonly DriveItemRef[],
    targetFolderID: number | null | undefined,
) => {
    const keys = new Set(items.map(itemKey));
    if (targetFolderID != null)
        keys.add(itemKey({ kind: "folder", id: targetFolderID }));
    return keys;
};

const settleAfterSync = async () => {
    if (await syncAfterOperation()) return;
    await nextSuccessfulSync(settleTimeoutMs);
};

interface OperationSpec {
    kind: PendingDriveOpKind;
    items: readonly DriveItemRef[];
    targetFolderID?: number | null;
}

// Runs a write as a pending operation, after the earlier operations on the
// same items (or into the same folder) have settled, so that it starts from
// where they left things. It resolves as soon as the server has done it; the
// operation (and the names it reserved) stays pending (`pendingItemKeys`)
// until a sync has brought its result into the model. That sync follows any
// request that may have changed something, failed ones too, since a failed
// batch may have partly happened.
export const runOperation = async <T>(
    { kind, items, targetFolderID }: OperationSpec,
    write: (op: PendingDriveOp) => Promise<T>,
): Promise<T> => {
    const keys = operationKeys(items, targetFolderID);
    const earlier = queue
        .filter((queued) => [...keys].some((key) => queued.keys.has(key)))
        .map((queued) => queued.settled);
    const op = addPendingOp(kind, items, targetFolderID);
    let settle!: () => void;
    const entry = {
        keys,
        settled: new Promise<void>((resolve) => (settle = resolve)),
    };
    queue.push(entry);
    let writesBefore = writeRequestCount();
    try {
        await Promise.all(earlier);
        writesBefore = writeRequestCount();
        startedOpIDs.add(op.id);
        await beginWrite();
        return await write(op);
    } catch (e) {
        throw operationError(e);
    } finally {
        const isWritten = writeRequestCount() != writesBefore;
        void (isWritten ? settleAfterSync() : Promise.resolve()).finally(() => {
            removePendingOp(op);
            releaseNames(op);
            startedOpIDs.delete(op.id);
            queue.splice(queue.indexOf(entry), 1);
            settle();
        });
    }
};

// Names chosen by operations whose results aren't in the model yet, so that
// concurrent operations don't pick the same one.
const reservations = new Map<
    number,
    { parentID: number | null; name: string }[]
>();

export const reserveName = (
    op: PendingDriveOp,
    parentID: number | null,
    name: string,
) => {
    const names = reservations.get(op.id) ?? [];
    names.push({ parentID, name });
    reservations.set(op.id, names);
};

const releaseNames = (op: PendingDriveOp) => reservations.delete(op.id);

export const reservedNames = (parentID: number | null) =>
    [...reservations.values()]
        .flat()
        .filter((r) => r.parentID === parentID)
        .map((r) => r.name);

export const validName = (name: string) => {
    const valid = normalizeItemName(name);
    if (!valid) throw new DriveOperationError("invalid_name");
    return valid;
};

// My Drive's root collection is addressed as null.
export const parentForNames = (folderID: number | null) =>
    folderID === null || folderID == driveState().model.rootCollectionID
        ? null
        : folderID;

// Folders that started being deleted (a delete queued after an operation
// doesn't fail it).
export const isFolderDeleting = (folderID: number) => {
    const { pendingOps, pendingTrash } = driveState();
    return (
        pendingTrash.some((entry) => entry.folderIDs.includes(folderID)) ||
        pendingOps.some(
            (op) =>
                op.kind == "delete_folder" &&
                startedOpIDs.has(op.id) &&
                op.items.some((i) => i.kind == "folder" && i.id == folderID),
        )
    );
};

export const ownedFolder = (folderID: number): Folder => {
    const folder = driveState().model.folders.get(folderID);
    if (!folder) throw new DriveOperationError("not_found");
    if (!folder.isOwned) throw new DriveOperationError("not_owner");
    if (isFolderDeleting(folderID))
        throw new DriveOperationError("folder_deleted");
    return folder;
};

export const ownedFile = (fileID: number): DriveFile => {
    const file = driveState().model.files.get(fileID);
    if (!file) throw new DriveOperationError("not_found");
    if (!file.isOwned) throw new DriveOperationError("not_owner");
    return file;
};

export const folderKey = async (folderID: number) => {
    const record = mirrorCollection(folderID);
    if (!record || record.isDeleted)
        throw new DriveOperationError("folder_deleted");
    return collectionKeyOf(record);
};

const retryDelays = [1000, 2000, 4000, 8000, 16000];

export const retryWhenBusy = async <T>(operation: () => Promise<T>) => {
    for (let attempt = 0; ; attempt++) {
        try {
            return await operation();
        } catch (e) {
            const isRetriable = isTreeBusyError(e) || isRateLimitedError(e);
            if (!isRetriable || attempt >= retryDelays.length) throw e;
            await wait(retryDelays[attempt]!);
        }
    }
};

const maxItemsPerRequest = 1000;

export const batches = <T>(items: readonly T[], size = maxItemsPerRequest) => {
    const result: T[][] = [];
    for (let i = 0; i < items.length; i += size)
        result.push(items.slice(i, i + size));
    return result;
};

export interface ItemFailure {
    ref: DriveItemRef;
    reason: DriveErrorReason;
}

export interface BatchResult {
    // Items that got a different name to avoid a clash.
    renamed: { ref: DriveItemRef; name: string }[];
    failed: ItemFailure[];
}
