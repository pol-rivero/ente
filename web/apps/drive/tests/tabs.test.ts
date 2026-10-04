import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { env } from "./environment";
import { FakeMuseum } from "./fake-museum";
import {
    createFakeDB,
    fakeFetch,
    makeCollection,
    makeFile,
    setupKeys,
    type FakeRequest,
} from "./helpers";

// Each tab is a separate instance of the data layer. They share the fake
// database, and this process's BroadcastChannel and Web Locks.

interface Tab {
    api: typeof import("../src/services/drive-api");
    sync: typeof import("../src/services/sync");
    start: () => void;
}

let museum: FakeMuseum;
let requests: FakeRequest[];
let delayOf: (r: FakeRequest) => number;
let visibility: DocumentVisibilityState;
let tabs: Tab[];

const openTab = async (): Promise<Tab> => {
    vi.resetModules();
    const api = await import("../src/services/drive-api");
    const sync = await import("../src/services/sync");
    const tab = { api, sync, start: () => void sync.startDriveSync() };
    tabs.push(tab);
    return tab;
};

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const show = () => {
    visibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
};

const collectionRequests = () =>
    requests.filter((r) => r.path == "/collections/v2");

beforeEach(async () => {
    await setupKeys();
    env.db.current = createFakeDB();
    museum = new FakeMuseum();
    delayOf = () => 0;
    const fake = fakeFetch(async (r) => {
        const delay = delayOf(r);
        if (delay) await sleep(delay);
        return museum.handle(r);
    });
    requests = fake.requests;
    vi.stubGlobal("fetch", fake.fetch);
    vi.stubGlobal(
        "window",
        Object.assign(new EventTarget(), {
            location: { replace: vi.fn() },
            // For the Wasm crypto, which looks for it on `window`.
            crypto: globalThis.crypto,
        }),
    );
    visibility = "visible";
    vi.stubGlobal(
        "document",
        Object.defineProperty(new EventTarget(), "visibilityState", {
            get: () => visibility,
        }),
    );
    tabs = [];

    const root = await makeCollection({
        id: 1,
        name: "Uncategorized",
        type: "uncategorized",
        updationTime: 100,
    });
    const a = await makeCollection({ id: 2, name: "A", updationTime: 200 });
    const b = await makeCollection({ id: 3, name: "B", updationTime: 300 });
    for (const c of [root, a, b]) museum.putCollection(c.remote);
    const { remote } = await makeFile({
        id: 100,
        collectionID: 2,
        collectionKey: a.key,
        title: "x.txt",
        updationTime: 150,
    });
    museum.putFile(2, remote);
});

afterEach(async () => {
    for (const tab of tabs) tab.sync.stopDriveSync();
    // Syncs still running must not write to the next test's database.
    await vi.waitFor(
        () =>
            expect(
                tabs.map((t) => t.api.driveState().sync.status),
            ).not.toContain("syncing"),
        { timeout: 5000 },
    );
    vi.unstubAllGlobals();
});

const deleteOnServer = (id: number) =>
    museum.handle({
        method: "DELETE",
        path: `/collections/v4/${id}`,
        query: new URLSearchParams("recursive=true&keepFiles=false"),
        body: undefined,
        headers: new Headers(),
    });

test("two idle tabs stay idle after a folder was deleted elsewhere", async () => {
    await deleteOnServer(3);
    visibility = "hidden";
    const a = await openTab();
    a.start();
    const b = await openTab();
    b.start();
    await sleep(1500);

    expect(collectionRequests()).toHaveLength(2);
    for (const tab of [a, b])
        expect([...tab.api.driveState().model.folders.keys()]).toEqual([1, 2]);
});

test("a tab shows what another tab wrote without syncing, once shown", async () => {
    const a = await openTab();
    await a.sync.syncDrive();
    const b = await openTab();
    b.start();
    await vi.waitFor(() =>
        expect(b.api.driveState().sync.lastSyncedAt).toBeDefined(),
    );

    requests.length = 0;
    await a.api.renameFolder(2, "Renamed");
    await vi.waitFor(() =>
        expect(b.api.driveState().model.folders.get(2)?.name).toBe("Renamed"),
    );
    const byA = requests.length;

    visibility = "hidden";
    await a.api.renameFolder(3, "Later");
    await a.sync.syncDrive();
    await sleep(50);
    expect(b.api.driveState().model.folders.get(3)?.name).toBe("B");

    show();
    await vi.waitFor(() =>
        expect(b.api.driveState().model.folders.get(3)?.name).toBe("Later"),
    );
    // Only A's operations and syncs reached the server.
    expect(collectionRequests().length).toBeLessThanOrEqual(byA + 2);
});

test("a tab shows the local state while another tab syncs", async () => {
    await (await openTab()).sync.syncDrive();
    const c = await makeCollection({ id: 4, name: "C", updationTime: 400 });
    museum.putCollection(c.remote);
    delayOf = (r) => (r.path == "/collections/v2/diff" ? 1000 : 0);

    const a = await openTab();
    a.start();
    await sleep(100);
    const b = await openTab();
    b.start();
    await vi.waitFor(
        () => expect(b.api.driveState().sync.isHydrated).toBe(true),
        { timeout: 500 },
    );
    expect(b.api.driveState().model.files.has(100)).toBe(true);
});

test("syncs without a lock when Web Locks fail", async () => {
    vi.stubGlobal("navigator", {
        locks: {
            request: () =>
                Promise.reject(new DOMException("Denied", "SecurityError")),
        },
    });
    const a = await openTab();
    await a.sync.syncDrive();
    expect(a.api.driveState().sync.status).toBe("idle");
    expect(a.api.driveState().model.files.has(100)).toBe(true);
});
