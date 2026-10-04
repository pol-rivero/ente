import { fileCategory } from "../file-types";
import type { StoredTrashItem } from "../records";
import type {
    DecryptedCollection,
    DecryptedFileRow,
    DecryptedState,
} from "./decrypt";
import type {
    DriveFile,
    DriveModel,
    DriveRole,
    DriveTrashItem,
    Folder,
    Sharee,
} from "./types";

const roles = new Set<string>(["VIEWER", "COLLABORATOR", "ADMIN", "OWNER"]);
const asRole = (role: string | undefined): DriveRole =>
    role && roles.has(role) ? (role as DriveRole) : "VIEWER";

const hiddenCollectionTypes = new Set(["favorites"]);

const streamChunkSize = 4 * 1024 * 1024;
const streamChunkOverhead = 17;

const plaintextSize = (encryptedSize: number | undefined) => {
    if (encryptedSize === undefined || encryptedSize < 0) return undefined;
    const chunks = Math.ceil(
        encryptedSize / (streamChunkSize + streamChunkOverhead),
    );
    return Math.max(0, encryptedSize - chunks * streamChunkOverhead);
};

const buildFolder = (
    c: DecryptedCollection,
    userID: number | undefined,
): Folder => {
    const { record, details } = c;
    const isOwned = record.ownerID == userID;
    const sharees: Sharee[] = details.sharees.map((s) => ({
        id: s.id,
        email: s.email,
        role: asRole(s.role),
    }));
    const type = record.type == "uncategorized" ? "uncategorized" : "folder";
    return {
        id: record.id,
        name: c.name,
        type,
        parentID: null,
        claimedParentID: record.parentID ?? null,
        isOwned,
        owner: { id: record.ownerID, email: details.owner.email },
        role: isOwned
            ? "OWNER"
            : (sharees.find((s) => s.id == userID)?.role ?? "VIEWER"),
        sharees,
        publicLinks: details.publicLinks,
        isShared: isOwned
            ? sharees.length > 0 || details.publicLinks.length > 0
            : true,
        updationTime: record.updationTime,
        starred: c.starred,
        integrityWarning: false,
        isRootCollection: isOwned && type == "uncategorized",
    };
};

const shallowEqual = (a: object, b: object) => {
    const ka = Object.keys(a);
    if (ka.length != Object.keys(b).length) return false;
    return ka.every(
        (k) =>
            (a as Record<string, unknown>)[k] ===
            (b as Record<string, unknown>)[k],
    );
};

const sameList = <T extends object>(a: readonly T[], b: readonly T[]) =>
    a.length == b.length && a.every((x, i) => shallowEqual(x, b[i]!));

// Ignores updationTime, which every change to the folder's files moves.
const sameFolder = (a: Folder, b: Folder) =>
    shallowEqual(
        { ...a, owner: 0, sharees: 0, publicLinks: 0, updationTime: 0 },
        { ...b, owner: 0, sharees: 0, publicLinks: 0, updationTime: 0 },
    ) &&
    shallowEqual(a.owner, b.owner) &&
    sameList(a.sharees, b.sharees) &&
    sameList(a.publicLinks, b.publicLinks);

// Accepts only verified parents, then breaks any cycle (a server could
// replay older, validly wrapped parents to form one) by moving its members
// to the root with a warning.
const resolveTree = (
    folders: Map<number, Folder>,
    parentWrapValid: ReadonlyMap<number, boolean>,
) => {
    for (const folder of folders.values()) {
        const parentID = folder.claimedParentID;
        if (!folder.isOwned || parentID === null) continue;
        const parent = folders.get(parentID);
        const isValid = parentWrapValid.get(folder.id);
        if (isValid === false) folder.integrityWarning = true;
        else if (isValid && parent && !parent.isRootCollection)
            folder.parentID = parentID;
    }

    const settled = new Set<number>();
    for (const start of folders.values()) {
        const path: Folder[] = [];
        const onPath = new Set<number>();
        let node: Folder | undefined = start;
        while (node && !settled.has(node.id)) {
            if (onPath.has(node.id)) {
                for (const member of path.slice(
                    path.findIndex((f) => f.id == node!.id),
                )) {
                    member.parentID = null;
                    member.integrityWarning = true;
                }
                break;
            }
            onPath.add(node.id);
            path.push(node);
            node =
                node.parentID === null ? undefined : folders.get(node.parentID);
        }
        for (const f of path) settled.add(f.id);
    }
};

const buildFile = (
    rows: readonly DecryptedFileRow[],
    folders: ReadonlyMap<number, Folder>,
    userID: number | undefined,
): DriveFile => {
    const isOwnFolder = (row: DecryptedFileRow) =>
        folders.get(row.record.collectionID)?.isOwned == true;
    const primary = rows.find(isOwnFolder) ?? rows[0]!;
    const { record, title } = primary;
    const name = primary.editedName ?? (title || "Untitled");
    const updationTime = rows.reduce(
        (max, r) => Math.max(max, r.record.updationTime),
        0,
    );
    const modificationTime =
        primary.modificationTime ?? primary.creationTime ?? updationTime;
    const isOwned = record.ownerID == userID;
    return {
        id: record.id,
        name,
        title,
        collectionIDs: [...new Set(rows.map((r) => r.record.collectionID))],
        folderID: record.collectionID,
        size: plaintextSize(record.encryptedSize),
        mimeType: primary.mimeType,
        fileType: primary.fileType,
        category: fileCategory(name, primary.mimeType),
        creationTime: primary.creationTime ?? modificationTime,
        modificationTime,
        updationTime,
        ownerID: record.ownerID,
        isOwned,
        starred:
            isOwned && rows.some((r) => r.starred !== undefined)
                ? rows.find((r) => r.starred !== undefined)!.starred!
                : false,
        hash: primary.hash,
        noThumb: primary.noThumb,
    };
};

const sameRows = (a: readonly unknown[], b: readonly unknown[]) =>
    a.length == b.length && a.every((x, i) => x === b[i]);

const sameMap = <K, V>(a: ReadonlyMap<K, V>, b: ReadonlyMap<K, V>) => {
    if (a.size != b.size) return false;
    for (const [k, v] of a) if (b.get(k) !== v) return false;
    return true;
};

// Builds models from decrypted state, reusing the objects (and the model)
// of the previous build for whatever didn't change, so that selectors and
// components keyed on them see stable identities.
export class ModelBuilder {
    private model: DriveModel | undefined;
    private files = new Map<
        number,
        { rows: readonly DecryptedFileRow[]; file: DriveFile }
    >();
    private trash = new Map<
        number,
        { row: DecryptedFileRow<StoredTrashItem>; item: DriveTrashItem }
    >();

    build(
        { collections, parentWrapValid, files, trash }: DecryptedState,
        userID: number | undefined,
    ): DriveModel {
        const previous = this.model;

        const built = new Map<number, Folder>();
        for (const c of collections)
            if (!hiddenCollectionTypes.has(c.record.type))
                built.set(c.record.id, buildFolder(c, userID));
        resolveTree(built, parentWrapValid);
        const folders = new Map<number, Folder>();
        for (const [id, folder] of built) {
            const old = previous?.folders.get(id);
            folders.set(id, old && sameFolder(old, folder) ? old : folder);
        }

        const rowsByFile = new Map<number, DecryptedFileRow[]>();
        for (const row of files) {
            if (!folders.has(row.record.collectionID)) continue;
            const rows = rowsByFile.get(row.record.id);
            if (rows) rows.push(row);
            else rowsByFile.set(row.record.id, [row]);
        }
        const fileCache = new Map<
            number,
            { rows: readonly DecryptedFileRow[]; file: DriveFile }
        >();
        const driveFiles = new Map<number, DriveFile>();
        for (const [id, rows] of rowsByFile) {
            const cached = this.files.get(id);
            const entry =
                cached && sameRows(cached.rows, rows)
                    ? cached
                    : { rows, file: buildFile(rows, folders, userID) };
            fileCache.set(id, entry);
            driveFiles.set(id, entry.file);
        }
        this.files = fileCache;

        const trashCache = new Map<
            number,
            { row: DecryptedFileRow<StoredTrashItem>; item: DriveTrashItem }
        >();
        for (const row of trash) {
            const cached = this.trash.get(row.record.id);
            trashCache.set(
                row.record.id,
                cached?.row === row
                    ? cached
                    : {
                          row,
                          item: {
                              file: buildFile([row], folders, userID),
                              deleteBy: row.record.deleteBy,
                              updatedAt: row.record.updatedAt,
                          },
                      },
            );
        }
        this.trash = trashCache;
        const trashItems = [...trashCache.values()]
            .map((t) => t.item)
            .sort((a, b) => b.updatedAt - a.updatedAt);

        let rootCollectionID: number | undefined;
        for (const f of folders.values())
            if (
                f.isRootCollection &&
                (rootCollectionID === undefined || f.id < rootCollectionID)
            )
                rootCollectionID = f.id;

        const model: DriveModel = {
            userID,
            folders:
                previous && sameMap(previous.folders, folders)
                    ? previous.folders
                    : folders,
            files:
                previous && sameMap(previous.files, driveFiles)
                    ? previous.files
                    : driveFiles,
            trash:
                previous && sameRows(previous.trash, trashItems)
                    ? previous.trash
                    : trashItems,
            rootCollectionID,
        };
        this.model =
            previous &&
            previous.userID == userID &&
            previous.folders == model.folders &&
            previous.files == model.files &&
            previous.trash == model.trash &&
            previous.rootCollectionID == rootCollectionID
                ? previous
                : model;
        return this.model;
    }
}
