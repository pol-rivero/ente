import log from "ente-base/log";
import { encryptJSONBox } from "./crypto";
import { collectionKeyOf, publishMirror, writeMirror } from "./mirror";
import type { DriveFile, Folder } from "./model/types";
import {
    collectionDetails,
    storedCollection,
    storedFile,
    type StoredFile,
} from "./records";
import { parseItems, RemoteCollection, RemoteFile } from "./remote/schemas";
import { driveState } from "./store";

// Adds collections and files that a write returned (e.g. a created folder,
// or files an upload committed) to the local state right away,
// without waiting for the next sync. Sync cursors don't move, so the next
// sync still fetches them, and finds them unchanged.

export const ingestRemoteCollection = async (
    collection: unknown,
): Promise<Folder | undefined> => {
    const [c] = parseItems(RemoteCollection, [collection], "collection");
    if (!c || c.isDeleted) return undefined;
    const record = storedCollection(c, undefined);
    try {
        record.details = await encryptJSONBox(
            collectionDetails(c),
            await collectionKeyOf(record),
        );
    } catch (e) {
        log.error(`Failed to open the key of collection ${c.id}`, e);
        return undefined;
    }
    await writeMirror({ putCollections: [record] });
    await publishMirror({ ifStale: true });
    return driveState().model.folders.get(c.id);
};

// Accepts files as museum returns them (e.g. from `POST /files`).
export const ingestRemoteFiles = async (
    files: readonly unknown[],
): Promise<DriveFile[]> => {
    const records = parseItems(RemoteFile, files, "file")
        .filter((f) => !f.isDeleted)
        .map(storedFile)
        .filter((f): f is StoredFile => !!f);
    if (!records.length) return [];
    await writeMirror({ putFiles: records });
    await publishMirror({ ifStale: true });
    const model = driveState().model;
    return [...new Set(records.map((r) => r.id))]
        .map((id) => model.files.get(id))
        .filter((f): f is DriveFile => !!f);
};
