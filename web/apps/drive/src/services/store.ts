import type {
    DriveItemRef,
    DriveModel,
    DriveState,
    DriveSyncState,
    PendingDriveOp,
    PendingDriveOpKind,
    PendingTrash,
} from "./model/types";

const emptyModel = (): DriveModel => ({
    userID: undefined,
    folders: new Map(),
    files: new Map(),
    trash: [],
    rootCollectionID: undefined,
});

const initialState = (): DriveState => ({
    model: emptyModel(),
    sync: {
        status: "idle",
        failedFolderIDs: [],
        isHydrated: false,
        isPersistent: true,
    },
    pendingOps: [],
    pendingTrash: [],
});

let state = initialState();
const listeners = new Set<() => void>();

export const driveState = () => state;

export const subscribeDriveState = (listener: () => void) => {
    listeners.add(listener);
    return () => {
        listeners.delete(listener);
    };
};

// Subscribers are only notified when a part of the state is replaced.
const setDriveState = (next: Partial<DriveState>) => {
    const keys = Object.keys(next) as (keyof DriveState)[];
    if (keys.every((k) => next[k] === state[k])) return;
    state = { ...state, ...next };
    for (const listener of listeners) listener();
};

export const resetDriveState = () => {
    state = initialState();
    for (const listener of listeners) listener();
};

export const publishModel = (model: DriveModel) => setDriveState({ model });

const sameProgress = (
    a: DriveSyncState["progress"],
    b: DriveSyncState["progress"],
) => a?.done == b?.done && a?.total == b?.total;

export const updateSyncState = (patch: Partial<DriveSyncState>) => {
    const current = state.sync;
    const changed = (Object.keys(patch) as (keyof DriveSyncState)[]).some(
        (k) =>
            k == "progress"
                ? !sameProgress(patch.progress, current.progress)
                : k == "failedFolderIDs"
                  ? patch.failedFolderIDs!.join() !=
                    current.failedFolderIDs.join()
                  : patch[k] !== current[k],
    );
    if (changed) setDriveState({ sync: { ...current, ...patch } });
};

let lastOpID = 0;

export const addPendingOp = (
    kind: PendingDriveOpKind,
    items: readonly DriveItemRef[],
    targetFolderID?: number | null,
): PendingDriveOp => {
    const op = {
        id: ++lastOpID,
        kind,
        items,
        ...(targetFolderID !== undefined && { targetFolderID }),
        startedAt: Date.now(),
    };
    setDriveState({ pendingOps: [...state.pendingOps, op] });
    return op;
};

export const removePendingOp = (op: PendingDriveOp) =>
    setDriveState({ pendingOps: state.pendingOps.filter((o) => o != op) });

export const setPendingTrash = (pendingTrash: readonly PendingTrash[]) =>
    setDriveState({ pendingTrash });
