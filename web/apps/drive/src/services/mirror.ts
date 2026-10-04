import log from "ente-base/log";
import {
    clearDriveDBs,
    collectionMetaKeys,
    loadDriveDB,
    writeDriveDB,
    type DriveDBChanges,
    type DriveDBSnapshot,
} from "./db";
import { ModelBuilder } from "./model/build";
import { decryptState, DriveDecryptor } from "./model/decrypt";
import type { DriveModel, PendingTrash } from "./model/types";
import {
    isSameCollectionRecord,
    isSameFileRecord,
    isSameTrashRecord,
    type StoredCollection,
    type StoredFile,
    type StoredPendingTrash,
    type StoredTrashItem,
} from "./records";
import {
    driveState,
    publishModel,
    resetDriveState,
    setPendingTrash,
    updateSyncState,
} from "./store";
import { postToOtherTabs } from "./tabs";

// The in-memory copy of the encrypted IndexedDB contents, kept in step with
// every write so that syncs and operations don't re-read the database.
interface Mirror {
    userID: number;
    collections: Map<number, StoredCollection>;
    // File ID → collection ID → record.
    files: Map<number, Map<number, StoredFile>>;
    // Collection ID → IDs of its files.
    filesByCollection: Map<number, Set<number>>;
    trash: Map<number, StoredTrashItem>;
    pendingTrash: Map<number, StoredPendingTrash>;
    meta: Map<string, number>;
    // False once the database turned out to be unusable: from then on the
    // state lives only in memory.
    isPersistent: boolean;
}

let mirror: Mirror | undefined;
let decryptor = new DriveDecryptor();
let builder = new ModelBuilder();
let generation = 0;
let lastUserID: number | undefined;
// Whether records changed since the model was last published.
let isModelStale = true;

export class MirrorResetError extends Error {
    constructor() {
        super("The local Drive state was cleared");
        this.name = "MirrorResetError";
    }
}

export const currentMirrorUserID = () => mirror?.userID;

const emptySnapshot = (): DriveDBSnapshot => ({
    collections: [],
    files: [],
    trash: [],
    pendingTrash: [],
    meta: new Map(),
});

const putFile = (m: Mirror, f: StoredFile) => {
    const rows = m.files.get(f.id);
    if (rows) rows.set(f.collectionID, f);
    else m.files.set(f.id, new Map([[f.collectionID, f]]));
    const ids = m.filesByCollection.get(f.collectionID);
    if (ids) ids.add(f.id);
    else m.filesByCollection.set(f.collectionID, new Set([f.id]));
};

const deleteFile = (m: Mirror, id: number, cid: number) => {
    const rows = m.files.get(id);
    rows?.delete(cid);
    if (rows?.size === 0) m.files.delete(id);
    m.filesByCollection.get(cid)?.delete(id);
};

const mirrorContents = (snapshot: DriveDBSnapshot) => {
    const contents = {
        collections: new Map(snapshot.collections.map((c) => [c.id, c])),
        files: new Map<number, Map<number, StoredFile>>(),
        filesByCollection: new Map<number, Set<number>>(),
        trash: new Map(snapshot.trash.map((t) => [t.id, t])),
        pendingTrash: new Map(
            snapshot.pendingTrash.map((p) => [p.folderID, p]),
        ),
        meta: snapshot.meta,
    };
    for (const f of snapshot.files) putFile(contents as Mirror, f);
    return contents;
};

let lastWrite: Promise<unknown> = Promise.resolve();

// Writes (and reloads) run one at a time, each starting from the in-memory
// state the previous one left.
const exclusively = <T>(task: () => Promise<T>): Promise<T> => {
    const result = lastWrite.then(task);
    lastWrite = result.catch(() => undefined);
    return result;
};

// Loads the user's database into memory.
export const loadMirror = async (userID: number) => {
    if (mirror?.userID == userID) return;
    const startedGeneration = generation;
    let snapshot: DriveDBSnapshot;
    let isPersistent = true;
    try {
        snapshot = await loadDriveDB(userID);
    } catch (e) {
        log.error(
            "The local Drive database is unusable, keeping it in memory",
            e,
        );
        snapshot = emptySnapshot();
        isPersistent = false;
    }
    if (startedGeneration != generation) throw new MirrorResetError();
    if (mirror?.userID == userID) return;
    isModelStale = true;
    lastUserID = userID;
    decryptor = new DriveDecryptor();
    builder = new ModelBuilder();
    mirror = { userID, ...mirrorContents(snapshot), isPersistent };
    updateSyncState({ isPersistent });
};

// Re-reads the database in place, after another tab wrote to it.
export const reloadMirror = (userID: number) =>
    exclusively(async () => {
        const current = mirror;
        if (current?.userID != userID || !current.isPersistent) return;
        const startedGeneration = generation;
        const snapshot = await loadDriveDB(userID);
        if (startedGeneration != generation || mirror != current)
            throw new MirrorResetError();
        Object.assign(current, mirrorContents(snapshot));
        isModelStale = true;
    });

const ensureMirror = () => {
    if (!mirror) throw new MirrorResetError();
    return mirror;
};

const isSamePendingTrash = (a: StoredPendingTrash, b: StoredPendingTrash) =>
    a.startedAt == b.startedAt &&
    a.folderIDs.join() == b.folderIDs.join() &&
    a.fileIDs.join() == b.fileIDs.join();

const hasCollectionData = (m: Mirror, id: number) =>
    !!m.filesByCollection.get(id)?.size ||
    collectionMetaKeys(id).some((key) => m.meta.has(key));

// Drops the changes that wouldn't change what's stored.
const effectiveChanges = (
    m: Mirror,
    changes: DriveDBChanges,
): DriveDBChanges => {
    const putCollections = changes.putCollections?.filter((c) => {
        const existing = m.collections.get(c.id);
        return !existing || !isSameCollectionRecord(existing, c);
    });
    const putFiles = changes.putFiles?.filter((f) => {
        const existing = m.files.get(f.id)?.get(f.collectionID);
        return !existing || !isSameFileRecord(existing, f);
    });
    const putTrash = changes.putTrash?.filter((t) => {
        const existing = m.trash.get(t.id);
        return !existing || !isSameTrashRecord(existing, t);
    });
    const deleteFiles = changes.deleteFiles?.filter(([id, cid]) =>
        m.files.get(id)?.has(cid),
    );
    const deleteTrash = changes.deleteTrash?.filter((id) => m.trash.has(id));
    const deleteCollections = changes.deleteCollections?.filter((id) =>
        m.collections.has(id),
    );
    const clearCollections = changes.clearCollections?.filter((id) =>
        hasCollectionData(m, id),
    );
    const putPendingTrash = changes.putPendingTrash?.filter((p) => {
        const existing = m.pendingTrash.get(p.folderID);
        return !existing || !isSamePendingTrash(existing, p);
    });
    const deletePendingTrash = changes.deletePendingTrash?.filter((id) =>
        m.pendingTrash.has(id),
    );
    // Absent sync times count as 0.
    const meta = Object.fromEntries(
        Object.entries(changes.meta ?? {}).filter(
            ([key, value]) => (m.meta.get(key) ?? 0) !== value,
        ),
    );
    return {
        putCollections,
        deleteCollections,
        clearCollections,
        putFiles,
        deleteFiles,
        putTrash,
        deleteTrash,
        putPendingTrash,
        deletePendingTrash,
        meta,
    };
};

const hasRecordChanges = (changes: DriveDBChanges) =>
    !!(
        changes.putCollections?.length ||
        changes.deleteCollections?.length ||
        changes.clearCollections?.length ||
        changes.putFiles?.length ||
        changes.deleteFiles?.length ||
        changes.putTrash?.length ||
        changes.deleteTrash?.length
    );

const hasPendingTrashChanges = (changes: DriveDBChanges) =>
    !!(changes.putPendingTrash?.length || changes.deletePendingTrash?.length);

export const writeMirror = (requested: DriveDBChanges) =>
    exclusively(() => write(requested));

const write = async (requested: DriveDBChanges) => {
    const m = ensureMirror();
    const changes = effectiveChanges(m, requested);
    const isRecordChange = hasRecordChanges(changes);
    const isVisibleChange = isRecordChange || hasPendingTrashChanges(changes);
    if (!isVisibleChange && !Object.keys(changes.meta ?? {}).length) return;
    const startedGeneration = generation;
    if (m.isPersistent) {
        try {
            await writeDriveDB(m.userID, changes);
        } catch (e) {
            if (startedGeneration != generation) throw new MirrorResetError();
            log.error(
                "Failed to write the local Drive database, keeping it in memory",
                e,
            );
            m.isPersistent = false;
            updateSyncState({ isPersistent: false });
        }
    }
    if (startedGeneration != generation || mirror != m)
        throw new MirrorResetError();
    if (isRecordChange) isModelStale = true;
    if (isVisibleChange && m.isPersistent) postToOtherTabs({ type: "changed" });

    for (const id of new Set(changes.clearCollections)) {
        for (const fileID of [...(m.filesByCollection.get(id) ?? [])])
            deleteFile(m, fileID, id);
        m.filesByCollection.delete(id);
        for (const key of collectionMetaKeys(id)) m.meta.delete(key);
    }
    for (const id of changes.deleteCollections ?? []) m.collections.delete(id);
    for (const c of changes.putCollections ?? []) m.collections.set(c.id, c);
    for (const [id, cid] of changes.deleteFiles ?? []) deleteFile(m, id, cid);
    for (const f of changes.putFiles ?? []) putFile(m, f);
    for (const id of changes.deleteTrash ?? []) m.trash.delete(id);
    for (const t of changes.putTrash ?? []) m.trash.set(t.id, t);
    for (const id of changes.deletePendingTrash ?? [])
        m.pendingTrash.delete(id);
    for (const p of changes.putPendingTrash ?? [])
        m.pendingTrash.set(p.folderID, p);
    for (const [key, value] of Object.entries(changes.meta ?? {}))
        m.meta.set(key, value);
};

export const mirrorMeta = (key: string) => ensureMirror().meta.get(key);

export const mirrorCollections = () => [...ensureMirror().collections.values()];

export const mirrorCollection = (id: number) =>
    ensureMirror().collections.get(id);

export const mirrorFileRecords = (fileID: number) => [
    ...(ensureMirror().files.get(fileID)?.values() ?? []),
];

export const mirrorFileRecord = (fileID: number, collectionID: number) =>
    ensureMirror().files.get(fileID)?.get(collectionID);

export const mirrorTrashRecord = (fileID: number) =>
    ensureMirror().trash.get(fileID);

export const collectionKeyOf = (record: StoredCollection) =>
    decryptor.collectionKey(record);

export const fileKeyOf = async (record: StoredFile) => {
    const collection = mirrorCollection(record.collectionID);
    if (!collection)
        throw new Error(`Collection ${record.collectionID} is not known`);
    return decryptor.fileKey(record, await collectionKeyOf(collection));
};

// Deleted folders' files keep arriving in the trash for a while after the
// delete. Give up on those that never arrive (e.g. deleted forever).
const pendingTrashTimeoutMs = 30 * 60 * 1000;

const settlePendingTrash = (
    entry: StoredPendingTrash,
    model: DriveModel,
    trashIDs: ReadonlySet<number>,
): StoredPendingTrash | undefined => {
    if (Date.now() - entry.startedAt > pendingTrashTimeoutMs) return undefined;
    const deleted = new Set(entry.folderIDs);
    const fileIDs = entry.fileIDs.filter((id) => {
        if (trashIDs.has(id)) return false;
        const file = model.files.get(id);
        return !file || file.collectionIDs.every((cid) => deleted.has(cid));
    });
    const isFolderListed = entry.folderIDs.some((id) => model.folders.has(id));
    if (!fileIDs.length && !isFolderListed) return undefined;
    return fileIDs.length == entry.fileIDs.length
        ? entry
        : { ...entry, fileIDs };
};

const publishPendingTrash = async (m: Mirror, model: DriveModel) => {
    const trashIDs = new Set(model.trash.map((t) => t.file.id));
    const put: StoredPendingTrash[] = [];
    const remove: number[] = [];
    for (const entry of m.pendingTrash.values()) {
        const settled = settlePendingTrash(entry, model, trashIDs);
        if (!settled) remove.push(entry.folderID);
        else if (settled != entry) put.push(settled);
    }
    if (put.length || remove.length)
        await writeMirror({ putPendingTrash: put, deletePendingTrash: remove });
    const pending: PendingTrash[] = [...m.pendingTrash.values()];
    const current = driveState().pendingTrash;
    if (
        pending.length != current.length ||
        pending.some((p, i) => p !== current[i])
    )
        setPendingTrash(pending);
};

export const addPendingTrash = async (entry: StoredPendingTrash) => {
    const m = ensureMirror();
    await writeMirror({ putPendingTrash: [entry] });
    setPendingTrash([...m.pendingTrash.values()]);
};

const decryptAndPublish = async () => {
    const m = ensureMirror();
    isModelStale = false;
    const startedGeneration = generation;
    // A snapshot, so that writes made meanwhile aren't half included.
    const decrypted = await decryptState(
        decryptor,
        m.userID,
        [...m.collections.values()],
        [...m.files.values()].flatMap((rows) => [...rows.values()]),
        [...m.trash.values()],
    );
    if (startedGeneration != generation || mirror != m)
        throw new MirrorResetError();
    const model = builder.build(decrypted, m.userID);
    publishModel(model);
    await publishPendingTrash(m, model);
};

let publishing: Promise<void> | undefined;
let publishQueued: Promise<void> | undefined;
let publishHold: Promise<void> | undefined;

// Holds back publishes while `task` writes related changes, so that none
// shows them half applied.
export const withPublishesHeld = async <T>(task: () => Promise<T>) => {
    let release!: () => void;
    const hold = new Promise<void>((resolve) => (release = resolve));
    publishHold = hold;
    try {
        return await task();
    } finally {
        if (publishHold == hold) publishHold = undefined;
        release();
    }
};

// Decrypts what changed and publishes a new model to the store. Runs one at
// a time so that an older model never replaces a newer one.
export const publishMirror = ({ ifStale = false } = {}): Promise<void> => {
    if (ifStale && !isModelStale && !publishing) return Promise.resolve();
    if (publishHold) return publishHold.then(() => publishMirror({ ifStale }));
    if (!publishing) {
        publishing = decryptAndPublish().finally(
            () => (publishing = undefined),
        );
        return publishing;
    }
    publishQueued ??= publishing
        .catch(() => undefined)
        .then(() => {
            publishQueued = undefined;
            return publishMirror({ ifStale });
        });
    return publishQueued;
};

export const clearMirror = async () => {
    generation++;
    mirror = undefined;
    decryptor = new DriveDecryptor();
    builder = new ModelBuilder();
    resetDriveState();
    await clearDriveDBs(lastUserID);
    lastUserID = undefined;
};
