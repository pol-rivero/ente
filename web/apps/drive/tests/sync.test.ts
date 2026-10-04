import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { env } from "./environment";
import { FakeMuseum } from "./fake-museum";
import {
    createFakeDB,
    deletedFileRow,
    fakeFetch,
    keys,
    makeCollection,
    makeFile,
    setupKeys,
} from "./helpers";

let museum: FakeMuseum;
let requests: ReturnType<typeof fakeFetch>["requests"];
let engine: typeof import("../src/services/sync");
let store: typeof import("../src/services/store");
let selectors: typeof import("../src/services/model/selectors");

const loadEngine = async () => {
    vi.resetModules();
    engine = await import("../src/services/sync");
    store = await import("../src/services/store");
    selectors = await import("../src/services/model/selectors");
};

beforeEach(async () => {
    await setupKeys();
    env.db.current = createFakeDB();
    museum = new FakeMuseum();
    const fake = fakeFetch(museum.handle);
    requests = fake.requests;
    vi.stubGlobal("fetch", fake.fetch);
    await loadEngine();
});

afterEach(() => {
    vi.unstubAllGlobals();
});

const db = () => env.db.current!;
const model = () => store.driveState().model;
const sync = () => store.driveState().sync;
const contents = (id: number | null) => selectors.contentsOf(model(), id);

const diffRequests = (collectionID?: number) =>
    requests.filter(
        (r) =>
            r.path == "/collections/v2/diff" &&
            (collectionID === undefined ||
                r.query.get("collectionID") == String(collectionID)),
    );

const seed = async () => {
    const root = await makeCollection({
        id: 1,
        name: "Uncategorized",
        type: "uncategorized",
        updationTime: 100,
    });
    const docs = await makeCollection({
        id: 2,
        name: "Docs",
        updationTime: 200,
    });
    museum.putCollection(root.remote);
    museum.putCollection(docs.remote);
    const times = [110, 120, 120, 120, 130];
    for (const [i, updationTime] of times.entries()) {
        const { remote } = await makeFile({
            id: 100 + i,
            collectionID: 2,
            collectionKey: docs.key,
            title: `doc-${i}.txt`,
            updationTime,
        });
        museum.putFile(2, remote);
    }
    return { root, docs };
};

const names = (items: readonly { name: string }[]) =>
    items.map((i) => i.name).sort();

const encryptName = async (key: string, name: string) => {
    const { encryptBoxBytes } = await import("ente-drive-wasm");
    return encryptBoxBytes(new TextEncoder().encode(name), key);
};

test("pages file diffs until hasMore is false, never splitting ties", async () => {
    await seed();
    museum.pageSize = 1;
    await engine.syncDrive();

    expect(sync().status).toBe("idle");
    expect(names(contents(2).files)).toEqual([
        "doc-0.txt",
        "doc-1.txt",
        "doc-2.txt",
        "doc-3.txt",
        "doc-4.txt",
    ]);
    expect(diffRequests(2).map((r) => r.query.get("sinceTime"))).toEqual([
        "0",
        "110",
        "120",
    ]);
    expect(db().meta.get("collectionSinceTime:2")).toBe(130);
    expect(db().meta.get("collectionsSinceTime")).toBe(200);
    for (const r of requests)
        expect(r.headers.get("X-Client-Package")).toBe("io.ente.drive.web");
});

const sinceTimes = (path: string) =>
    requests
        .filter((r) => r.path == path)
        .map((r) => Number(r.query.get("sinceTime")));

const later = (ms: number) => {
    const now = Date.now() + ms;
    vi.spyOn(Date, "now").mockReturnValue(now);
};

test("fetches recent rows again after operations, and once late commits have landed", async () => {
    const { docs } = await seed();
    await engine.syncDrive();
    const before = model();

    // Committed after the diff that returned the row at 130, but with an
    // earlier updationTime.
    const addLate = async (id: number, updationTime: number) => {
        const { remote } = await makeFile({
            id,
            collectionID: 2,
            collectionKey: docs.key,
            title: `late-${id}.txt`,
            updationTime,
        });
        museum.putFile(2, remote);
    };
    await addLate(200, 125);
    requests.length = 0;
    await engine.syncDrive();
    expect(diffRequests(2)).toHaveLength(0);

    await engine.syncAfterOperation();
    expect(sinceTimes("/collections/v2/diff")[0]).toBeLessThan(125);
    expect(names(contents(2).files)).toContain("late-200.txt");
    expect(model()).not.toBe(before);

    await addLate(201, 126);
    later(31 * 1000);
    requests.length = 0;
    await engine.syncDrive();
    expect(sinceTimes("/collections/v2/diff")[0]).toBeLessThan(126);
    expect(names(contents(2).files)).toContain("late-201.txt");

    // Re-fetching rows that didn't change left the model as it was, and the
    // recheck is done.
    const after = model();
    requests.length = 0;
    await engine.syncDrive();
    expect(diffRequests(2)).toHaveLength(0);
    expect(model()).toBe(after);
    vi.restoreAllMocks();
});

test("fetches collections and trash again only once after they changed", async () => {
    const { docs } = await seed();
    const { remote: trashed } = await makeFile({
        id: 300,
        collectionID: 2,
        collectionKey: docs.key,
        title: "gone.txt",
    });
    museum.trash.push({
        file: trashed,
        isDeleted: false,
        isRestored: false,
        deleteBy: 9,
        updatedAt: 300,
    });
    await engine.syncDrive();
    const first = model();
    const writeCount = db().writes.length;

    requests.length = 0;
    await engine.syncDrive();
    await engine.syncDrive();
    expect(sinceTimes("/collections/v2")).toEqual([200, 200]);
    expect(sinceTimes("/trash/v2/diff")).toEqual([300, 300]);

    later(31 * 1000);
    requests.length = 0;
    await engine.syncDrive();
    await engine.syncDrive();
    expect(sinceTimes("/collections/v2")).toEqual([0, 200]);
    expect(sinceTimes("/trash/v2/diff")).toEqual([0, 300]);
    expect(model()).toBe(first);
    const recordWrites = db()
        .writes.slice(writeCount)
        .filter((w) => w.putCollections?.length || w.putTrash?.length);
    expect(recordWrites).toEqual([]);
    vi.restoreAllMocks();
});

test("a deleted folder fetched again writes nothing", async () => {
    await seed();
    await engine.syncDrive();
    void museum.handle({
        method: "DELETE",
        path: "/collections/v4/2",
        query: new URLSearchParams("recursive=true&keepFiles=false"),
        body: undefined,
        headers: new Headers(),
    });
    await engine.syncDrive();
    expect(model().folders.has(2)).toBe(false);

    const writeCount = db().writes.length;
    await engine.syncAfterOperation();
    await engine.syncAfterOperation();
    expect(sinceTimes("/collections/v2").at(-1)).toBe(0);
    expect(db().writes.slice(writeCount)).toEqual([]);
});

test("applies removals, and doesn't re-diff a folder that changed without file changes", async () => {
    const { docs } = await seed();
    await engine.syncDrive();

    museum.putFile(2, deletedFileRow(100, 2, 300));
    museum.putCollection({ ...docs.remote, updationTime: 300 });
    await engine.syncDrive();
    expect(contents(2).files).toHaveLength(4);

    const name = await encryptName(docs.key, "Documents");
    museum.putCollection({
        ...docs.remote,
        encryptedName: name.encryptedData,
        nameDecryptionNonce: name.nonce,
        updationTime: 400,
    });
    vi.spyOn(Date, "now").mockReturnValue(Date.now() + 60 * 60 * 1000);
    requests.length = 0;
    await engine.syncDrive();
    expect(diffRequests(2)).toHaveLength(1);
    expect(model().folders.get(2)?.name).toBe("Documents");

    requests.length = 0;
    await engine.syncDrive();
    expect(diffRequests(2)).toHaveLength(0);
    vi.restoreAllMocks();
});

test("drops deleted folders and their files, keeping their keys for trash", async () => {
    const { docs } = await seed();
    await engine.syncDrive();

    museum.putCollection({
        ...docs.remote,
        isDeleted: true,
        updationTime: 500,
    });
    const { remote: trashed } = await makeFile({
        id: 100,
        collectionID: 2,
        collectionKey: docs.key,
        title: "doc-0.txt",
    });
    museum.trash.push({
        file: trashed,
        isDeleted: false,
        isRestored: false,
        deleteBy: 9,
        updatedAt: 510,
    });
    await engine.syncDrive();

    expect(model().folders.has(2)).toBe(false);
    expect(model().files.size).toBe(0);
    expect([...db().files.keys()]).toEqual([]);
    expect(db().collections.get(2)?.isDeleted).toBe(true);
    expect(model().trash.map((t) => t.file.name)).toEqual(["doc-0.txt"]);

    museum.trash = [{ ...museum.trash[0]!, isRestored: true, updatedAt: 520 }];
    await engine.syncDrive();
    expect(model().trash).toEqual([]);
    expect(db().meta.get("trashSinceTime")).toBe(520);
});

test("a 404 file diff drops the folder locally", async () => {
    const { docs } = await seed();
    museum.deletedCollectionIDs.add(2);
    await engine.syncDrive();
    expect(model().folders.has(2)).toBe(false);
    expect(sync().status).toBe("idle");
    expect(db().collections.get(2)).toMatchObject({
        id: docs.remote.id,
        isDeleted: true,
    });
});

test("a folder whose diff fails doesn't hold back the others", async () => {
    await seed();
    const other = await makeCollection({
        id: 3,
        name: "Other",
        updationTime: 300,
    });
    museum.putCollection(other.remote);
    museum.overrides.push((r) =>
        r.path == "/collections/v2/diff" && r.query.get("collectionID") == "2"
            ? { status: 500, json: {} }
            : undefined,
    );
    museum.putFile(
        3,
        (
            await makeFile({
                id: 300,
                collectionID: 3,
                collectionKey: other.key,
                title: "fine.txt",
                updationTime: 310,
            })
        ).remote,
    );
    await engine.syncDrive();

    expect(sync()).toMatchObject({ status: "idle", failedFolderIDs: [2] });
    expect(names(contents(3).files)).toEqual(["fine.txt"]);
    expect(names(contents(null).folders)).toEqual(["Docs", "Other"]);
    expect(db().meta.get("collectionSinceTime:2")).toBeUndefined();

    museum.overrides = [];
    await engine.syncDrive();
    expect(sync().failedFolderIDs).toEqual([]);
    expect(contents(2).files).toHaveLength(5);
});

test("publishes folders before their files arrive, and reports progress", async () => {
    await seed();
    const progress: unknown[] = [];
    const folderCounts: number[] = [];
    store.subscribeDriveState(() => {
        const s = store.driveState();
        progress.push(s.sync.progress);
        folderCounts.push(s.model.folders.size && s.model.files.size);
    });
    await engine.syncDrive();
    expect(folderCounts).toContain(0);
    expect(progress).toContainEqual({ done: 0, total: 2 });
    expect(progress).toContainEqual({ done: 2, total: 2 });
    expect(sync().progress).toBeUndefined();
});

test("doesn't rewrite a collection whose key keeps failing to open", async () => {
    const shared = await makeCollection({
        id: 5,
        name: "From Bob",
        ownerID: 7,
        updationTime: 500,
    });
    museum.putCollection(shared.remote);
    const { default: log } = await import("ente-base/log");
    vi.mocked(log.error).mockClear();
    keys.failingOpens = 1000;
    await engine.syncDrive();
    await engine.syncDrive();
    await engine.syncDrive();
    const puts = db().writes.filter((w) =>
        w.putCollections?.some((c) => c.id == 5),
    );
    expect(puts).toHaveLength(1);
    const decryptFailures = vi
        .mocked(log.error)
        .mock.calls.filter(
            ([m]) => m == "Failed to decrypt collection 5 (Error)",
        );
    expect(decryptFailures).toHaveLength(1);
});

test("doesn't advance past a collection whose key failed to open", async () => {
    const shared = await makeCollection({
        id: 5,
        name: "From Bob",
        ownerID: 7,
        updationTime: 500,
        sharees: [{ id: 1, email: "me@example.org", role: "COLLABORATOR" }],
    });
    museum.putCollection(shared.remote);
    keys.failingOpens = 1;
    await engine.syncDrive();
    expect(db().meta.get("collectionsSinceTime")).toBeLessThan(500);

    await engine.syncDrive();
    expect(model().folders.get(5)).toMatchObject({
        role: "COLLABORATOR",
        owner: { id: 7, email: "user7@example.org" },
    });
});

test("skips malformed records without failing the sync", async () => {
    await seed();
    museum.putFile(2, {
        id: 999,
        updationTime: 140,
        collectionID: 2,
        garbage: true,
    });
    museum.putCollection({ id: 3, updationTime: 150, owner: "bad" });
    await engine.syncDrive();
    expect(sync().status).toBe("idle");
    expect(contents(2).files).toHaveLength(5);
    expect(model().folders.has(3)).toBe(false);
});

test("hydrates from the local database before reaching the server", async () => {
    await seed();
    await engine.syncDrive();
    await loadEngine();
    vi.stubGlobal("fetch", () => Promise.reject(new TypeError("offline")));

    await engine.syncDrive();
    expect(sync()).toMatchObject({ isHydrated: true, status: "offline" });
    expect(names(contents(null).folders)).toEqual(["Docs"]);
    expect(contents(2).files).toHaveLength(5);
});

test("keeps working in memory when the local database is unusable", async () => {
    await seed();
    db().failures.load = true;
    await engine.syncDrive();
    expect(sync()).toMatchObject({ status: "idle", isPersistent: false });
    expect(contents(2).files).toHaveLength(5);
    expect(db().writes).toEqual([]);
});

test("stops syncing on an unsupported server or an expired session", async () => {
    await seed();
    env.ensureDriveServerSupported.mockRejectedValueOnce(
        Object.assign(new Error("unsupported"), {
            name: "drive_server_unsupported",
        }),
    );
    await engine.syncDrive();
    expect(sync()).toMatchObject({
        status: "error",
        error: "unsupported_server",
    });
    expect(requests).toEqual([]);

    museum.overrides.push(() => ({ status: 401, json: {} }));
    await engine.syncDrive();
    expect(sync()).toMatchObject({ status: "error", error: "unauthorized" });
});

test("runs one sync at a time and coalesces requests made meanwhile", async () => {
    await seed();
    await Promise.all([
        engine.syncDrive(),
        engine.syncDrive(),
        engine.syncDrive(),
    ]);
    expect(requests.filter((r) => r.path == "/collections/v2")).toHaveLength(2);
});

test("keeps the model and unchanged objects across syncs and status changes", async () => {
    const { docs } = await seed();
    await engine.syncDrive();
    const first = model();
    const root = selectors.contentsOf(first, null);
    await engine.syncDrive();
    expect(model()).toBe(first);
    expect(selectors.contentsOf(model(), null)).toBe(root);

    const name = await encryptName(docs.key, "Renamed");
    museum.putCollection({
        ...docs.remote,
        encryptedName: name.encryptedData,
        nameDecryptionNonce: name.nonce,
        updationTime: 900,
    });
    await engine.syncDrive();
    expect(model()).not.toBe(first);
    expect(model().folders.get(2)!.name).toBe("Renamed");
    expect(model().folders.get(1)).toBe(first.folders.get(1));
    expect(model().files).toBe(first.files);
});

test("clearing local state empties the store and database", async () => {
    await seed();
    await engine.syncDrive();
    await engine.clearDriveLocalState();
    expect(model().folders.size).toBe(0);
    expect(db().collections.size).toBe(0);
});

test("never shows a file half moved into a folder that was empty", async () => {
    const { docs } = await seed();
    const empty = await makeCollection({
        id: 3,
        name: "Empty",
        updationTime: 250,
    });
    museum.putCollection(empty.remote);
    await engine.syncDrive();
    const mirror = await import("../src/services/mirror");

    museum.putFile(2, deletedFileRow(100, 2, 400));
    const { remote } = await makeFile({
        id: 100,
        collectionID: 3,
        collectionKey: empty.key,
        title: "doc-0.txt",
        updationTime: 400,
    });
    museum.putFile(3, remote);
    museum.putCollection({ ...docs.remote, updationTime: 400 });
    museum.putCollection({ ...empty.remote, updationTime: 400 });
    const sleep = (ms: number) =>
        new Promise((resolve) => setTimeout(resolve, ms));
    vi.stubGlobal(
        "fetch",
        fakeFetch(async (r) => {
            if (r.query.get("collectionID") == "3") {
                await sleep(30);
                // Another publisher (e.g. an upload's ingest) meanwhile.
                void mirror.publishMirror();
                await sleep(30);
            }
            return museum.handle(r);
        }).fetch,
    );
    const shown: boolean[] = [];
    store.subscribeDriveState(() => shown.push(model().files.has(100)));
    await engine.syncDrive();
    expect(model().files.get(100)?.folderID).toBe(3);
    expect(shown).not.toContain(false);
});

test("a reload of what another tab wrote keeps local writes made meanwhile", async () => {
    const { docs } = await seed();
    await engine.syncDrive();
    const mirror = await import("../src/services/mirror");
    const ingest = await import("../src/services/ingest");
    const load = db().load;
    db().load = async () => {
        const snapshot = await load();
        await new Promise((resolve) => setTimeout(resolve, 20));
        return snapshot;
    };
    const { remote } = await makeFile({
        id: 120,
        collectionID: 2,
        collectionKey: docs.key,
        title: "uploaded.txt",
        updationTime: 500,
    });
    await Promise.all([
        mirror.reloadMirror(1),
        ingest.ingestRemoteFiles([remote]),
    ]);
    await mirror.publishMirror();
    expect(names(contents(2).files)).toContain("uploaded.txt");
});
