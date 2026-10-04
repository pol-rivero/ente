import { afterEach, expect, test, vi } from "vitest";

const { openSession } = vi.hoisted(() => ({
    openSession: vi.fn(() =>
        Promise.resolve({ free: vi.fn(), updateAuthToken: vi.fn() }),
    ),
}));

vi.mock("ente-base/token", () => ({
    ensureAuthToken: () => Promise.resolve("token"),
}));
vi.mock("ente-base/origins", () => ({
    apiOrigin: () => Promise.resolve("http://localhost:8080"),
}));
vi.mock("ente-accounts/services/user", () => ({
    ensureSavedKeyAttributes: () => ({ publicKey: "public-key" }),
}));
vi.mock("ente-drive-wasm", () => ({ openSession }));
vi.mock("../src/services/account-keys", () => ({
    masterKeyFromSession: vi.fn(),
}));

afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
});

test("web requests identify as the Drive web app", async () => {
    vi.stubEnv("appName", "drive");
    const { authenticatedRequestHeaders, publicRequestHeaders } =
        await import("ente-base/http");

    expect(await authenticatedRequestHeaders()).toEqual({
        "X-Auth-Token": "token",
        "X-Client-Package": "io.ente.drive.web",
    });
    expect(publicRequestHeaders()).toEqual({
        "X-Client-Package": "io.ente.drive.web",
    });
});

test("the Rust session identifies as the Drive web app", async () => {
    vi.stubEnv("appName", "drive");
    const { openAuthenticatedSession } =
        await import("../src/services/authenticated-session");

    await openAuthenticatedSession(1, "token", "master-key");

    expect(openSession).toHaveBeenCalledWith(
        expect.objectContaining({
            baseUrl: "http://localhost:8080",
            clientPackage: "io.ente.drive.web",
            clientVersion: undefined,
        }),
    );
});
