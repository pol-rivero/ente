import type { FileCategory } from "../file-types";
import type { PublicLink } from "../records";

export type { FileCategory } from "../file-types";
export type { PublicLink } from "../records";

export type DriveRole = "VIEWER" | "COLLABORATOR" | "ADMIN" | "OWNER";

export interface DriveUser {
    id: number;
    email?: string;
}

export interface Sharee extends DriveUser {
    role: DriveRole;
}

export interface Folder {
    id: number;
    name: string;
    // "uncategorized" is the collection behind My Drive's root.
    type: "folder" | "uncategorized";
    // Verified parent (null for top-level, shared-with-me, orphaned and
    // unverifiable folders).
    parentID: number | null;
    // The parent the server claims, even when it failed verification.
    claimedParentID: number | null;
    isOwned: boolean;
    owner: DriveUser;
    role: DriveRole;
    sharees: Sharee[];
    publicLinks: PublicLink[];
    isShared: boolean;
    // Epoch microseconds. Changes to the folder's files alone don't update
    // it.
    updationTime: number;
    starred: boolean;
    // The server-claimed parent didn't verify (client guide §5.1).
    integrityWarning: boolean;
    isRootCollection: boolean;
}

export interface DriveFile {
    id: number;
    name: string;
    // The original name from the encrypted metadata.
    title: string;
    // Every live collection the file is in. A file is in more than one when
    // it was contributed to a shared folder or linked by another client.
    collectionIDs: number[];
    // The collection that places the file in the tree: the owner's own
    // folder for files we own, otherwise the folder it was found in.
    folderID: number;
    size: number | undefined;
    mimeType: string | undefined;
    fileType: number | undefined;
    category: FileCategory;
    // Epoch microseconds.
    creationTime: number;
    modificationTime: number;
    updationTime: number;
    ownerID: number;
    isOwned: boolean;
    // Only files we own can be starred (owner-only private metadata).
    starred: boolean;
    hash: string | undefined;
    noThumb: boolean;
}

export interface DriveTrashItem {
    file: DriveFile;
    // Epoch microseconds.
    deleteBy: number;
    updatedAt: number;
}

export interface DriveModel {
    userID: number | undefined;
    folders: ReadonlyMap<number, Folder>;
    files: ReadonlyMap<number, DriveFile>;
    // Most recently trashed first.
    trash: readonly DriveTrashItem[];
    rootCollectionID: number | undefined;
}

export type ItemKind = "file" | "folder";

export interface DriveItemRef {
    kind: ItemKind;
    id: number;
}

export type DriveItem =
    | { kind: "folder"; folder: Folder }
    | { kind: "file"; file: DriveFile };

export const itemKey = (item: DriveItemRef | DriveItem) =>
    `${item.kind}:${"id" in item ? item.id : item.kind == "folder" ? item.folder.id : item.file.id}`;

export const itemRef = (item: DriveItem): DriveItemRef =>
    item.kind == "folder"
        ? { kind: "folder", id: item.folder.id }
        : { kind: "file", id: item.file.id };

export const toDate = (microseconds: number) =>
    new Date(Math.floor(microseconds / 1000));

export type SyncStatus = "idle" | "syncing" | "offline" | "error";

// unauthorized and unsupported_server stop syncing until the session changes.
export type SyncErrorReason = "unauthorized" | "unsupported_server" | "failed";

export interface DriveSyncState {
    status: SyncStatus;
    error?: SyncErrorReason;
    // Folders whose contents couldn't be synced in the last sync, which
    // otherwise succeeded. Their contents may be stale.
    failedFolderIDs: readonly number[];
    // Local epoch milliseconds.
    lastSyncedAt?: number;
    isHydrated: boolean;
    // False when the local database is unusable and the state lives only in
    // memory (it is fetched again on every start).
    isPersistent: boolean;
    // Folders whose contents were fetched during the current sync.
    progress?: { done: number; total: number };
}

export type PendingDriveOpKind =
    | "create_folder"
    | "rename"
    | "move"
    | "trash"
    | "restore"
    | "delete_forever"
    | "delete_folder"
    | "star"
    | "empty_trash";

// An operation that is running, or whose result isn't synced yet.
export interface PendingDriveOp {
    id: number;
    kind: PendingDriveOpKind;
    items: readonly DriveItemRef[];
    // null: My Drive's root.
    targetFolderID?: number | null;
    // Local epoch milliseconds.
    startedAt: number;
}

export interface PendingTrash {
    folderID: number;
    // The deleted folder and its subfolders.
    folderIDs: readonly number[];
    // Files of the deleted folders that haven't reached the trash yet.
    fileIDs: readonly number[];
    startedAt: number;
}

export interface DriveState {
    // Only replaced when the data changes, so it can key memoised selectors.
    model: DriveModel;
    sync: DriveSyncState;
    pendingOps: readonly PendingDriveOp[];
    pendingTrash: readonly PendingTrash[];
}
