// The data layer's API for the UI and the transfer layer: the UI imports only
// from here.

export { itemKey, itemRef, toDate } from "./model/types";
export type {
    DriveFile,
    DriveItem,
    DriveItemRef,
    DriveModel,
    DriveRole,
    DriveState,
    DriveSyncState,
    DriveTrashItem,
    DriveUser,
    FileCategory,
    Folder,
    ItemKind,
    PendingDriveOp,
    PendingDriveOpKind,
    PendingTrash,
    PublicLink,
    Sharee,
    SyncErrorReason,
    SyncStatus,
} from "./model/types";

export { driveState, subscribeDriveState } from "./store";
export { syncDrive } from "./sync";
export {
    useDeletingFolderIDs,
    useDriveModel,
    useDriveSelector,
    useDriveSyncLifecycle,
    useDriveSyncState,
    usePendingItemKeys,
    usePendingOps,
    usePendingTrash,
} from "./use-drive";

export {
    breadcrumbs,
    canWriteFolder,
    contentsOf,
    deletingFolderIDs,
    folderStats,
    isDescendantOrSelf,
    navigationParent,
    pendingItemKeys,
    recentFiles,
    searchDrive,
    sharedWithMe,
    sortDriveItems,
    starredItems,
    trashEntries,
    userEmail,
} from "./model/selectors";
export type {
    Breadcrumb,
    FolderContents,
    FolderStats,
    SearchOptions,
    SearchScope,
    SortKey,
    SortOrder,
} from "./model/selectors";
export { normalizeItemName } from "./names";

export {
    DriveOperationError,
    driveErrorReason,
    isDriveOperationError,
} from "./operations/common";
export type {
    BatchResult,
    DriveErrorReason,
    DriveOperationErrorReason,
    ItemFailure,
} from "./operations/common";
export { moveItems, renameFile, setStarred } from "./operations/files";
export {
    createFolder,
    moveFolder,
    renameFolder,
    repairFolderParent,
} from "./operations/folders";
export {
    deleteForever,
    emptyTrash,
    restoreFiles,
    trashItems,
} from "./operations/trash";
export type { TrashResult } from "./operations/trash";

export { fileKeyMaterial, uploadTarget } from "./file-access";
export type { FileKeyMaterial, UploadTarget } from "./file-access";
export { ingestRemoteCollection, ingestRemoteFiles } from "./ingest";
