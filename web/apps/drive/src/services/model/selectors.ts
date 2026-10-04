import type { FileCategory } from "../file-types";
import { nameKey, uniqueName } from "../names";
import {
    itemKey,
    type DriveFile,
    type DriveItem,
    type DriveItemRef,
    type DriveModel,
    type DriveState,
    type DriveTrashItem,
    type Folder,
    type ItemKind,
} from "./types";

export interface FolderContents {
    folders: readonly Folder[];
    files: readonly DriveFile[];
}

export interface FolderStats {
    // Sum of the (estimated plaintext) sizes of every file in the subtree.
    size: number;
    fileCount: number;
    folderCount: number;
}

export type SearchScope = "all" | "my-drive" | "shared";

export interface SearchOptions {
    scope?: SearchScope;
    category?: FileCategory | "folder";
}

// Models are immutable and only replaced when their data changes, so
// results are memoised per model.
const memo = <K, V>(compute: (model: DriveModel, key: K) => V) => {
    const cache = new WeakMap<DriveModel, Map<K, V>>();
    return (model: DriveModel, key: K): V => {
        let entries = cache.get(model);
        if (!entries) cache.set(model, (entries = new Map<K, V>()));
        if (!entries.has(key)) entries.set(key, compute(model, key));
        return entries.get(key)!;
    };
};

const memoOne = <V>(compute: (model: DriveModel) => V) => {
    const cache = new WeakMap<DriveModel, V>();
    return (model: DriveModel): V => {
        if (!cache.has(model)) cache.set(model, compute(model));
        return cache.get(model)!;
    };
};

const push = <K, V>(map: Map<K, V[]>, key: K, value: V) => {
    const values = map.get(key);
    if (values) values.push(value);
    else map.set(key, [value]);
};

const isListedFolder = (f: Folder) => !f.isRootCollection;

const childIndex = memoOne((model: DriveModel) => {
    const subfolders = new Map<number | null, Folder[]>();
    for (const f of model.folders.values())
        if (isListedFolder(f) && f.isOwned) push(subfolders, f.parentID, f);
    const filesByCollection = new Map<number, DriveFile[]>();
    for (const file of model.files.values())
        for (const id of file.collectionIDs) push(filesByCollection, id, file);
    return { subfolders, filesByCollection };
});

const noContents: FolderContents = { folders: [], files: [] };

// The folder at My Drive's root is addressed as null.
const asParent = (model: DriveModel, folderID: number | null) =>
    folderID === model.rootCollectionID ? null : folderID;

// Children of `folderID`, or of My Drive's root when it is null. Folders
// shared with us list only their files: their subfolders aren't shared.
export const contentsOf = (model: DriveModel, folderID: number | null) =>
    contentsOfParent(model, asParent(model, folderID));

const contentsOfParent = memo(
    (model: DriveModel, parent: number | null): FolderContents => {
        const { subfolders, filesByCollection } = childIndex(model);
        const collectionID = parent ?? model.rootCollectionID;
        if (parent !== null && !model.folders.has(parent)) return noContents;
        return {
            folders: subfolders.get(parent) ?? [],
            files:
                collectionID === undefined
                    ? []
                    : (filesByCollection.get(collectionID) ?? []),
        };
    },
);

const itemOf = new WeakMap<Folder | DriveFile, DriveItem>();

const folderItem = (folder: Folder): DriveItem => {
    let item = itemOf.get(folder);
    if (!item) itemOf.set(folder, (item = { kind: "folder", folder }));
    return item;
};

const fileItem = (file: DriveFile): DriveItem => {
    let item = itemOf.get(file);
    if (!item) itemOf.set(file, (item = { kind: "file", file }));
    return item;
};

export type SortKey = "name" | "modified" | "size" | "owner";

export interface SortOrder {
    key: SortKey;
    direction: "asc" | "desc";
}

const collator = new Intl.Collator(undefined, {
    numeric: true,
    sensitivity: "base",
});

const sortCache = new WeakMap<FolderContents, Map<string, DriveItem[]>>();

// Folders first, each group ordered by `key`. Folders have no size, so
// "size" orders them by name. Results are cached per contents object.
export const sortDriveItems = (
    model: DriveModel,
    contents: FolderContents,
    { key, direction }: SortOrder,
): readonly DriveItem[] => {
    let entries = sortCache.get(contents);
    if (!entries)
        sortCache.set(contents, (entries = new Map<string, DriveItem[]>()));
    const cacheKey = `${key}:${direction}`;
    const cached = entries.get(cacheKey);
    if (cached) return cached;

    const sign = direction == "asc" ? 1 : -1;
    const byName = (a: { name: string; id: number }, b: typeof a) =>
        collator.compare(a.name, b.name) || a.id - b.id;
    const ownerName = (isOwned: boolean, ownerID: number) =>
        isOwned ? "" : (userEmail(model, ownerID) ?? `~${ownerID}`);
    const compareBy = <T extends { name: string; id: number }>(
        value: (item: T) => number | string,
    ) => {
        return (a: T, b: T) => {
            const x = value(a);
            const y = value(b);
            const order =
                typeof x == "number" && typeof y == "number"
                    ? x - y
                    : collator.compare(String(x), String(y));
            return sign * order || byName(a, b);
        };
    };

    const folders = [...contents.folders];
    const files = [...contents.files];
    switch (key) {
        case "name":
            folders.sort((a, b) => sign * byName(a, b));
            files.sort((a, b) => sign * byName(a, b));
            break;
        case "modified":
            folders.sort(compareBy((f) => f.updationTime));
            files.sort(compareBy((f) => f.modificationTime));
            break;
        case "size":
            folders.sort((a, b) => sign * byName(a, b));
            files.sort(compareBy((f) => f.size ?? 0));
            break;
        case "owner":
            folders.sort(compareBy((f) => ownerName(f.isOwned, f.owner.id)));
            files.sort(compareBy((f) => ownerName(f.isOwned, f.ownerID)));
            break;
    }
    const sorted = [...folders.map(folderItem), ...files.map(fileItem)];
    entries.set(cacheKey, sorted);
    return sorted;
};

// From the top-level ancestor down to the folder itself. Shared-with-me
// folders are their own top level.
export const folderPath = (model: DriveModel, folderID: number): Folder[] => {
    const path: Folder[] = [];
    const seen = new Set<number>();
    let folder = model.folders.get(folderID);
    while (folder && !seen.has(folder.id)) {
        seen.add(folder.id);
        path.unshift(folder);
        folder =
            folder.parentID === null
                ? undefined
                : model.folders.get(folder.parentID);
    }
    return path;
};

export const isDescendantOrSelf = (
    model: DriveModel,
    folderID: number,
    ancestorID: number,
) => folderPath(model, folderID).some((f) => f.id == ancestorID);

export interface Breadcrumb {
    // null for the top of a section.
    id: number | null;
    // For the top of a section, an i18n key ("drive_my_drive",
    // "drive_shared_with_me").
    name: string;
    section: "my-drive" | "shared";
}

export const breadcrumbs = memo(
    (model: DriveModel, folderID: number | null): readonly Breadcrumb[] => {
        const myDrive: Breadcrumb = {
            id: null,
            name: "drive_my_drive",
            section: "my-drive",
        };
        const parent = asParent(model, folderID);
        const folder = parent === null ? undefined : model.folders.get(parent);
        if (!folder) return [myDrive];
        if (!folder.isOwned)
            return [
                { id: null, name: "drive_shared_with_me", section: "shared" },
                { id: folder.id, name: folder.name, section: "shared" },
            ];
        return [
            myDrive,
            ...folderPath(model, folder.id).map(
                (f): Breadcrumb => ({
                    id: f.id,
                    name: f.name,
                    section: "my-drive",
                }),
            ),
        ];
    },
);

// The folder to go "up" to from an item, or to show as its location. null
// is the top of the item's section (My Drive's root, or Shared with me).
export const navigationParent = (
    model: DriveModel,
    { kind, id }: DriveItemRef,
): number | null => {
    if (kind == "folder") return model.folders.get(id)?.parentID ?? null;
    const file = model.files.get(id);
    return file ? asParent(model, file.folderID) : null;
};

// True for My Drive's root and the live folders we own (subfolders and
// uploads can only go into those, D6).
export const canWriteFolder = (model: DriveModel, folderID: number | null) => {
    const parent = asParent(model, folderID);
    return parent === null || model.folders.get(parent)?.isOwned === true;
};

const emails = memoOne((model: DriveModel) => {
    const byID = new Map<number, string>();
    for (const f of model.folders.values())
        for (const user of [f.owner, ...f.sharees])
            if (user.email) byID.set(user.id, user.email);
    return byID;
});

// Emails are known only for the owners and sharees of folders we can see.
export const userEmail = (model: DriveModel, userID: number) =>
    emails(model).get(userID);

export const sharedWithMe = memoOne((model: DriveModel) =>
    [...model.folders.values()].filter((f) => !f.isOwned),
);

const filesByModification = memoOne((model: DriveModel) =>
    [...model.files.values()].sort(
        (a, b) => b.modificationTime - a.modificationTime || b.id - a.id,
    ),
);

// Most recently modified first (by the file's own modification time, so
// renames and stars don't reorder them).
export const recentFiles = memo((model: DriveModel, limit: number) =>
    filesByModification(model).slice(0, limit),
);

export const starredItems = memoOne(
    (model: DriveModel): FolderContents => ({
        folders: [...model.folders.values()].filter(
            (f) => f.starred && isListedFolder(f),
        ),
        files: [...model.files.values()].filter((f) => f.starred),
    }),
);

export const trashEntries = (model: DriveModel): readonly DriveTrashItem[] =>
    model.trash;

export const trashLastUpdatedAt = (model: DriveModel) => {
    let max = 0;
    for (const t of model.trash) if (t.updatedAt > max) max = t.updatedAt;
    return max;
};

const isInScope = (isOwnedLocation: boolean, scope: SearchScope) =>
    scope == "all" || (scope == "my-drive") == isOwnedLocation;

// Results are memoised per model and query, so the same search returns the
// same object.
export const searchDrive = (
    model: DriveModel,
    query: string,
    { scope = "all", category }: SearchOptions = {},
): FolderContents => {
    const needle = nameKey(query.trim());
    if (!needle) return noContents;
    return searchResults(model, JSON.stringify([needle, scope, category]));
};

const searchResults = memo((model: DriveModel, key: string): FolderContents => {
    const [needle, scope, category] = JSON.parse(key) as [
        string,
        SearchScope,
        SearchOptions["category"] | null,
    ];
    const matches = (name: string) => nameKey(name).includes(needle);

    const folders =
        category && category != "folder"
            ? []
            : [...model.folders.values()].filter(
                  (f) =>
                      isListedFolder(f) &&
                      isInScope(f.isOwned, scope) &&
                      matches(f.name),
              );
    const files =
        category == "folder"
            ? []
            : [...model.files.values()].filter(
                  (file) =>
                      (!category || file.category == category) &&
                      file.collectionIDs.some((id) => {
                          const folder = model.folders.get(id);
                          return !!folder && isInScope(folder.isOwned, scope);
                      }) &&
                      matches(file.name),
              );
    return { folders, files };
});

export const folderStats = memo(
    (model: DriveModel, folderID: number): FolderStats => {
        const { folders, files } = contentsOf(model, folderID);
        const stats: FolderStats = {
            size: files.reduce((sum, f) => sum + (f.size ?? 0), 0),
            fileCount: files.length,
            folderCount: folders.length,
        };
        for (const child of folders) {
            const childStats = folderStats(model, child.id);
            stats.size += childStats.size;
            stats.fileCount += childStats.fileCount;
            stats.folderCount += childStats.folderCount;
        }
        return stats;
    },
);

// The folder and its owned subfolders, at any depth.
export const subtreeIDs = (model: DriveModel, folderID: number) => {
    const { subfolders } = childIndex(model);
    const ids = [folderID];
    for (const id of ids)
        for (const child of subfolders.get(id) ?? []) ids.push(child.id);
    return ids;
};

// Folders and files share one namespace per parent, as on a file system.
export const uniqueChildName = (
    model: DriveModel,
    parentID: number | null,
    desiredName: string,
    kind: ItemKind,
    exclude?: DriveItemRef,
    extraTakenNames: Iterable<string> = [],
) => {
    const { folders, files } = contentsOf(model, parentID);
    const isExcluded = (k: ItemKind, id: number) =>
        exclude?.kind == k && exclude.id == id;
    return uniqueName(
        desiredName,
        [
            ...folders
                .filter((f) => !isExcluded("folder", f.id))
                .map((f) => f.name),
            ...files
                .filter((f) => !isExcluded("file", f.id))
                .map((f) => f.name),
            ...extraTakenNames,
        ],
        kind,
    );
};

const memoPending = <V>(
    compute: (state: Pick<DriveState, "pendingOps" | "pendingTrash">) => V,
) => {
    const cache = new WeakMap<object, WeakMap<object, V>>();
    return (state: Pick<DriveState, "pendingOps" | "pendingTrash">): V => {
        let byTrash = cache.get(state.pendingOps);
        if (!byTrash) cache.set(state.pendingOps, (byTrash = new WeakMap()));
        if (!byTrash.has(state.pendingTrash))
            byTrash.set(state.pendingTrash, compute(state));
        return byTrash.get(state.pendingTrash)!;
    };
};

// Folders queued for deletion, being deleted, or deleted with files still
// on their way to the trash.
export const deletingFolderIDs = memoPending(
    ({ pendingOps, pendingTrash }): ReadonlySet<number> => {
        const ids = new Set<number>();
        for (const op of pendingOps)
            if (op.kind == "delete_folder")
                for (const item of op.items)
                    if (item.kind == "folder") ids.add(item.id);
        for (const entry of pendingTrash)
            for (const id of entry.folderIDs) ids.add(id);
        return ids;
    },
);

// Keys (`itemKey`) of the items with an operation in progress, including
// folders being deleted.
export const pendingItemKeys = memoPending((state): ReadonlySet<string> => {
    const keys = new Set<string>();
    for (const op of state.pendingOps)
        for (const item of op.items) keys.add(itemKey(item));
    for (const id of deletingFolderIDs(state))
        keys.add(itemKey({ kind: "folder", id }));
    return keys;
});
