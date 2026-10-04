import { beforeEach, expect, test } from "vitest";
import { ModelBuilder } from "../src/services/model/build";
import {
    breadcrumbs,
    canWriteFolder,
    contentsOf,
    folderPath,
    folderStats,
    navigationParent,
    recentFiles,
    searchDrive,
    sharedWithMe,
    sortDriveItems,
    starredItems,
    trashEntries,
    uniqueChildName,
    userEmail,
} from "../src/services/model/selectors";
import {
    itemKey,
    toDate,
    type DriveItem,
    type DriveModel,
} from "../src/services/model/types";
import {
    collectionDetails,
    storedCollection,
    storedFile,
    storedTrashItem,
} from "../src/services/records";
import "./environment";
import {
    keys,
    makeCollection,
    makeFile,
    setupKeys,
    userID,
    type CollectionSpec,
    type FileSpec,
} from "./helpers";

// Loaded after ./environment has registered its mocks.
const { encryptJSONBox } = await import("../src/services/crypto");
const { decryptState, DriveDecryptor } =
    await import("../src/services/model/decrypt");
const { parseItems, RemoteCollection, RemoteFile } =
    await import("../src/services/remote/schemas");

beforeEach(setupKeys);

const remoteCollections: unknown[] = [];
const remoteFiles: unknown[] = [];
const collectionKeys = new Map<number, string>();

const folder = async (spec: CollectionSpec) => {
    const { remote, key } = await makeCollection(spec);
    remoteCollections.push(remote);
    collectionKeys.set(spec.id, key);
    return key;
};

const file = async (spec: Omit<FileSpec, "collectionKey">) => {
    const { remote } = await makeFile({
        ...spec,
        collectionKey: collectionKeys.get(spec.collectionID)!,
    });
    remoteFiles.push(remote);
    return remote;
};

const decryptedState = async (
    trash: unknown[] = [],
    decryptor = new DriveDecryptor(),
) => {
    const collections = await Promise.all(
        parseItems(RemoteCollection, remoteCollections, "c").map(async (c) =>
            storedCollection(
                c,
                await encryptJSONBox(
                    collectionDetails(c),
                    collectionKeys.get(c.id)!,
                ),
            ),
        ),
    );
    const files = parseItems(RemoteFile, remoteFiles, "f").map(
        (f) => storedFile(f)!,
    );
    const trashRecords = trash.map(
        (t) =>
            storedTrashItem({
                file: RemoteFile.parse(t),
                updatedAt: 50,
                deleteBy: 99,
            })!,
    );
    return decryptState(decryptor, userID, collections, files, trashRecords);
};

const model = async (trash: unknown[] = []) =>
    new ModelBuilder().build(await decryptedState(trash), userID);

beforeEach(() => {
    remoteCollections.length = 0;
    remoteFiles.length = 0;
    collectionKeys.clear();
});

const names = (items: readonly { name: string }[]) =>
    items.map((i) => i.name).sort();
const rootOf = (m: DriveModel) => contentsOf(m, null);

test("verifies parent wraps and builds the tree", async () => {
    const root = await folder({
        id: 1,
        name: "Uncategorized",
        type: "uncategorized",
    });
    const a = await folder({ id: 2, name: "A" });
    const b = await folder({ id: 3, name: "B", parent: { id: 2, key: a } });
    await folder({ id: 4, name: "C", parent: { id: 3, key: b } });
    await folder({
        id: 5,
        name: "Forged",
        parent: { id: 2, key: a },
        forgedParentKey: b,
    });
    await folder({ id: 6, name: "Orphan", parent: { id: 77, key: root } });
    await folder({
        id: 7,
        name: "Shared",
        ownerID: 2,
        sharees: [{ id: 1, email: "me@example.org", role: "COLLABORATOR" }],
    });
    await file({ id: 100, collectionID: 1, title: "root.txt" });
    await file({ id: 101, collectionID: 3, title: "deep.pdf" });

    const m = await model();
    expect(m.rootCollectionID).toBe(1);
    expect(names(rootOf(m).folders)).toEqual(["A", "Forged", "Orphan"]);
    expect(names(rootOf(m).files)).toEqual(["root.txt"]);
    expect(contentsOf(m, 1)).toBe(rootOf(m));
    expect(names(contentsOf(m, 2).folders)).toEqual(["B"]);
    expect(names(contentsOf(m, 3).files)).toEqual(["deep.pdf"]);
    expect(folderPath(m, 4).map((f) => f.name)).toEqual(["A", "B", "C"]);

    const forged = m.folders.get(5)!;
    expect([
        forged.parentID,
        forged.claimedParentID,
        forged.integrityWarning,
    ]).toEqual([null, 2, true]);
    const orphan = m.folders.get(6)!;
    expect([orphan.parentID, orphan.integrityWarning]).toEqual([null, false]);
    expect(m.folders.get(4)!.integrityWarning).toBe(false);

    const shared = sharedWithMe(m);
    expect(
        shared.map((f) => [f.name, f.role, f.isOwned, f.owner.email]),
    ).toEqual([["Shared", "COLLABORATOR", false, "user2@example.org"]]);
    expect(folderPath(m, 7).map((f) => f.id)).toEqual([7]);
});

test("breaks cycles made of validly wrapped parents", async () => {
    await folder({ id: 2, name: "A" });
    await folder({ id: 3, name: "B" });
    // Replay older but valid wraps so that A → B and B → A.
    const keyA = collectionKeys.get(2)!;
    const keyB = collectionKeys.get(3)!;
    remoteCollections.length = 0;
    const { remote: a } = await makeCollection({
        id: 2,
        name: "A",
        parent: { id: 3, key: keyB },
    });
    const { remote: b } = await makeCollection({
        id: 3,
        name: "B",
        parent: { id: 2, key: keyA },
    });
    // Re-key the collections so that the parent wraps hold their keys.
    const rekeyed = await Promise.all([
        makeCollectionWithKey(a, keyA),
        makeCollectionWithKey(b, keyB),
    ]);
    remoteCollections.push(...rekeyed);

    const m = await model();
    for (const id of [2, 3]) {
        const f = m.folders.get(id)!;
        expect([f.parentID, f.integrityWarning]).toEqual([null, true]);
    }
    expect(names(rootOf(m).folders)).toEqual(["A", "B"]);
});

const makeCollectionWithKey = async (
    remote: Awaited<ReturnType<typeof makeCollection>>["remote"],
    key: string,
) => {
    const { encryptBox } = await import("ente-drive-wasm");
    const box = await encryptBox(key, keys.masterKey);
    const parentKey = collectionKeys.get(remote.parentID!)!;
    const parentWrap = await encryptBox(key, parentKey);
    const { encryptBoxBytes } = await import("ente-drive-wasm");
    const name = await encryptBoxBytes(
        new TextEncoder().encode(remote.id == 2 ? "A" : "B"),
        key,
    );
    return {
        ...remote,
        encryptedKey: box.encryptedData,
        keyDecryptionNonce: box.nonce,
        encryptedName: name.encryptedData,
        nameDecryptionNonce: name.nonce,
        parentEncryptedKey: parentWrap.encryptedData,
        parentKeyNonce: parentWrap.nonce,
    };
};

const t = (n: number) => 1_800_000_000_000_000 + n;

const sampleModel = async (): Promise<DriveModel> => {
    await folder({ id: 1, name: "Uncategorized", type: "uncategorized" });
    const work = await folder({
        id: 2,
        name: "Work",
        magic: { starred: true, visibility: 0 },
    });
    await folder({ id: 3, name: "Reports", parent: { id: 2, key: work } });
    await folder({ id: 7, name: "Team", ownerID: 2 });
    await file({
        id: 100,
        collectionID: 1,
        title: "todo.txt",
        updationTime: t(10),
        metadata: { modificationTime: t(50) },
        encryptedSize: 117,
    });
    await file({
        id: 101,
        collectionID: 2,
        title: "Plan.docx",
        updationTime: t(30),
        metadata: { modificationTime: t(10) },
        privateMagic: { starred: true },
        encryptedSize: 1017,
    });
    await file({
        id: 102,
        collectionID: 3,
        title: "q1.pdf",
        updationTime: t(20),
        metadata: { modificationTime: t(40) },
        publicMagic: { editedName: "Q1 report.pdf", noThumb: true },
        encryptedSize: 2017,
    });
    await file({
        id: 103,
        collectionID: 7,
        title: "team-plan.xlsx",
        ownerID: 2,
        updationTime: t(40),
        metadata: { modificationTime: t(20) },
    });
    // A file of ours that we also contributed to the shared folder.
    const contributed = await file({
        id: 104,
        collectionID: 2,
        title: "shared-notes.md",
        updationTime: t(5),
        encryptedSize: 3017,
    });
    const { remote } = await makeFile({
        id: 104,
        collectionID: 7,
        collectionKey: collectionKeys.get(7)!,
        title: "shared-notes.md",
        updationTime: t(6),
        encryptedSize: contributed.info.fileSize,
    });
    remoteFiles.push(remote);
    return model();
};

test("decodes files and merges multi-collection files", async () => {
    const m = await sampleModel();
    const renamed = m.files.get(102)!;
    expect(renamed).toMatchObject({
        name: "Q1 report.pdf",
        title: "q1.pdf",
        noThumb: true,
        category: "pdf",
        size: 2000,
        fileType: 3,
        creationTime: 1_700_000_000_000_000,
        isOwned: true,
    });
    const multi = m.files.get(104)!;
    expect(multi.collectionIDs.sort()).toEqual([2, 7]);
    expect(multi.folderID).toBe(2);
    expect(multi.updationTime).toBe(t(6));
    expect(m.files.get(103)).toMatchObject({
        isOwned: false,
        folderID: 7,
        starred: false,
    });
    expect(m.files.size).toBe(5);
});

test("selects recent, starred, shared and search results", async () => {
    const m = await sampleModel();
    expect(recentFiles(m, 3).map((f) => f.id)).toEqual([100, 102, 103]);
    expect(recentFiles(m, 3)).toBe(recentFiles(m, 3));
    const starred = starredItems(m);
    expect(names(starred.folders)).toEqual(["Work"]);
    expect(names(starred.files)).toEqual(["Plan.docx"]);

    expect(names(searchDrive(m, "PLAN").files)).toEqual([
        "Plan.docx",
        "team-plan.xlsx",
    ]);
    expect(names(searchDrive(m, "plan", { scope: "my-drive" }).files)).toEqual([
        "Plan.docx",
    ]);
    expect(names(searchDrive(m, "plan", { scope: "shared" }).files)).toEqual([
        "team-plan.xlsx",
    ]);
    expect(names(searchDrive(m, "notes", { scope: "shared" }).files)).toEqual([
        "shared-notes.md",
    ]);
    expect(
        names(searchDrive(m, "plan", { category: "spreadsheet" }).files),
    ).toEqual(["team-plan.xlsx"]);
    const folders = searchDrive(m, "e", { category: "folder" });
    expect([names(folders.folders), folders.files]).toEqual([
        ["Reports", "Team"],
        [],
    ]);
    expect(searchDrive(m, "  ")).toEqual({ folders: [], files: [] });
    expect(names(searchDrive(m, "q1").files)).toEqual(["Q1 report.pdf"]);
    expect(searchDrive(m, "plan ")).toBe(searchDrive(m, "PLAN"));
});

test("computes recursive folder stats", async () => {
    const m = await sampleModel();
    expect(folderStats(m, 2)).toEqual({
        size: 6000,
        fileCount: 3,
        folderCount: 1,
    });
    expect(folderStats(m, 3)).toEqual({
        size: 2000,
        fileCount: 1,
        folderCount: 0,
    });
    expect(folderStats(m, 2)).toBe(folderStats(m, 2));
});

test("resolves child name conflicts across folders and files", async () => {
    const m = await sampleModel();
    expect(uniqueChildName(m, 2, "reports", "folder")).toBe("reports (1)");
    expect(uniqueChildName(m, 2, "plan.docx", "file")).toBe("plan (1).docx");
    expect(
        uniqueChildName(m, 2, "Plan.docx", "file", { kind: "file", id: 101 }),
    ).toBe("Plan.docx");
    expect(uniqueChildName(m, null, "Work", "folder")).toBe("Work (1)");
    expect(uniqueChildName(m, null, "TODO.txt", "file")).toBe("TODO (1).txt");
    expect(uniqueChildName(m, null, "todo.txt", "folder")).toBe("todo.txt (1)");
});

test("decrypts trash items with the keys of deleted folders", async () => {
    await folder({ id: 1, name: "Uncategorized", type: "uncategorized" });
    await folder({ id: 8, name: "Gone" });
    const { remote: trashed } = await makeFile({
        id: 200,
        collectionID: 8,
        collectionKey: collectionKeys.get(8)!,
        title: "old.mp3",
    });
    (remoteCollections[1] as { isDeleted?: boolean }).isDeleted = true;
    const m = await model([trashed]);
    expect(m.folders.has(8)).toBe(false);
    expect(
        trashEntries(m).map((t) => [t.file.name, t.deleteBy, t.file.category]),
    ).toEqual([["old.mp3", 99, "audio"]]);
});

test("tolerates metadata written by other clients", async () => {
    await folder({ id: 1, name: "Uncategorized", type: "uncategorized" });
    await file({
        id: 300,
        collectionID: 1,
        title: "",
        metadata: {
            title: undefined,
            creationTime: 1_700_000_000_000,
            modificationTime: "x",
            fileType: "other",
        },
        updationTime: 1_800_000_000_000_000,
    });
    const m = await model();
    expect(m.files.get(300)).toMatchObject({
        name: "Untitled",
        creationTime: 1_700_000_000_000_000,
        modificationTime: 1_700_000_000_000_000,
        fileType: undefined,
    });
});

const keysOf = (items: readonly DriveItem[]) => items.map(itemKey);

test("sorts folders first, by name, modification time, size or owner", async () => {
    const m = await sampleModel();
    const work = contentsOf(m, 2);
    expect(
        keysOf(sortDriveItems(m, work, { key: "name", direction: "asc" })),
    ).toEqual(["folder:3", "file:101", "file:104"]);
    expect(
        keysOf(sortDriveItems(m, work, { key: "modified", direction: "desc" })),
    ).toEqual(["folder:3", "file:101", "file:104"]);
    expect(
        keysOf(sortDriveItems(m, work, { key: "size", direction: "desc" })),
    ).toEqual(["folder:3", "file:104", "file:101"]);
    expect(sortDriveItems(m, work, { key: "size", direction: "desc" })).toBe(
        sortDriveItems(m, work, { key: "size", direction: "desc" }),
    );
    const shared = contentsOf(m, 7);
    expect(
        keysOf(sortDriveItems(m, shared, { key: "owner", direction: "asc" })),
    ).toEqual(["file:104", "file:103"]);
});

test("orders names naturally", async () => {
    await folder({ id: 1, name: "Uncategorized", type: "uncategorized" });
    for (const [i, title] of [
        "file 10.txt",
        "File 9.txt",
        "file 1.txt",
    ].entries())
        await file({ id: 200 + i, collectionID: 1, title });
    const m = await model();
    const sorted = sortDriveItems(m, rootOf(m), {
        key: "name",
        direction: "asc",
    });
    expect(sorted.map((i) => (i.kind == "file" ? i.file.name : ""))).toEqual([
        "file 1.txt",
        "File 9.txt",
        "file 10.txt",
    ]);
});

test("derives breadcrumbs, navigation parents, emails and write access", async () => {
    const m = await sampleModel();
    expect(breadcrumbs(m, null)).toEqual([
        { id: null, name: "drive_my_drive", section: "my-drive" },
    ]);
    expect(breadcrumbs(m, 1)).toEqual(breadcrumbs(m, null));
    expect(breadcrumbs(m, 3)).toBe(breadcrumbs(m, 3));
    expect(breadcrumbs(m, 3)).toEqual([
        { id: null, name: "drive_my_drive", section: "my-drive" },
        { id: 2, name: "Work", section: "my-drive" },
        { id: 3, name: "Reports", section: "my-drive" },
    ]);
    expect(breadcrumbs(m, 7)).toEqual([
        { id: null, name: "drive_shared_with_me", section: "shared" },
        { id: 7, name: "Team", section: "shared" },
    ]);
    expect(navigationParent(m, { kind: "folder", id: 3 })).toBe(2);
    expect(navigationParent(m, { kind: "folder", id: 2 })).toBeNull();
    expect(navigationParent(m, { kind: "file", id: 100 })).toBeNull();
    expect(navigationParent(m, { kind: "file", id: 102 })).toBe(3);
    expect(userEmail(m, 2)).toBe("user2@example.org");
    expect(userEmail(m, 99)).toBeUndefined();
    expect([null, 1, 3, 7].map((id) => canWriteFolder(m, id))).toEqual([
        true,
        true,
        true,
        false,
    ]);
    expect(toDate(1_700_000_000_123_456).getTime()).toBe(1_700_000_000_123);
});

test("reuses unchanged objects when rebuilding the model", async () => {
    await sampleModel();
    const decryptor = new DriveDecryptor();
    const builder = new ModelBuilder();
    const first = builder.build(await decryptedState([], decryptor), userID);
    expect(builder.build(await decryptedState([], decryptor), userID)).toBe(
        first,
    );

    remoteFiles.push(
        (
            await makeFile({
                id: 105,
                collectionID: 3,
                collectionKey: collectionKeys.get(3)!,
                title: "new.txt",
            })
        ).remote,
    );
    const second = builder.build(await decryptedState([], decryptor), userID);
    expect(second).not.toBe(first);
    expect(second.folders).toBe(first.folders);
    expect(second.files.get(101)).toBe(first.files.get(101));
    expect(second.files.get(105)?.name).toBe("new.txt");
});

const storedRecord = async (spec: CollectionSpec) => {
    const { remote, key } = await makeCollection(spec);
    const [c] = parseItems(RemoteCollection, [remote], "c");
    return {
        record: storedCollection(
            c!,
            await encryptJSONBox(collectionDetails(c!), key),
        ),
        key,
    };
};

test("keeps decrypted collections, and failures, until their records change", async () => {
    const decryptor = new DriveDecryptor();
    const { record } = await storedRecord({ id: 2, name: "A" });
    const decrypted = await decryptor.decryptCollection(record, userID);
    expect(decrypted?.name).toBe("A");
    // As read back from the database.
    expect(
        await decryptor.decryptCollection(structuredClone(record), userID),
    ).toBe(decrypted);

    const shared = await storedRecord({ id: 5, name: "S", ownerID: 7 });
    const sealedKey = shared.record.encryptedKey!;
    const key = keys.shared.get(sealedKey)!;
    keys.shared.delete(sealedKey);
    expect(
        await decryptor.decryptCollection(shared.record, userID),
    ).toBeUndefined();
    keys.shared.set(sealedKey, key);
    expect(
        await decryptor.decryptCollection(
            structuredClone(shared.record),
            userID,
        ),
    ).toBeUndefined();
    const changed = { ...shared.record, updationTime: 2000 };
    expect((await decryptor.decryptCollection(changed, userID))?.name).toBe(
        "S",
    );
});

test("keeps a folder whose files alone changed", async () => {
    await folder({ id: 2, name: "A", updationTime: 100 });
    const decryptor = new DriveDecryptor();
    const builder = new ModelBuilder();
    const first = builder.build(await decryptedState([], decryptor), userID);
    remoteCollections[0] = {
        ...(remoteCollections[0] as object),
        updationTime: 200,
    };
    const second = builder.build(await decryptedState([], decryptor), userID);
    expect(second.folders).toBe(first.folders);
});
