import {
    fileKeyOf,
    mirrorFileRecord,
    mirrorFileRecords,
    mirrorTrashRecord,
} from "./mirror";
import {
    beginWrite,
    DriveOperationError,
    folderKey,
    ownedFolder,
} from "./operations/common";
import { ensureRootCollection } from "./operations/folders";
import { driveState } from "./store";

export interface FileKeyMaterial {
    fileID: number;
    collectionID: number;
    fileKey: string;
    fileDecryptionHeader: string;
    thumbnailDecryptionHeader: string | undefined;
    encryptedSize: number | undefined;
}

// What the transfer layer needs to download a file (live or trashed). Keys
// are decrypted on demand and never kept in the model.
export const fileKeyMaterial = async (
    fileID: number,
): Promise<FileKeyMaterial> => {
    const file = driveState().model.files.get(fileID);
    const record =
        (file && mirrorFileRecord(fileID, file.folderID)) ??
        mirrorFileRecords(fileID)[0] ??
        mirrorTrashRecord(fileID);
    if (!record) throw new DriveOperationError("not_found");
    return {
        fileID,
        collectionID: record.collectionID,
        fileKey: await fileKeyOf(record),
        fileDecryptionHeader: record.fileDecryptionHeader,
        thumbnailDecryptionHeader: record.thumbnailDecryptionHeader,
        encryptedSize: record.encryptedSize,
    };
};

export interface UploadTarget {
    collectionID: number;
    collectionKey: string;
}

// The collection an upload into `folderID` (null: My Drive's root) commits
// to. Uploads always go to a folder we own (client guide §6.1). Like every
// write, it first confirms that the server supports Drive.
export const uploadTarget = async (
    folderID: number | null,
): Promise<UploadTarget> => {
    await beginWrite();
    const collectionID = folderID ?? (await ensureRootCollection());
    ownedFolder(collectionID);
    return { collectionID, collectionKey: await folderKey(collectionID) };
};
