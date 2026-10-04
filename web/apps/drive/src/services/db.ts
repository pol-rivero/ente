import log from "ente-base/log";
import { deleteDB, openDB, type DBSchema, type IDBPDatabase } from "idb";
import type {
    StoredCollection,
    StoredFile,
    StoredPendingTrash,
    StoredTrashItem,
} from "./records";

const dbNamePrefix = "ente-drive-";
const dbVersion = 1;

export const collectionsSinceTimeKey = "collectionsSinceTime";
export const trashSinceTimeKey = "trashSinceTime";
export const collectionSinceTimeKey = (collectionID: number) =>
    `collectionSinceTime:${collectionID}`;
// The collection updationTime that the last completed file diff caught up
// with. A collection's updationTime also changes without file changes (e.g.
// a rename), so it can stay ahead of the diff cursor.
export const collectionCheckedTimeKey = (collectionID: number) =>
    `collectionCheckedTime:${collectionID}`;

// A pending second fetch of rows from just before a cursor (see
// `overlapMicroseconds` in sync.ts): from which updationTime, and from when
// (local epoch ms). 0 when none is pending.
export interface RecheckKeys {
    since: string;
    at: string;
}

export const collectionsRecheckKeys: RecheckKeys = {
    since: "collectionsRecheckSince",
    at: "collectionsRecheckAt",
};
export const trashRecheckKeys: RecheckKeys = {
    since: "trashRecheckSince",
    at: "trashRecheckAt",
};
export const collectionRecheckKeys = (collectionID: number): RecheckKeys => ({
    since: `collectionRecheckSince:${collectionID}`,
    at: `collectionRecheckAt:${collectionID}`,
});

export const collectionMetaKeys = (collectionID: number) => {
    const recheck = collectionRecheckKeys(collectionID);
    return [
        collectionSinceTimeKey(collectionID),
        collectionCheckedTimeKey(collectionID),
        recheck.since,
        recheck.at,
    ];
};

interface DriveDBSchema extends DBSchema {
    collections: { key: number; value: StoredCollection };
    files: {
        key: [number, number];
        value: StoredFile;
        indexes: { collectionID: number };
    };
    trash: { key: number; value: StoredTrashItem };
    pendingTrash: { key: number; value: StoredPendingTrash };
    meta: { key: string; value: number };
}

const storeNames = [
    "collections",
    "files",
    "trash",
    "pendingTrash",
    "meta",
] as const;

export interface DriveDBSnapshot {
    collections: StoredCollection[];
    files: StoredFile[];
    trash: StoredTrashItem[];
    pendingTrash: StoredPendingTrash[];
    meta: Map<string, number>;
}

export interface DriveDBChanges {
    putCollections?: StoredCollection[];
    deleteCollections?: number[];
    // Removes every file row of these collections, and their sync times.
    clearCollections?: number[];
    putFiles?: StoredFile[];
    deleteFiles?: [number, number][];
    putTrash?: StoredTrashItem[];
    deleteTrash?: number[];
    putPendingTrash?: StoredPendingTrash[];
    deletePendingTrash?: number[];
    meta?: Record<string, number>;
}

let current:
    | { userID: number; db: Promise<IDBPDatabase<DriveDBSchema>> }
    | undefined;

const dbName = (userID: number) => `${dbNamePrefix}${userID}`;

const openDriveDB = (userID: number) => {
    const name = dbName(userID);
    return openDB<DriveDBSchema>(name, dbVersion, {
        upgrade(db, oldVersion) {
            log.info(`Upgrading ${name} from version ${oldVersion}`);
            for (const store of db.objectStoreNames)
                db.deleteObjectStore(store);
            db.createObjectStore("collections", { keyPath: "id" });
            db.createObjectStore("files", {
                keyPath: ["id", "collectionID"],
            }).createIndex("collectionID", "collectionID");
            db.createObjectStore("trash", { keyPath: "id" });
            db.createObjectStore("pendingTrash", { keyPath: "folderID" });
            db.createObjectStore("meta");
        },
        blocking() {
            log.info(`Closing ${name} for a newer version elsewhere`);
            void closeDriveDB();
        },
        terminated() {
            log.warn(`Connection to ${name} was terminated`);
            if (current?.userID == userID) current = undefined;
        },
    });
};

const driveDB = (userID: number) => {
    if (current?.userID != userID) {
        void closeDriveDB();
        const db = openDriveDB(userID);
        current = { userID, db };
        db.catch(() => {
            if (current?.db == db) current = undefined;
        });
    }
    return current.db;
};

const closeDriveDB = async () => {
    const db = current?.db;
    current = undefined;
    try {
        (await db)?.close();
    } catch (e) {
        log.warn("Ignoring error when closing the Drive DB", e);
    }
};

export const loadDriveDB = async (userID: number): Promise<DriveDBSnapshot> => {
    const db = await driveDB(userID);
    const tx = db.transaction(storeNames);
    const metaStore = tx.objectStore("meta");
    const [collections, files, trash, pendingTrash, metaKeys, metaValues] =
        await Promise.all([
            tx.objectStore("collections").getAll(),
            tx.objectStore("files").getAll(),
            tx.objectStore("trash").getAll(),
            tx.objectStore("pendingTrash").getAll(),
            metaStore.getAllKeys(),
            metaStore.getAll(),
        ]);
    await tx.done;
    const meta = new Map(metaKeys.map((key, i) => [key, metaValues[i]!]));
    return { collections, files, trash, pendingTrash, meta };
};

export const writeDriveDB = async (userID: number, changes: DriveDBChanges) => {
    const db = await driveDB(userID);
    const tx = db.transaction(storeNames, "readwrite");
    const collections = tx.objectStore("collections");
    const files = tx.objectStore("files");
    const trash = tx.objectStore("trash");
    const pendingTrash = tx.objectStore("pendingTrash");
    const meta = tx.objectStore("meta");
    const pending: Promise<unknown>[] = [];

    for (const id of new Set(changes.clearCollections)) {
        pending.push(
            (async () => {
                let cursor = await files
                    .index("collectionID")
                    .openCursor(IDBKeyRange.only(id));
                while (cursor) {
                    await cursor.delete();
                    cursor = await cursor.continue();
                }
            })(),
        );
        for (const key of collectionMetaKeys(id))
            pending.push(meta.delete(key));
    }
    for (const id of changes.deleteCollections ?? [])
        pending.push(collections.delete(id));
    for (const c of changes.putCollections ?? [])
        pending.push(collections.put(c));
    for (const key of changes.deleteFiles ?? [])
        pending.push(files.delete(key));
    for (const f of changes.putFiles ?? []) pending.push(files.put(f));
    for (const id of changes.deleteTrash ?? []) pending.push(trash.delete(id));
    for (const t of changes.putTrash ?? []) pending.push(trash.put(t));
    for (const id of changes.deletePendingTrash ?? [])
        pending.push(pendingTrash.delete(id));
    for (const p of changes.putPendingTrash ?? [])
        pending.push(pendingTrash.put(p));
    for (const [key, value] of Object.entries(changes.meta ?? {}))
        pending.push(meta.put(value, key));

    await Promise.all([...pending, tx.done]);
};

export const clearDriveDBs = async (userID: number | undefined) => {
    await closeDriveDB();
    const names = new Set<string>();
    if (userID !== undefined) names.add(dbName(userID));
    try {
        for (const { name } of await indexedDB.databases())
            if (name?.startsWith(dbNamePrefix)) names.add(name);
    } catch (e) {
        log.warn("Could not list IndexedDB databases", e);
    }
    for (const name of names) await deleteDB(name);
};
