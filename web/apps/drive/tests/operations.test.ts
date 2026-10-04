import { decryptBlob, decryptBox, decryptBoxBytes } from "ente-drive-wasm";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { env } from "./environment";
import { FakeMuseum, type FakeTrashEntry } from "./fake-museum";
import {
    bodiesOf,
    createFakeDB,
    fakeFetch,
    keys,
    makeCollection,
    makeFile,
    setupKeys,
    type FakeRequest,
    type FakeResponse,
} from "./helpers";

let museum: FakeMuseum;
let requests: FakeRequest[];
let api: typeof import("../src/services/drive-api");
let sync: typeof import("../src/services/sync");
const collectionKeys = new Map<number, string>();
const fileKeys = new Map<number, string>();

const addCollection = async (spec: Parameters<typeof makeCollection>[0]) => {
    const { remote, key } = await makeCollection({
        updationTime: museum.now(),
        ...spec,
    });
    museum.putCollection(remote);
    collectionKeys.set(spec.id, key);
    return key;
};

const addFile = async (
    id: number,
    collectionID: number,
    title: string,
    extra: Partial<Parameters<typeof makeFile>[0]> = {},
) => {
    const { remote, fileKey } = await makeFile({
        id,
        collectionID,
        collectionKey: collectionKeys.get(collectionID)!,
        title,
        fileKey: fileKeys.get(id),
        updationTime: museum.now(),
        ...extra,
    });
    museum.putFile(collectionID, remote);
    const collection = museum.collections.get(collectionID)!;
    museum.putCollection({ ...collection, updationTime: remote.updationTime });
    fileKeys.set(id, fileKey);
    return remote;
};

const loadServices = async () => {
    vi.resetModules();
    api = await import("../src/services/drive-api");
    sync = await import("../src/services/sync");
};

beforeEach(async () => {
    await setupKeys();
    env.db.current = createFakeDB();
    museum = new FakeMuseum();
    const fake = fakeFetch(museum.handle);
    requests = fake.requests;
    vi.stubGlobal("fetch", fake.fetch);
    await loadServices();

    collectionKeys.clear();
    fileKeys.clear();
    await addCollection({
        id: 1,
        name: "Uncategorized",
        type: "uncategorized",
    });
    const a = await addCollection({
        id: 2,
        name: "A",
        magic: { visibility: 1 },
    });
    await addCollection({ id: 3, name: "B", parent: { id: 2, key: a } });
    await addCollection({ id: 4, name: "C" });
    await addCollection({
        id: 9,
        name: "Theirs",
        ownerID: 2,
        sharees: [{ id: 1, email: "me@example.org", role: "COLLABORATOR" }],
    });
    await addFile(100, 2, "report.pdf", {
        publicMagic: { noThumb: true },
        publicMagicVersion: 2,
    });
    await addFile(101, 4, "report.pdf");
    await addFile(102, 4, "notes.txt");
    await addFile(103, 9, "theirs.txt", { ownerID: 2 });
    await sync.syncDrive();
    requests.length = 0;
});

afterEach(async () => {
    // Background syncs of this test must not run into the next one.
    await vi.waitFor(() => expect(api.driveState().pendingOps).toEqual([]));
    vi.unstubAllGlobals();
});

const state = () => api.driveState();
const model = () => state().model;
const contents = (id: number | null) => api.contentsOf(model(), id);
const names = (items: readonly { name: string }[]) =>
    items.map((i) => i.name).sort();
const writes = (path: string) =>
    requests.filter((r) => r.path == path && r.method != "GET");

// Lets the syncs that follow operations finish.
const settle = () => sync.syncDrive();

const openBox = (encryptedData: string, nonce: string, key: string) =>
    decryptBox({ encryptedData, nonce }, key);

const openMagic = async (mm: { data: string; header: string }, key: string) =>
    JSON.parse(
        new TextDecoder().decode(
            await decryptBlob(
                { encryptedData: mm.data, decryptionHeader: mm.header },
                key,
            ),
        ),
    ) as unknown;

interface MagicMetadataBody {
    metadataList: {
        id: number;
        magicMetadata: {
            version: number;
            count: number;
            data: string;
            header: string;
        };
    }[];
}

interface CreateBody {
    encryptedKey: string;
    keyDecryptionNonce: string;
    encryptedName: string;
    nameDecryptionNonce: string;
    parentID?: number;
    parentEncryptedKey: string;
    parentKeyNonce: string;
}

test("creates a child folder with a verifiable parent wrap, shown right away", async () => {
    const id = await api.createFolder(2, "  b ");
    expect(model().folders.get(id)).toMatchObject({
        name: "b (1)",
        parentID: 2,
    });

    const [body] = bodiesOf<CreateBody>(requests, "/collections");
    expect(body).toMatchObject({ type: "folder", parentID: 2 });
    expect(body).not.toHaveProperty("name");
    const key = await openBox(
        body!.encryptedKey,
        body!.keyDecryptionNonce,
        keys.masterKey,
    );
    expect(
        await openBox(
            body!.parentEncryptedKey,
            body!.parentKeyNonce,
            collectionKeys.get(2)!,
        ),
    ).toBe(key);
    const name = await decryptBoxBytes(
        {
            encryptedData: body!.encryptedName,
            nonce: body!.nameDecryptionNonce,
        },
        key,
    );
    expect(new TextDecoder().decode(name)).toBe("b (1)");
    expect(writes("/collections/move-collection")).toEqual([]);
});

test("moves a new folder whose parent an old server dropped", async () => {
    museum.overrides.push((r) => {
        if (r.path != "/collections" || r.method != "POST") return undefined;
        const { parentID: _, ...rest } = r.body as CreateBody;
        const collection = {
            ...rest,
            id: 51,
            owner: { id: 1 },
            updationTime: 5000,
        };
        museum.putCollection(collection);
        return { json: { collection } };
    });
    await api.createFolder(2, "New");
    const [move] = writes("/collections/move-collection");
    expect(move!.body).toMatchObject({ collectionID: 51, newParentID: 2 });
});

test("a create whose response was lost doesn't create a duplicate", async () => {
    museum.dropResponse = (r) => r.path == "/collections" && r.method == "POST";
    const id = await api.createFolder(2, "Reports");
    museum.dropResponse = undefined;
    await settle();
    expect(names(contents(2).folders)).toEqual(["B", "Reports"]);
    expect(model().folders.get(id)?.name).toBe("Reports");
    expect(writes("/collections")).toHaveLength(1);
});

test("concurrent creates pick different names", async () => {
    const ids = await Promise.all([
        api.createFolder(null, "New"),
        api.createFolder(null, "New"),
    ]);
    await settle();
    expect(ids.map((id) => model().folders.get(id)!.name).sort()).toEqual([
        "New",
        "New (1)",
    ]);
});

test("refuses subfolders in folders shared with us, and invalid names", async () => {
    await expect(api.createFolder(9, "x")).rejects.toMatchObject({
        reason: "not_owner",
    });
    await expect(api.createFolder(null, "a/b")).rejects.toMatchObject({
        reason: "invalid_name",
    });
    expect(requests).toEqual([]);
});

test("moves folders, re-wrapping for the new parent and mapping errors", async () => {
    await api.moveFolder(3, 4);
    const [toC] = bodiesOf<{
        parentEncryptedKey: string;
        parentKeyNonce: string;
    }>(requests, "/collections/move-collection");
    expect(
        await openBox(
            toC!.parentEncryptedKey,
            toC!.parentKeyNonce,
            collectionKeys.get(4)!,
        ),
    ).toBe(collectionKeys.get(3));
    await settle();
    expect(model().folders.get(3)?.parentID).toBe(4);

    requests.length = 0;
    await api.moveFolder(3, null);
    expect(writes("/collections/move-collection")[0]!.body).toEqual({
        collectionID: 3,
        newParentID: null,
    });
    await settle();

    await expect(api.moveFolder(2, 2)).rejects.toMatchObject({
        reason: "cycle",
    });

    museum.overrides.unshift((r) =>
        r.path == "/collections/move-collection"
            ? { status: 400, json: { code: "MAX_DEPTH_EXCEEDED", message: "" } }
            : undefined,
    );
    await expect(api.moveFolder(4, 2)).rejects.toMatchObject({
        reason: "max_depth",
    });

    museum.overrides.unshift((r) =>
        r.path == "/collections/move-collection"
            ? { status: 404, text: "404 page not found" }
            : undefined,
    );
    await expect(api.moveFolder(4, 2)).rejects.toMatchObject({
        reason: "server_outdated",
    });
});

test("renames folders without an extension, and keeps the root unrenamable", async () => {
    await api.renameFolder(4, "v1.2");
    await settle();
    expect(await api.renameFolder(2, "V1.2")).toBe("V1.2 (1)");
    await expect(api.renameFolder(1, "x")).rejects.toMatchObject({
        reason: "invalid_target",
    });
});

test("retries a file rename after a version conflict, merging the latest metadata", async () => {
    const fileKey = fileKeys.get(100)!;
    // Another device changes the metadata meanwhile.
    await addFile(100, 2, "report.pdf", {
        publicMagic: { noThumb: true, caption: "from another device" },
        publicMagicVersion: 3,
    });

    expect(await api.renameFile(100, "Final.pdf")).toBe("Final.pdf");
    const bodies = bodiesOf<MagicMetadataBody>(
        requests,
        "/files/public-magic-metadata",
    );
    expect(bodies.map((b) => b.metadataList[0]!.magicMetadata.version)).toEqual(
        [2, 3],
    );
    const merged = bodies[1]!.metadataList[0]!.magicMetadata;
    expect(await openMagic(merged, fileKey)).toEqual({
        noThumb: true,
        caption: "from another device",
        editedName: "Final.pdf",
    });
    expect(merged.count).toBe(3);
    await settle();
    expect(model().files.get(100)?.name).toBe("Final.pdf");
});

test("a rename conflict after the file moved elsewhere refetches from its new folder", async () => {
    const fileKey = fileKeys.get(102)!;
    const moved = await addFile(102, 2, "notes.txt", {
        publicMagic: { caption: "c" },
        publicMagicVersion: 5,
    });
    museum.putFile(4, {
        id: 102,
        collectionID: 4,
        isDeleted: true,
        updationTime: moved.updationTime,
    });

    expect(await api.renameFile(102, "renamed.txt")).toBe("renamed.txt");
    const bodies = bodiesOf<MagicMetadataBody>(
        requests,
        "/files/public-magic-metadata",
    );
    expect(
        await openMagic(bodies.at(-1)!.metadataList[0]!.magicMetadata, fileKey),
    ).toEqual({ caption: "c", editedName: "renamed.txt" });
});

test("gives up a rename after repeated conflicts", async () => {
    museum.overrides.push((r) =>
        r.path == "/files/public-magic-metadata"
            ? { status: 409, json: {} }
            : undefined,
    );
    await expect(api.renameFile(100, "x.pdf")).rejects.toMatchObject({
        reason: "conflict",
    });
    expect(writes("/files/public-magic-metadata")).toHaveLength(4);
});

test("moves files and folders, re-wrapping keys and renaming arrivals that clash", async () => {
    const result = await api.moveItems(
        [
            { kind: "file", id: 101 },
            { kind: "file", id: 102 },
            { kind: "file", id: 100 },
            { kind: "folder", id: 4 },
        ],
        2,
    );
    expect(result).toEqual({
        renamed: [{ ref: { kind: "file", id: 101 }, name: "report (1).pdf" }],
        failed: [],
    });
    const [move] = bodiesOf<{
        fromCollectionID: number;
        toCollectionID: number;
        files: {
            id: number;
            encryptedKey: string;
            keyDecryptionNonce: string;
        }[];
    }>(requests, "/collections/move-files");
    expect([move!.fromCollectionID, move!.toCollectionID]).toEqual([4, 2]);
    expect(move!.files.map((f) => f.id)).toEqual([101, 102]);
    for (const f of move!.files)
        expect(
            await openBox(
                f.encryptedKey,
                f.keyDecryptionNonce,
                collectionKeys.get(2)!,
            ),
        ).toBe(fileKeys.get(f.id));
    expect(api.pendingItemKeys(state()).has("file:101")).toBe(true);

    await settle();
    expect(api.pendingItemKeys(state()).size).toBe(0);
    expect(names(contents(2).files)).toEqual([
        "notes.txt",
        "report (1).pdf",
        "report.pdf",
    ]);
    expect(names(contents(2).folders)).toEqual(["B", "C"]);
});

test("rejects a move only when nothing moved, mapping errors", async () => {
    await expect(
        api.moveItems([{ kind: "file", id: 103 }], 2),
    ).rejects.toMatchObject({ reason: "not_owner" });
    museum.overrides.unshift((r) =>
        r.path == "/collections/move-files"
            ? { status: 409, json: { code: "FILE_IN_TRASH" } }
            : undefined,
    );
    await expect(
        api.moveItems([{ kind: "file", id: 102 }], 3),
    ).rejects.toMatchObject({ reason: "file_in_trash" });
    museum.overrides.shift();

    const partial = await api.moveItems(
        [
            { kind: "file", id: 103 },
            { kind: "file", id: 102 },
        ],
        3,
    );
    expect(partial.failed).toEqual([
        { ref: { kind: "file", id: 103 }, reason: "not_owner" },
    ]);
});

test("a move into a folder deleted meanwhile reports folder_deleted", async () => {
    museum.collections.set(3, {
        ...museum.collections.get(3)!,
        isDeleted: true,
        updationTime: 9000,
    });
    await expect(
        api.moveItems([{ kind: "file", id: 102 }], 3),
    ).rejects.toMatchObject({ reason: "folder_deleted" });
});

test("moves a file out of every folder of ours it is in", async () => {
    await addFile(102, 2, "notes.txt");
    await sync.syncDrive();
    expect(model().files.get(102)!.collectionIDs.sort()).toEqual([2, 4]);

    await api.moveItems([{ kind: "file", id: 102 }], 3);
    await settle();
    expect(museum.liveCollectionIDsOf(102)).toEqual([3]);
    expect(model().files.get(102)!.collectionIDs).toEqual([3]);

    await addFile(102, 2, "notes.txt");
    await sync.syncDrive();
    await api.moveItems([{ kind: "file", id: 102 }], 2);
    await settle();
    expect(museum.liveCollectionIDsOf(102)).toEqual([2]);
});

test("a partly failed move still renames what arrived, and reports the rest", async () => {
    await addCollection({ id: 5, name: "D" });
    await addFile(110, 5, "other.txt");
    await addFile(111, 2, "notes.txt");
    await sync.syncDrive();
    let n = 0;
    museum.overrides.push((r) =>
        r.path == "/collections/move-files" && ++n == 2
            ? { status: 500, json: { code: "INTERNAL" } }
            : undefined,
    );
    const result = await api.moveItems(
        [
            { kind: "file", id: 102 },
            { kind: "file", id: 110 },
        ],
        2,
    );
    expect(result.failed).toEqual([
        { ref: { kind: "file", id: 110 }, reason: "unknown" },
    ]);
    await settle();
    expect(names(contents(2).files)).toEqual([
        "notes (1).txt",
        "notes.txt",
        "report.pdf",
    ]);
    expect(contents(5).files.map((f) => f.id)).toEqual([110]);
});

test("stars files and folders, merging the server's latest folder metadata", async () => {
    // Another device changed the folder's metadata since our last sync.
    const { encryptBlob } = await import("ente-drive-wasm");
    const blob = await encryptBlob(
        new TextEncoder().encode(JSON.stringify({ visibility: 1, order: 5 })),
        collectionKeys.get(2)!,
    );
    museum.putCollection({
        ...museum.collections.get(2)!,
        magicMetadata: {
            version: 7,
            count: 2,
            data: blob.encryptedData,
            header: blob.decryptionHeader,
        },
    });

    await api.setStarred({ kind: "file", id: 102 }, true);
    await api.setStarred({ kind: "folder", id: 2 }, true);
    const [fileBody] = bodiesOf<MagicMetadataBody>(
        requests,
        "/files/magic-metadata",
    );
    expect(
        await openMagic(
            fileBody!.metadataList[0]!.magicMetadata,
            fileKeys.get(102)!,
        ),
    ).toEqual({ starred: true });
    const [folderBody] = bodiesOf<{
        id: number;
        magicMetadata: { version: number; data: string; header: string };
    }>(requests, "/collections/magic-metadata");
    expect(folderBody!.magicMetadata.version).toBe(7);
    expect(
        await openMagic(folderBody!.magicMetadata, collectionKeys.get(2)!),
    ).toEqual({ visibility: 1, order: 5, starred: true });
    await settle();
    const starred = api.starredItems(model());
    expect([names(starred.folders), names(starred.files)]).toEqual([
        ["A"],
        ["notes.txt"],
    ]);

    await expect(
        api.setStarred({ kind: "file", id: 103 }, true),
    ).rejects.toMatchObject({ reason: "not_owner" });
});

test("stars folders shared with us in our sharee metadata", async () => {
    // Another client's key in our sharee metadata.
    const { encryptBlob } = await import("ente-drive-wasm");
    const blob = await encryptBlob(
        new TextEncoder().encode(JSON.stringify({ visibility: 2 })),
        collectionKeys.get(9)!,
    );
    museum.putCollection({
        ...museum.collections.get(9)!,
        sharedMagicMetadata: {
            version: 3,
            count: 1,
            data: blob.encryptedData,
            header: blob.decryptionHeader,
        },
        updationTime: museum.now(),
    });
    await settle();

    await api.setStarred({ kind: "folder", id: 9 }, true);
    const [body] = bodiesOf<{
        id: number;
        magicMetadata: { version: number; data: string; header: string };
    }>(requests, "/collections/sharee-magic-metadata");
    expect(body!.id).toBe(9);
    expect(body!.magicMetadata.version).toBe(3);
    expect(
        await openMagic(body!.magicMetadata, collectionKeys.get(9)!),
    ).toEqual({ visibility: 2, starred: true });
    await settle();
    expect(model().folders.get(9)?.starred).toBe(true);
    expect(names(api.starredItems(model()).folders)).toEqual(["Theirs"]);
});

test("trashes files and folders, reporting per-item failures", async () => {
    museum.overrides.push((r) =>
        r.path == "/collections/v4/3"
            ? { status: 409, json: { code: "SUBTREE_TOO_LARGE" } }
            : undefined,
    );
    const result = await api.trashItems([
        { kind: "file", id: 100 },
        { kind: "file", id: 103 },
        { kind: "folder", id: 4 },
        { kind: "folder", id: 3 },
    ]);
    expect(result).toMatchObject({
        trashedFileIDs: [100],
        deletedFolderIDs: [4],
    });
    expect(result.failed).toHaveLength(2);
    expect(result.failed).toEqual(
        expect.arrayContaining([
            { ref: { kind: "file", id: 103 }, reason: "not_owner" },
            { ref: { kind: "folder", id: 3 }, reason: "subtree_too_large" },
        ]),
    );
    expect(writes("/files/trash")[0]!.body).toEqual({
        items: [{ fileID: 100, collectionID: 2 }],
    });
    await settle();
    expect(
        model()
            .trash.map((t) => t.file.id)
            .sort(),
    ).toEqual([100, 101, 102]);
    expect(model().folders.has(4)).toBe(false);
});

test("restores files right after trashing them (undo)", async () => {
    await api.trashItems([
        { kind: "file", id: 100 },
        { kind: "file", id: 101 },
    ]);
    const result = await api.restoreFiles([100, 101], 4);
    expect(result.renamed).toEqual([
        { ref: { kind: "file", id: 101 }, name: "report (1).pdf" },
    ]);
    const [restore] = bodiesOf<{
        collectionID: number;
        files: {
            id: number;
            encryptedKey: string;
            keyDecryptionNonce: string;
        }[];
    }>(requests, "/collections/restore-files");
    expect(restore!.collectionID).toBe(4);
    for (const f of restore!.files)
        expect(
            await openBox(
                f.encryptedKey,
                f.keyDecryptionNonce,
                collectionKeys.get(4)!,
            ),
        ).toBe(fileKeys.get(f.id));
    await settle();
    expect(model().trash).toEqual([]);
    expect(names(contents(4).files)).toEqual([
        "notes.txt",
        "report (1).pdf",
        "report.pdf",
    ]);
});

test("restoring a file that's no longer in the trash still restores the others", async () => {
    await api.trashItems([
        { kind: "file", id: 100 },
        { kind: "file", id: 102 },
    ]);
    await settle();
    // Another device restores one of them.
    const entry = museum.trash.find((t) => t.file.id == 100)!;
    entry.isRestored = true;
    entry.updatedAt = museum.now();

    const result = await api.restoreFiles([100, 102], 3);
    expect(result.failed).toEqual([
        { ref: { kind: "file", id: 100 }, reason: "not_found" },
    ]);
    await settle();
    expect(names(contents(3).files)).toEqual(["notes.txt"]);
});

test("deletes forever and empties what this device has seen of the trash", async () => {
    await api.trashItems([
        { kind: "file", id: 100 },
        { kind: "file", id: 102 },
    ]);
    await settle();
    const seen = Math.max(...model().trash.map((t) => t.updatedAt));
    await api.deleteForever([100]);
    expect(writes("/trash/delete")[0]!.body).toEqual({ fileIDs: [100] });
    await api.emptyTrash();
    expect(writes("/trash/empty")[0]!.body).toEqual({ lastUpdatedAt: seen });
    await settle();
    expect(model().trash).toEqual([]);
});

test("serialises folder deletes, marking them deleting from when they're queued", async () => {
    let active = 0;
    let maxActive = 0;
    let deletingAtFirstDelete: number[] | undefined;
    let isPendingAtFirstDelete: boolean | undefined;
    museum.overrides.unshift((r) => {
        if (r.method != "DELETE") return undefined;
        maxActive = Math.max(maxActive, ++active);
        deletingAtFirstDelete ??= [...api.deletingFolderIDs(state())].sort();
        isPendingAtFirstDelete ??= api.pendingItemKeys(state()).has("folder:3");
        return undefined;
    });
    const handle = museum.handle;
    const fake = fakeFetch(async (r) => {
        const response = await handle(r);
        if (r.method == "DELETE") {
            await new Promise((resolve) => setTimeout(resolve, 5));
            active--;
        }
        return response;
    });
    vi.stubGlobal("fetch", fake.fetch);

    const result = await api.trashItems([
        { kind: "folder", id: 2 },
        { kind: "folder", id: 4 },
    ]);
    expect(deletingAtFirstDelete).toEqual([2, 3, 4]);
    expect(isPendingAtFirstDelete).toBe(true);
    expect(result.deletedFolderIDs.sort()).toEqual([2, 4]);
    expect(maxActive).toBe(1);
});

test("keeps a deleted folder's files pending until they reach the trash", async () => {
    // The server trashes the files a while after deleting the folders.
    let isHolding = true;
    let held: FakeTrashEntry[] = [];
    museum.overrides.unshift((r) => {
        if (r.method != "DELETE" || !isHolding) return undefined;
        isHolding = false;
        const response = museum.handle(r) as FakeResponse;
        held = museum.trash;
        museum.trash = [];
        return response;
    });
    await api.trashItems([{ kind: "folder", id: 4 }]);
    await settle();
    expect(model().folders.has(4)).toBe(false);
    expect(state().pendingTrash).toMatchObject([
        { folderID: 4, fileIDs: [101, 102] },
    ]);
    expect(api.deletingFolderIDs(state()).has(4)).toBe(true);

    // Kept across restarts.
    await loadServices();
    await sync.syncDrive();
    expect(state().pendingTrash).toMatchObject([
        { folderID: 4, fileIDs: [101, 102] },
    ]);

    museum.trash = held;
    await sync.syncDrive();
    expect(state().pendingTrash).toEqual([]);
    expect(
        model()
            .trash.map((t) => t.file.id)
            .sort(),
    ).toEqual([101, 102]);
});

test("retries a delete whose response was lost", async () => {
    let drops = 1;
    museum.dropResponse = (r) => r.method == "DELETE" && drops-- > 0;
    const result = await api.trashItems([{ kind: "folder", id: 4 }]);
    expect(result).toMatchObject({ deletedFolderIDs: [4], failed: [] });
    expect(writes("/collections/v4/4")).toHaveLength(2);
});

test("reports offline operations, and an expired session to the sync state", async () => {
    vi.stubGlobal("fetch", () => Promise.reject(new TypeError("offline")));
    const error = await api.renameFile(100, "x.pdf").catch((e: unknown) => e);
    expect(api.driveErrorReason(error)).toBe("offline");
    await settle();
    expect(state().pendingOps).toHaveLength(1);
    vi.stubGlobal("fetch", fakeFetch(museum.handle).fetch);
    await settle();
    await vi.waitFor(() => expect(state().pendingOps).toEqual([]));

    vi.stubGlobal("fetch", fakeFetch(() => ({ status: 401, json: {} })).fetch);
    await expect(api.renameFile(100, "x.pdf")).rejects.toMatchObject({
        reason: "unauthorized",
    });
    expect(state().sync).toMatchObject({
        status: "error",
        error: "unauthorized",
    });
    expect(api.driveErrorReason(new Error("?"))).toBe("unknown");
});

test("creates the root collection once, on first use", async () => {
    env.db.current = createFakeDB();
    museum.collections.delete(1);
    await loadServices();
    await sync.syncDrive();
    expect(model().rootCollectionID).toBeUndefined();

    const [a, b] = await Promise.all([
        api.uploadTarget(null),
        api.uploadTarget(null),
    ]);
    expect([a.collectionID, b.collectionID]).toEqual([5000, 5000]);
    expect(writes("/collections")).toHaveLength(1);
    expect(model().rootCollectionID).toBe(5000);
    expect(a.collectionKey).toHaveLength(44);
});

test("ingests files committed by an upload before the next sync", async () => {
    const { remote } = await makeFile({
        id: 120,
        collectionID: 3,
        collectionKey: collectionKeys.get(3)!,
        title: "upload.bin",
        updationTime: museum.now(),
    });
    const [file] = await api.ingestRemoteFiles([remote]);
    expect(file).toMatchObject({ id: 120, name: "upload.bin", folderID: 3 });
    expect(names(contents(3).files)).toEqual(["upload.bin"]);

    museum.putFile(3, remote);
    museum.putCollection({
        ...museum.collections.get(3)!,
        updationTime: museum.now(),
    });
    await sync.syncDrive();
    expect(model().files.get(120)).toBe(file);
});

test("exposes file key material for transfers", async () => {
    expect(await api.fileKeyMaterial(102)).toMatchObject({
        fileID: 102,
        collectionID: 4,
        fileKey: fileKeys.get(102),
        fileDecryptionHeader: "file-header",
        thumbnailDecryptionHeader: "thumb-header",
    });
});

test("writes nothing when the server isn't confirmed to support Drive", async () => {
    env.ensureDriveServerSupported.mockRejectedValue(new Error("unsupported"));
    await expect(api.renameFolder(2, "x")).rejects.toThrow("unsupported");
    await expect(api.uploadTarget(2)).rejects.toThrow("unsupported");
    await expect(
        api.moveItems([{ kind: "file", id: 100 }], 4),
    ).rejects.toThrow();
    expect(requests).toEqual([]);
    env.ensureDriveServerSupported.mockReset();
    env.ensureDriveServerSupported.mockResolvedValue(undefined);
});

// Delays the responses to requests for which `delayOf` returns > 0 ms.
const delayResponses = (delayOf: (r: FakeRequest) => number) => {
    const fake = fakeFetch(async (r) => {
        const delay = delayOf(r);
        if (delay) await new Promise((resolve) => setTimeout(resolve, delay));
        return museum.handle(r);
    });
    requests = fake.requests;
    vi.stubGlobal("fetch", fake.fetch);
};

test("runs operations on the same item in the order they were called", async () => {
    const events: string[] = [];
    let n = 0;
    delayResponses((r) => {
        if (!r.path.startsWith("/files/") || r.method != "PUT") return 0;
        const i = ++n;
        events.push(`start ${i}`);
        setTimeout(() => events.push(`end ${i}`), i == 2 ? 60 : 10);
        return i == 2 ? 61 : 11;
    });
    const starring = api.setStarred({ kind: "file", id: 100 }, true);
    await new Promise((resolve) => setTimeout(resolve, 5));
    const renames = [
        api.renameFile(100, "a.pdf"),
        api.renameFile(100, "b.pdf"),
    ];
    expect(await Promise.all([starring, ...renames])).toEqual([
        undefined,
        "a.pdf",
        "b.pdf",
    ]);
    expect(events).toEqual([
        "start 1",
        "end 1",
        "start 2",
        "end 2",
        "start 3",
        "end 3",
    ]);
    await settle();
    expect(model().files.get(100)).toMatchObject({
        name: "b.pdf",
        starred: true,
    });
});

test("an operation doesn't fail on a delete queued after it", async () => {
    delayResponses((r) => (r.path == "/collections/move-files" ? 20 : 0));
    const move = api.moveItems([{ kind: "file", id: 102 }], 3);
    const trash = api.trashItems([{ kind: "folder", id: 3 }]);
    expect(await move).toEqual({ renamed: [], failed: [] });
    expect(await trash).toMatchObject({ deletedFolderIDs: [3], failed: [] });
    const paths = requests.map((r) => r.path);
    expect(paths.indexOf("/collections/move-files")).toBeLessThan(
        paths.indexOf("/collections/v4/3"),
    );
});

test("keeps an operation pending until a sync after it succeeds", async () => {
    let failSyncs = true;
    museum.overrides.push((r) =>
        failSyncs && r.path == "/collections/v2"
            ? { status: 500, json: {} }
            : undefined,
    );
    const id = await api.createFolder(null, "New");
    await settle();
    expect(state().pendingOps).toHaveLength(1);
    // Its name stays reserved.
    const other = await api.createFolder(null, "New");
    expect(model().folders.get(other)?.name).toBe("New (1)");

    failSyncs = false;
    await settle();
    await vi.waitFor(() => expect(state().pendingOps).toEqual([]));
    expect(model().folders.get(id)?.name).toBe("New");
});

test("syncs after an operation that failed locally after sending a request", async () => {
    museum.overrides.push((r) =>
        r.path == "/collections/move-files"
            ? { status: 500, json: {} }
            : undefined,
    );
    await expect(
        api.moveItems(
            [
                { kind: "file", id: 999 },
                { kind: "file", id: 102 },
            ],
            3,
        ),
    ).rejects.toMatchObject({ reason: "not_found" });
    await vi.waitFor(() => expect(state().pendingOps).toEqual([]));
    const paths = requests.map((r) => r.path);
    expect(paths.lastIndexOf("/collections/v2")).toBeGreaterThan(
        paths.indexOf("/collections/move-files"),
    );
});

test("restores the files whose keys open when others' don't", async () => {
    await api.trashItems([{ kind: "file", id: 102 }]);
    await settle();
    // A trash entry for a live file, whose key no longer opens.
    const live = museum.fileRow(4, 101)!;
    museum.trash.push({
        file: { ...live, encryptedKey: "AAAA", updationTime: museum.now() },
        isDeleted: false,
        isRestored: false,
        deleteBy: 0,
        updatedAt: museum.now(),
    });
    await settle();

    const result = await api.restoreFiles([101, 102], 3);
    expect(result.failed).toEqual([
        { ref: { kind: "file", id: 101 }, reason: "unknown" },
    ]);
    await settle();
    expect(names(contents(3).files)).toEqual(["notes.txt"]);
});

test("doesn't show sync progress for the syncs after operations", async () => {
    const progress: unknown[] = [];
    api.subscribeDriveState(() => progress.push(state().sync.progress));
    await api.renameFolder(2, "Renamed");
    await vi.waitFor(() => expect(state().pendingOps).toEqual([]));
    expect(model().folders.get(2)?.name).toBe("Renamed");
    expect(progress.filter(Boolean)).toEqual([]);
});
