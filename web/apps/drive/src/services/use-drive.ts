import { useEffect, useSyncExternalStore } from "react";
import { deletingFolderIDs, pendingItemKeys } from "./model/selectors";
import type { DriveModel, DriveState } from "./model/types";
import { driveState, subscribeDriveState } from "./store";
import { startDriveSync } from "./sync";

// Keeps the Drive state in sync with the server while `isSessionReady`.
// Use once, at the root of the signed-in UI.
export const useDriveSyncLifecycle = (isSessionReady: boolean) =>
    useEffect(
        () => (isSessionReady ? startDriveSync() : undefined),
        [isSessionReady],
    );

const useDriveState = <T>(select: (state: DriveState) => T) => {
    const snapshot = () => select(driveState());
    return useSyncExternalStore(subscribeDriveState, snapshot, snapshot);
};

export const useDriveModel = () => useDriveState((s) => s.model);

export const useDriveSyncState = () => useDriveState((s) => s.sync);

// `select` must return the same value for the same model, as the memoised
// selectors do (and plain property reads).
export const useDriveSelector = <T>(select: (model: DriveModel) => T) =>
    useDriveState((s) => select(s.model));

export const usePendingOps = () => useDriveState((s) => s.pendingOps);

export const usePendingTrash = () => useDriveState((s) => s.pendingTrash);

export const usePendingItemKeys = () => useDriveState(pendingItemKeys);

export const useDeletingFolderIDs = () => useDriveState(deletingFolderIDs);
