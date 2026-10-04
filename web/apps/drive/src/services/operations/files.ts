import log from "ente-base/log";
import { encryptJSONBlob, wrapKey } from "../crypto";
import {
    fileKeyOf,
    mirrorFileRecord,
    mirrorFileRecords,
    mirrorTrashRecord,
} from "../mirror";
import { decryptMagicMetadata, type JSONObject } from "../model/decrypt";
import { uniqueChildName } from "../model/selectors";
import type { DriveItemRef, PendingDriveOp } from "../model/types";
import {
    storedFile,
    type StoredFile,
    type StoredMagicMetadata,
} from "../records";
import {
    isDriveApiError,
    isTargetFolderGoneError,
    isVersionConflictError,
} from "../remote/errors";
import { driveRequest, driveRequestJSON } from "../remote/request";
import { FileResponse } from "../remote/schemas";
import { driveState } from "../store";
import { syncAfterOperation } from "../sync";
import {
    batches,
    driveErrorReason,
    DriveOperationError,
    folderKey,
    operationError,
    ownedFile,
    ownedFolder,
    parentForNames,
    reservedNames,
    reserveName,
    runOperation,
    validName,
    type BatchResult,
    type ItemFailure,
} from "./common";
import {
    ensureRootCollection,
    moveFolderIn,
    setFolderStarred,
} from "./folders";

const maxMagicMetadataAttempts = 4;

type MagicMetadataKind = "public" | "private";

const magicMetadataOf = (
    record: StoredFile | undefined,
    kind: MagicMetadataKind,
): StoredMagicMetadata | undefined =>
    kind == "public" ? record?.pubMagicMetadata : record?.magicMetadata;

// Any record of a file: the magic metadata is the same in all of them.
const anyFileRecord = (fileID: number) =>
    mirrorFileRecords(fileID)[0] ?? mirrorTrashRecord(fileID);

// The server's current record, from whichever collection holds the file now.
const latestFileRecord = async (fileID: number, collectionID: number) => {
    const fetch = (cid: number) =>
        driveRequestJSON("GET", "/collections/file", FileResponse, {
            query: { collectionID: cid, fileID },
        });
    try {
        return storedFile((await fetch(collectionID)).file);
    } catch (e) {
        if (!isDriveApiError(e, 404)) throw e;
        // Moved or trashed meanwhile.
        await syncAfterOperation();
        const record = mirrorFileRecords(fileID)[0];
        if (!record || record.collectionID == collectionID)
            throw new DriveOperationError("not_found", e);
        return storedFile((await fetch(record.collectionID)).file);
    }
};

// File magic metadata is versioned: the server answers 409 if `version` isn't
// the current one (or if too many keys are dropped). On 409, refetch the file,
// merge again and retry.
const updateFileMagicMetadata = async (
    fileID: number,
    kind: MagicMetadataKind,
    update: (current: JSONObject) => JSONObject,
) => {
    let record = anyFileRecord(fileID);
    if (!record) throw new DriveOperationError("not_found");
    const fileKey = await fileKeyOf(record);
    for (let attempt = 1; ; attempt++) {
        const mm = magicMetadataOf(record, kind);
        const next = update(await decryptMagicMetadata(mm, fileKey));
        const { encryptedData, decryptionHeader } = await encryptJSONBlob(
            next,
            fileKey,
        );
        try {
            await driveRequest(
                "PUT",
                kind == "public"
                    ? "/files/public-magic-metadata"
                    : "/files/magic-metadata",
                {
                    body: {
                        metadataList: [
                            {
                                id: fileID,
                                magicMetadata: {
                                    version: mm?.version ?? 1,
                                    count: Object.keys(next).length,
                                    data: encryptedData,
                                    header: decryptionHeader,
                                },
                            },
                        ],
                    },
                },
            );
            return;
        } catch (e) {
            if (!isVersionConflictError(e)) throw e;
            if (attempt >= maxMagicMetadataAttempts)
                throw new DriveOperationError("conflict", e);
        }
        const latest: StoredFile | undefined = await latestFileRecord(
            fileID,
            record.collectionID,
        );
        if (!latest) throw new DriveOperationError("not_found");
        record = latest;
    }
};

const fileRef = (id: number): DriveItemRef => ({ kind: "file", id });

// Returns the final name, which differs from `name` if that was taken.
export const renameFile = async (fileID: number, name: string) => {
    const desiredName = validName(name);
    return runOperation(
        { kind: "rename", items: [fileRef(fileID)] },
        async (op) => {
            const file = ownedFile(fileID);
            const parent = parentForNames(file.folderID);
            const finalName = uniqueChildName(
                driveState().model,
                parent,
                desiredName,
                "file",
                fileRef(fileID),
                reservedNames(parent),
            );
            if (finalName == file.name) return finalName;
            reserveName(op, parent, finalName);
            await updateFileMagicMetadata(fileID, "public", (current) => ({
                ...current,
                editedName: finalName,
            }));
            return finalName;
        },
    );
};

// Starred is private metadata (plan C15). Folders shared with us can be
// starred (in our sharee metadata), but files shared with us can't: their
// metadata belongs to their owner.
export const setStarred = (item: DriveItemRef, starred: boolean) =>
    runOperation({ kind: "star", items: [item] }, async () => {
        if (item.kind == "folder") {
            await setFolderStarred(item.id, starred);
            return;
        }
        ownedFile(item.id);
        await updateFileMagicMetadata(item.id, "private", (current) => ({
            ...current,
            starred,
        }));
    });

interface RewrappedFile {
    id: number;
    encryptedKey: string;
    keyDecryptionNonce: string;
}

export const rewrapFileKey = async (
    id: number,
    fileKey: string,
    targetKey: string,
): Promise<RewrappedFile> => {
    const { encryptedData, nonce } = await wrapKey(fileKey, targetKey);
    return { id, encryptedKey: encryptedData, keyDecryptionNonce: nonce };
};

export interface Arrival {
    fileID: number;
    fileKey: string;
    name: string;
}

// Names are encrypted, so the server can't keep them unique within a folder;
// files that would clash are renamed after they arrive. Best effort: a
// failure leaves the clash, and is only logged.
export const renameArrivals = async (
    op: PendingDriveOp,
    arrivals: Arrival[],
    targetID: number,
): Promise<BatchResult["renamed"]> => {
    const parent = parentForNames(targetID);
    const renames: (Arrival & { finalName: string })[] = [];
    for (const arrival of arrivals) {
        const finalName = uniqueChildName(
            driveState().model,
            parent,
            arrival.name,
            "file",
            fileRef(arrival.fileID),
            reservedNames(parent),
        );
        reserveName(op, parent, finalName);
        if (finalName != arrival.name) renames.push({ ...arrival, finalName });
    }
    const renamed: BatchResult["renamed"] = [];
    for (const { fileID, finalName } of renames) {
        try {
            await updateFileMagicMetadata(fileID, "public", (current) => ({
                ...current,
                editedName: finalName,
            }));
            renamed.push({ ref: fileRef(fileID), name: finalName });
        } catch (e) {
            log.error(`Failed to rename arrived file ${fileID}`, e);
        }
    }
    return renamed;
};

// Attributes a failure that a deleted folder may have caused, which the
// server reports the same way for either end of a move (client guide §6.2).
export const targetAwareError = async (e: unknown, targetID: number) => {
    if (!isTargetFolderGoneError(e) && !isDriveApiError(e, 403))
        return operationError(e);
    await syncAfterOperation();
    const target = driveState().model.folders.get(targetID);
    if (!target) return new DriveOperationError("folder_deleted", e);
    return isTargetFolderGoneError(e)
        ? new DriveOperationError("not_found", e)
        : operationError(e);
};

const moveErrorCodes = {
    FILE_IN_TRASH: "file_in_trash",
    COLLECTION_DELETED: "folder_deleted",
} as const;

const failure = (
    ref: DriveItemRef,
    e: unknown,
): ItemFailure & { error: unknown } => ({
    ref,
    reason: driveErrorReason(e),
    error: e,
});

// Moves files we own into `targetID`, a folder we own. A file leaves every
// folder of ours it is in, so it ends up in exactly one (C10). Files in
// folders shared with us (contributions) stay there.
const moveFilesIn = async (
    op: PendingDriveOp,
    fileIDs: readonly number[],
    targetID: number,
) => {
    const failed: (ItemFailure & { error: unknown })[] = [];
    const targetKey = await folderKey(targetID);
    const { folders } = driveState().model;
    const bySource = new Map<number, RewrappedFile[]>();
    const arrivals: Arrival[] = [];
    for (const fileID of new Set(fileIDs)) {
        try {
            const file = ownedFile(fileID);
            const sources = file.collectionIDs.filter(
                (id) => id != targetID && folders.get(id)?.isOwned,
            );
            if (!sources.length) continue;
            const record = mirrorFileRecord(fileID, sources[0]!);
            if (!record) throw new DriveOperationError("not_found");
            const fileKey = await fileKeyOf(record);
            const wrapped = await rewrapFileKey(fileID, fileKey, targetKey);
            for (const source of sources)
                bySource.set(source, [
                    ...(bySource.get(source) ?? []),
                    wrapped,
                ]);
            if (!file.collectionIDs.includes(targetID))
                arrivals.push({ fileID, fileKey, name: file.name });
        } catch (e) {
            failed.push(failure(fileRef(fileID), e));
        }
    }

    const moved = new Set<number>();
    const failedIDs = new Set<number>();
    for (const [fromCollectionID, items] of bySource) {
        for (const files of batches(items)) {
            try {
                await driveRequest("POST", "/collections/move-files", {
                    body: { fromCollectionID, toCollectionID: targetID, files },
                });
                for (const f of files) moved.add(f.id);
            } catch (e) {
                const error = isTargetFolderGoneError(e)
                    ? await targetAwareError(e, targetID)
                    : operationError(e, moveErrorCodes);
                for (const f of files)
                    if (!failedIDs.has(f.id)) {
                        failedIDs.add(f.id);
                        failed.push(failure(fileRef(f.id), error));
                    }
            }
        }
    }
    const renamed = await renameArrivals(
        op,
        arrivals.filter((a) => moved.has(a.fileID) && !failedIDs.has(a.fileID)),
        targetID,
    );
    return {
        moved: [...moved].filter((id) => !failedIDs.has(id)),
        renamed,
        failed,
    };
};

// Moves files and folders into `toFolderID` (null: My Drive's root), with
// one sync after. Items whose names are taken there are renamed. Rejects
// only when nothing could be moved.
export const moveItems = (
    items: readonly DriveItemRef[],
    toFolderID: number | null,
): Promise<BatchResult> =>
    runOperation(
        { kind: "move", items, targetFolderID: toFolderID },
        async (op) => {
            const targetID = toFolderID ?? (await ensureRootCollection());
            ownedFolder(targetID);
            const renamed: BatchResult["renamed"] = [];
            const failed: (ItemFailure & { error: unknown })[] = [];
            let movedCount = 0;
            for (const item of items.filter((i) => i.kind == "folder")) {
                try {
                    const name = await moveFolderIn(
                        op,
                        item.id,
                        parentForNames(targetID),
                    );
                    movedCount++;
                    if (name !== undefined) renamed.push({ ref: item, name });
                } catch (e) {
                    failed.push(failure(item, operationError(e)));
                }
            }
            const fileIDs = items
                .filter((i) => i.kind == "file")
                .map((i) => i.id);
            if (fileIDs.length) {
                const result = await moveFilesIn(op, fileIDs, targetID);
                movedCount += result.moved.length;
                renamed.push(...result.renamed);
                failed.push(...result.failed);
            }
            if (!movedCount && failed.length) throw failed[0]!.error;
            return {
                renamed,
                failed: failed.map(({ ref, reason }) => ({ ref, reason })),
            };
        },
    );
