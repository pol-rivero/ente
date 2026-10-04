import log from "ente-base/log";
import { generateKey } from "ente-drive-wasm";
import { PromiseQueue, wait } from "ente-utils/promise";
import {
    encryptJSONBlob,
    encryptText,
    ensureMasterKey,
    wrapKey,
} from "../crypto";
import { ingestRemoteCollection } from "../ingest";
import { addPendingTrash, mirrorCollection } from "../mirror";
import { decryptMagicMetadata } from "../model/decrypt";
import {
    folderPath,
    isDescendantOrSelf,
    subtreeIDs,
    uniqueChildName,
} from "../model/selectors";
import type { DriveItemRef, Folder, PendingDriveOp } from "../model/types";
import { storedCollection, type StoredMagicMetadata } from "../records";
import { isDriveApiError, isNetworkError } from "../remote/errors";
import { driveRequest, driveRequestJSON } from "../remote/request";
import {
    CollectionResponse,
    CollectionsResponse,
    parseItems,
    RemoteCollection,
} from "../remote/schemas";
import { driveState } from "../store";
import { syncAfterOperation } from "../sync";
import {
    beginWrite,
    DriveOperationError,
    folderKey,
    isFolderDeleting,
    operationError,
    ownedFolder,
    parentForNames,
    reservedNames,
    reserveName,
    retryWhenBusy,
    runOperation,
    validName,
    type DriveOperationErrorReason,
} from "./common";

const moveErrorCodes = {
    NOT_FOUND: "not_found",
    COLLECTION_DELETED: "folder_deleted",
    INVALID_COLLECTION: "invalid_target",
    INVALID_PARENT: "invalid_target",
    COLLECTION_CYCLE: "cycle",
    MAX_DEPTH_EXCEEDED: "max_depth",
} as const;

const createCollection = async (
    type: string,
    name: string,
    parent?: { id: number; key: string },
) => {
    const key = await generateKey();
    const { encryptedData, nonce } = await wrapKey(
        key,
        await ensureMasterKey(),
    );
    const encryptedName = await encryptText(name, key);
    const parentWrap = parent && (await wrapKey(key, parent.key));
    const { collection } = await retryWhenBusy(() =>
        driveRequestJSON("POST", "/collections", CollectionResponse, {
            body: {
                type,
                encryptedKey: encryptedData,
                keyDecryptionNonce: nonce,
                encryptedName: encryptedName.encryptedData,
                nameDecryptionNonce: encryptedName.nonce,
                ...(parent &&
                    parentWrap && {
                        parentID: parent.id,
                        parentEncryptedKey: parentWrap.encryptedData,
                        parentKeyNonce: parentWrap.nonce,
                    }),
            },
        }),
    ).catch((e: unknown) => {
        throw operationError(e, {
            INVALID_PARENT: "invalid_target",
            MAX_DEPTH_EXCEEDED: "max_depth",
        });
    });
    return { collection, key };
};

let rootCreation: Promise<number> | undefined;

// The Drive `uncategorized` collection holds My Drive's root-level files. It
// is created on first use; the server returns the existing one if another
// device created it meanwhile.
export const ensureRootCollection = async (): Promise<number> => {
    const existing = driveState().model.rootCollectionID;
    if (existing !== undefined) return existing;
    rootCreation ??= (async () => {
        await beginWrite();
        const { collection } = await createCollection(
            "uncategorized",
            "Uncategorized",
        );
        await ingestRemoteCollection(collection);
        return collection.id;
    })().finally(() => (rootCreation = undefined));
    return rootCreation;
};

const writableParent = (parentID: number | null) => {
    const id = parentForNames(parentID);
    // Subfolders can only be created in folders we own (D6).
    if (id !== null) ownedFolder(id);
    return id;
};

const moveCollection = async (
    folderID: number,
    newParentID: number | null,
    folderKeyB64?: string,
) => {
    const parentWrap =
        newParentID === null
            ? undefined
            : await wrapKey(
                  folderKeyB64 ?? (await folderKey(folderID)),
                  await folderKey(newParentID),
              );
    await retryWhenBusy(() =>
        driveRequest("POST", "/collections/move-collection", {
            body: {
                collectionID: folderID,
                newParentID,
                ...(parentWrap && {
                    parentEncryptedKey: parentWrap.encryptedData,
                    parentKeyNonce: parentWrap.nonce,
                }),
            },
        }),
    ).catch((e: unknown) => {
        throw operationError(e, moveErrorCodes);
    });
};

const renameCollection = async (folderID: number, name: string) => {
    const encryptedName = await encryptText(name, await folderKey(folderID));
    await driveRequest("POST", "/collections/rename", {
        body: {
            collectionID: folderID,
            encryptedName: encryptedName.encryptedData,
            nameDecryptionNonce: encryptedName.nonce,
        },
    }).catch((e: unknown) => {
        throw operationError(e, { NOT_FOUND: "not_found" });
    });
};

const folderRef = (id: number): DriveItemRef => ({ kind: "folder", id });

// After a create whose response was lost: the folder that appeared under
// `parentID` with `name`, if the request did reach the server.
const findCreatedFolder = (
    parentID: number | null,
    name: string,
    knownIDs: ReadonlySet<number>,
) =>
    [...driveState().model.folders.values()].find(
        (f) =>
            !knownIDs.has(f.id) &&
            f.isOwned &&
            !f.isRootCollection &&
            f.parentID === parentID &&
            f.name == name,
    );

export const createFolder = async (
    parentID: number | null,
    name: string,
): Promise<number> => {
    const desiredName = validName(name);
    return runOperation(
        { kind: "create_folder", items: [], targetFolderID: parentID },
        async (op) => {
            const parent = writableParent(parentID);
            const model = driveState().model;
            const knownIDs = new Set(model.folders.keys());
            const finalName = uniqueChildName(
                model,
                parent,
                desiredName,
                "folder",
                undefined,
                reservedNames(parent),
            );
            reserveName(op, parent, finalName);
            let created;
            try {
                created = await createCollection(
                    "folder",
                    finalName,
                    parent === null
                        ? undefined
                        : { id: parent, key: await folderKey(parent) },
                );
            } catch (e) {
                if (!isDriveOperationErrorOffline(e)) throw e;
                await syncAfterOperation();
                const found = findCreatedFolder(parent, finalName, knownIDs);
                if (found) return found.id;
                throw e;
            }
            const { collection, key } = created;
            await ingestRemoteCollection(collection);
            // Servers without nested folders drop the parent fields and
            // create a top-level folder (client guide §5.1).
            if (parent !== null && collection.parentID != parent) {
                try {
                    await moveCollection(collection.id, parent, key);
                } catch (e) {
                    const error = operationError(e);
                    if (error instanceof DriveOperationError)
                        error.createdFolderID = collection.id;
                    throw error;
                }
            }
            return collection.id;
        },
    );
};

const isDriveOperationErrorOffline = (e: unknown) =>
    e instanceof DriveOperationError && e.reason == "offline";

const editableFolder = (folderID: number) => {
    const folder = ownedFolder(folderID);
    if (folder.isRootCollection)
        throw new DriveOperationError("invalid_target");
    return folder;
};

export const renameFolder = async (folderID: number, name: string) => {
    const desiredName = validName(name);
    return runOperation(
        { kind: "rename", items: [folderRef(folderID)] },
        async (op) => {
            const folder = editableFolder(folderID);
            const finalName = uniqueChildName(
                driveState().model,
                folder.parentID,
                desiredName,
                "folder",
                folderRef(folderID),
                reservedNames(folder.parentID),
            );
            if (finalName == folder.name) return finalName;
            reserveName(op, folder.parentID, finalName);
            await renameCollection(folderID, finalName);
            return finalName;
        },
    );
};

// Moves `folder` under `newParentID` (null: My Drive's root), renaming it
// if its name is taken there. Returns the new name if it was renamed.
const moveFolderTo = async (
    op: PendingDriveOp,
    folder: Folder,
    newParentID: number | null,
): Promise<string | undefined> => {
    const finalName = uniqueChildName(
        driveState().model,
        newParentID,
        folder.name,
        "folder",
        folderRef(folder.id),
        reservedNames(newParentID),
    );
    reserveName(op, newParentID, finalName);
    await moveCollection(folder.id, newParentID);
    if (finalName == folder.name) return undefined;
    try {
        await renameCollection(folder.id, finalName);
        return finalName;
    } catch (e) {
        // The move happened; only the name clash remains.
        log.error(`Failed to rename moved folder ${folder.id}`, e);
        return undefined;
    }
};

// Validates and moves one folder as part of operation `op`.
export const moveFolderIn = async (
    op: PendingDriveOp,
    folderID: number,
    newParentID: number | null,
) => {
    const folder = editableFolder(folderID);
    const parent = writableParent(newParentID);
    if (
        parent !== null &&
        isDescendantOrSelf(driveState().model, parent, folderID)
    )
        throw new DriveOperationError("cycle");
    if (folder.parentID === parent && !folder.integrityWarning)
        return undefined;
    return moveFolderTo(op, folder, parent);
};

// Returns the folder's new name if it had to be renamed.
export const moveFolder = (folderID: number, newParentID: number | null) =>
    runOperation(
        {
            kind: "move",
            items: [folderRef(folderID)],
            targetFolderID: newParentID,
        },
        (op) => moveFolderIn(op, folderID, newParentID),
    );

// Re-wraps the folder key under the parent the server claims, accepting
// that placement for a folder flagged with an integrity warning. Without a
// usable claimed parent the folder is moved to the root.
export const repairFolderParent = (folderID: number) =>
    runOperation({ kind: "move", items: [folderRef(folderID)] }, async (op) => {
        const folder = editableFolder(folderID);
        const model = driveState().model;
        const claimed =
            folder.claimedParentID === null
                ? undefined
                : model.folders.get(folder.claimedParentID);
        const parent =
            claimed?.isOwned &&
            !isFolderDeleting(claimed.id) &&
            !claimed.isRootCollection &&
            !folderPath(model, claimed.id).some((f) => f.id == folderID)
                ? claimed.id
                : null;
        return moveFolderTo(op, folder, parent);
    });

interface DeleteFolderOptions {
    // Also delete every subfolder (otherwise the folder must have none).
    recursive: boolean;
    // Fail instead of trashing files (otherwise files are trashed, or just
    // unlinked when they also live elsewhere).
    keepFiles: boolean;
}

const deleteErrorCodes: Record<string, DriveOperationErrorReason> = {
    HAS_CHILDREN: "has_children",
    COLLECTION_NOT_EMPTY: "not_empty",
    SUBTREE_TOO_LARGE: "subtree_too_large",
    INVALID_COLLECTION: "invalid_target",
};

// Recursive deletes of big trees can take tens of seconds on the server.
const deleteTimeoutMs = 3 * 60 * 1000;
const deleteNetworkRetries = 2;

// Files that the delete will move to the trash: ours, and in no folder of
// ours outside the deleted ones.
const filesToTrash = (folderIDs: ReadonlySet<number>) => {
    const { files, folders } = driveState().model;
    const ids: number[] = [];
    for (const file of files.values()) {
        if (!file.isOwned) continue;
        const owned = file.collectionIDs.filter(
            (id) => folders.get(id)?.isOwned,
        );
        if (
            owned.some((id) => folderIDs.has(id)) &&
            owned.every((id) => folderIDs.has(id))
        )
            ids.push(file.id);
    }
    return ids;
};

const isFolderListed = (folderID: number) =>
    driveState().model.folders.has(folderID);

// Throws a DriveOperationError (has_children, not_empty, subtree_too_large,
// ...) when the folder wasn't deleted. A folder that's already gone counts
// as deleted.
const deleteFolderNow = async (
    folderID: number,
    { recursive, keepFiles }: DeleteFolderOptions,
) => {
    if (!isFolderListed(folderID)) {
        await syncAfterOperation();
        if (!isFolderListed(folderID)) return;
    }
    const folder = driveState().model.folders.get(folderID)!;
    if (!folder.isOwned) throw new DriveOperationError("not_owner");
    if (folder.isRootCollection)
        throw new DriveOperationError("invalid_target");
    const folderIDs = recursive
        ? subtreeIDs(driveState().model, folderID)
        : [folderID];
    const fileIDs = filesToTrash(new Set(folderIDs));
    for (let attempt = 0; ; attempt++) {
        try {
            await retryWhenBusy(() =>
                driveRequest("DELETE", `/collections/v4/${folderID}`, {
                    query: { keepFiles, recursive },
                    timeoutMs: deleteTimeoutMs,
                }),
            );
            break;
        } catch (e) {
            if (isDriveApiError(e, 404, "NOT_FOUND")) break;
            if (!isNetworkError(e)) throw operationError(e, deleteErrorCodes);
            // The delete is idempotent, and its outcome unknown.
            if (attempt < deleteNetworkRetries) {
                await wait(2000 * (attempt + 1));
                continue;
            }
            await syncAfterOperation();
            if (isFolderListed(folderID)) throw operationError(e);
            break;
        }
    }
    await addPendingTrash({
        folderID,
        folderIDs,
        fileIDs,
        startedAt: Date.now(),
    });
};

// One folder delete at a time per account (client guide §5.1).
const deleteQueue = new PromiseQueue<void>();

// Deletes a folder and its subfolders, trashing their files. The folders
// show as deleting from the moment the delete is queued.
export const deleteFolderRecursively = (folderID: number) =>
    runOperation(
        {
            kind: "delete_folder",
            items: (isFolderListed(folderID)
                ? subtreeIDs(driveState().model, folderID)
                : [folderID]
            ).map(folderRef),
        },
        () =>
            deleteQueue.add(() =>
                deleteFolderNow(folderID, {
                    recursive: true,
                    keepFiles: false,
                }),
            ),
    );

// Sends the current version, like Photos (the server doesn't check it).
const putCollectionMagicMetadata = async (
    path: "/collections/magic-metadata" | "/collections/sharee-magic-metadata",
    folderID: number,
    current: StoredMagicMetadata | undefined,
    update: (value: Record<string, unknown>) => Record<string, unknown>,
) => {
    const key = await folderKey(folderID);
    const next = update(await decryptMagicMetadata(current, key));
    const { encryptedData, decryptionHeader } = await encryptJSONBlob(
        next,
        key,
    );
    await driveRequest("PUT", path, {
        body: {
            id: folderID,
            magicMetadata: {
                version: current?.version ?? 1,
                count: Object.keys(next).length,
                data: encryptedData,
                header: decryptionHeader,
            },
        },
    });
};

// Collection magic metadata has no version check: last write wins. So the
// merge starts from the server's current value, not from the local copy,
// which another device may have changed since.
const latestOwnMagicMetadata = async (folderID: number) => {
    const { collection } = await driveRequestJSON(
        "GET",
        `/collections/${folderID}`,
        CollectionResponse,
    );
    return storedCollection(collection, undefined).magicMetadata;
};

// GET /collections/:id doesn't return a sharee's magic metadata, but the
// collections diff does, starting from our copy of the collection.
const latestShareeMagicMetadata = async (folderID: number) => {
    const record = mirrorCollection(folderID);
    if (!record) throw new DriveOperationError("not_found");
    const { collections } = await driveRequestJSON(
        "GET",
        "/collections/v2",
        CollectionsResponse,
        { query: { sinceTime: Math.max(0, record.updationTime - 1) } },
    );
    const latest = parseItems(RemoteCollection, collections, "collection").find(
        (c) => c.id == folderID,
    );
    if (!latest || latest.isDeleted) throw new DriveOperationError("not_found");
    return storedCollection(latest, undefined).shareeMagicMetadata;
};

// Folders we own keep `starred` in their private magic metadata; folders
// shared with us in our own sharee magic metadata.
export const setFolderStarred = async (folderID: number, starred: boolean) => {
    const folder = driveState().model.folders.get(folderID);
    if (!folder) throw new DriveOperationError("not_found");
    const star = (value: Record<string, unknown>) => ({ ...value, starred });
    if (folder.isOwned) {
        const current = await latestOwnMagicMetadata(folderID);
        await putCollectionMagicMetadata(
            "/collections/magic-metadata",
            folderID,
            current,
            star,
        );
    } else {
        const current = await latestShareeMagicMetadata(folderID);
        await putCollectionMagicMetadata(
            "/collections/sharee-magic-metadata",
            folderID,
            current,
            star,
        );
    }
};
