import { vi } from "vitest";
import type { createFakeDB } from "./helpers";

// Module mocks shared by the data-layer tests: a fake IndexedDB, a session
// for user 1 whose master key (and shared collection keys) live in ./keys,
// and requests that go to the stubbed `fetch`.

export const env = {
    db: { current: undefined as ReturnType<typeof createFakeDB> | undefined },
    ensureDriveServerSupported: vi.fn(() => Promise.resolve()),
};

vi.mock("ente-base/log", () => ({
    default: { warn: vi.fn(), info: vi.fn(), error: vi.fn() },
}));
vi.mock("ente-utils/promise", async (importOriginal) => ({
    ...(await importOriginal<typeof import("ente-utils/promise")>()),
    wait: () => Promise.resolve(),
}));
vi.mock("ente-base/app", () => ({
    clientPackageName: "io.ente.drive.web",
    desktopAppVersion: undefined,
    isDesktop: false,
}));
vi.mock("ente-base/token", () => ({
    ensureAuthToken: () => Promise.resolve("token"),
}));
vi.mock("ente-base/origins", () => ({
    apiURL: (path: string, query?: Record<string, string | number | boolean>) =>
        Promise.resolve(
            `http://museum${path}${query ? `?${new URLSearchParams(Object.entries(query).map(([k, v]) => [k, String(v)])).toString()}` : ""}`,
        ),
}));
vi.mock("ente-accounts/services/user", () => ({
    ensureLocalUser: () => ({ id: 1 }),
}));
vi.mock("../src/services/server-capability", () => ({
    ensureDriveServerSupported: env.ensureDriveServerSupported,
}));
vi.mock("../src/services/db", async (importOriginal) => ({
    ...(await importOriginal<typeof import("../src/services/db")>()),
    loadDriveDB: () => env.db.current!.load(),
    writeDriveDB: (userID: number, changes: object) =>
        env.db.current!.write(userID, changes),
    clearDriveDBs: () => env.db.current!.clear(),
}));
vi.mock("../src/services/account-keys", async () => {
    const { keys } = await import("./keys");
    return { masterKeyFromSession: () => Promise.resolve(keys.masterKey) };
});
vi.mock("../src/services/authenticated-session", () => ({
    ensureAuthenticatedSession: () => Promise.resolve({}),
}));
vi.mock("ente-drive-wasm", async (importOriginal) => {
    const actual = await importOriginal<typeof import("ente-drive-wasm")>();
    const { fakeOpenCollectionKey } = await import("./keys");
    return {
        ...actual,
        openCollectionKey: fakeOpenCollectionKey(actual.decryptBox),
    };
});
