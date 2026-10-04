import { fileKeyOf, mirrorFileRecords, mirrorTrashRecord } from "../mirror";
import { trashLastUpdatedAt } from "../model/selectors";
import type { DriveItemRef } from "../model/types";
import { isDriveApiError } from "../remote/errors";
import { driveRequest } from "../remote/request";
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
    runOperation,
    type BatchResult,
    type DriveErrorReason,
    type ItemFailure,
} from "./common";
import {
    renameArrivals,
    rewrapFileKey,
    targetAwareError,
    type Arrival,
} from "./files";
import { deleteFolderRecursively, ensureRootCollection } from "./folders";

const fileRef = (id: number): DriveItemRef => ({ kind: "file", id });

// Trashing removes a file from every folder it is in.
const trashFiles = (fileIDs: readonly number[]) =>
    runOperation({ kind: "trash", items: fileIDs.map(fileRef) }, async () => {
        const trashed: number[] = [];
        const failed: ItemFailure[] = [];
        const items: { fileID: number; collectionID: number }[] = [];
        for (const fileID of fileIDs) {
            try {
                items.push({
                    fileID,
                    collectionID: ownedFile(fileID).folderID,
                });
            } catch (e) {
                failed.push({
                    ref: fileRef(fileID),
                    reason: driveErrorReason(e),
                });
            }
        }
        for (const batch of batches(items)) {
            try {
                await driveRequest("POST", "/files/trash", {
                    body: { items: batch },
                });
                trashed.push(...batch.map((i) => i.fileID));
            } catch (e) {
                const reason = driveErrorReason(operationError(e));
                for (const { fileID } of batch)
                    failed.push({ ref: fileRef(fileID), reason });
            }
        }
        return { trashed, failed };
    });

export interface TrashResult {
    trashedFileIDs: number[];
    deletedFolderIDs: number[];
    failed: { ref: DriveItemRef; reason: DriveErrorReason }[];
}

// Trashes files and deletes folders (with their subfolders, trashing their
// files). Per-item failures are reported in the result, not thrown.
export const trashItems = async (
    items: readonly DriveItemRef[],
): Promise<TrashResult> => {
    const result: TrashResult = {
        trashedFileIDs: [],
        deletedFolderIDs: [],
        failed: [],
    };
    const fileIDs = [
        ...new Set(items.filter((i) => i.kind == "file").map((i) => i.id)),
    ];
    const folderIDs = [
        ...new Set(items.filter((i) => i.kind == "folder").map((i) => i.id)),
    ];
    const trashing = fileIDs.length
        ? trashFiles(fileIDs).then(
              ({ trashed, failed }) => {
                  result.trashedFileIDs.push(...trashed);
                  result.failed.push(...failed);
              },
              (e: unknown) => {
                  const reason = driveErrorReason(e);
                  for (const id of fileIDs)
                      result.failed.push({ ref: fileRef(id), reason });
              },
          )
        : Promise.resolve();
    const deleting = folderIDs.map((id) =>
        deleteFolderRecursively(id).then(
            () => void result.deletedFolderIDs.push(id),
            (e: unknown) =>
                void result.failed.push({
                    ref: { kind: "folder", id },
                    reason: driveErrorReason(e),
                }),
        ),
    );
    await Promise.all([trashing, ...deleting]);
    return result;
};

// Restores trashed files into a folder we own (null: My Drive's root), since
// their original folders may be gone. Works right after the files were
// trashed (before a sync lists them in the trash). Rejects only when nothing
// could be restored.
export const restoreFiles = (
    fileIDs: readonly number[],
    targetFolderID: number | null,
): Promise<BatchResult> =>
    runOperation(
        { kind: "restore", items: fileIDs.map(fileRef), targetFolderID },
        async (op) => {
            const targetID = targetFolderID ?? (await ensureRootCollection());
            ownedFolder(targetID);
            const targetKey = await folderKey(targetID);
            const { model } = driveState();
            const names = new Map(
                model.trash.map((t) => [t.file.id, t.file.name]),
            );

            const failed: (ItemFailure & { error: unknown })[] = [];
            const fail = (id: number, error: unknown) =>
                failed.push({
                    ref: fileRef(id),
                    reason: driveErrorReason(error),
                    error,
                });
            const files = new Map<
                number,
                Awaited<ReturnType<typeof rewrapFileKey>>
            >();
            const arrivals = new Map<number, Arrival>();
            for (const fileID of new Set(fileIDs)) {
                const record =
                    mirrorTrashRecord(fileID) ?? mirrorFileRecords(fileID)[0];
                const name = names.get(fileID) ?? model.files.get(fileID)?.name;
                if (!record || name === undefined) {
                    fail(fileID, new DriveOperationError("not_found"));
                    continue;
                }
                try {
                    const fileKey = await fileKeyOf(record);
                    files.set(
                        fileID,
                        await rewrapFileKey(fileID, fileKey, targetKey),
                    );
                    arrivals.set(fileID, { fileID, fileKey, name });
                } catch (e) {
                    fail(fileID, e);
                }
            }

            const restored: number[] = [];
            const restore = (batch: { id: number }[]) =>
                driveRequest("POST", "/collections/restore-files", {
                    body: { collectionID: targetID, files: batch },
                });
            for (const batch of batches([...files.values()])) {
                try {
                    await restore(batch);
                    restored.push(...batch.map((f) => f.id));
                    continue;
                } catch (e) {
                    // One file no longer in the trash fails the whole batch
                    // with a plain 400.
                    if (!isDriveApiError(e, 400) || e.body != "empty") {
                        const error = await targetAwareError(e, targetID);
                        for (const f of batch) fail(f.id, error);
                        continue;
                    }
                }
                await syncAfterOperation();
                const stillTrashed = batch.filter((f) =>
                    mirrorTrashRecord(f.id),
                );
                for (const f of batch)
                    if (!stillTrashed.includes(f))
                        fail(f.id, new DriveOperationError("not_found"));
                if (!stillTrashed.length) continue;
                try {
                    await restore(stillTrashed);
                    restored.push(...stillTrashed.map((f) => f.id));
                } catch (e) {
                    const error = await targetAwareError(e, targetID);
                    for (const f of stillTrashed) fail(f.id, error);
                }
            }

            const renamed = await renameArrivals(
                op,
                restored.map((id) => arrivals.get(id)!),
                targetID,
            );
            if (!restored.length && failed.length) throw failed[0]!.error;
            return {
                renamed,
                failed: failed.map(({ ref, reason }) => ({ ref, reason })),
            };
        },
    );

export const deleteForever = (fileIDs: readonly number[]) =>
    runOperation(
        { kind: "delete_forever", items: fileIDs.map(fileRef) },
        async () => {
            for (const batch of batches([...new Set(fileIDs)]))
                await driveRequest("POST", "/trash/delete", {
                    body: { fileIDs: batch },
                });
        },
    );

// Empties only what this device has seen in the trash: files that reach the
// trash later (e.g. from a folder delete still in progress) are kept.
export const emptyTrash = () =>
    runOperation({ kind: "empty_trash", items: [] }, async () => {
        const lastUpdatedAt = trashLastUpdatedAt(driveState().model);
        if (!lastUpdatedAt) return;
        await driveRequest("POST", "/trash/empty", { body: { lastUpdatedAt } });
    });
