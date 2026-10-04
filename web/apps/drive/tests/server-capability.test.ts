import { isNamedError } from "ente-base/error";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

vi.mock("ente-base/token", () => ({
    ensureAuthToken: () => Promise.resolve("token"),
}));
vi.mock("ente-base/origins", () => ({
    apiURL: (path: string) => Promise.resolve(`http://localhost:8080${path}`),
}));
vi.mock("ente-base/log", () => ({
    default: { warn: vi.fn(), error: vi.fn() },
}));

const fetch = vi.fn<typeof globalThis.fetch>();

let ensureDriveServerSupported: typeof import("../src/services/server-capability").ensureDriveServerSupported;
let isHTTP401Error: typeof import("ente-base/http").isHTTP401Error;

beforeEach(async () => {
    vi.stubEnv("appName", "drive");
    vi.stubGlobal("fetch", fetch);
    ({ ensureDriveServerSupported } =
        await import("../src/services/server-capability"));
    ({ isHTTP401Error } = await import("ente-base/http"));
});

afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
    vi.resetModules();
    fetch.mockReset();
});

const response = (status: number, body: string, contentType: string) => {
    const res = new Response(body, {
        status,
        headers: { "Content-Type": contentType },
    });
    Object.defineProperty(res, "url", {
        value: "http://localhost:8080/files/uploads",
    });
    return res;
};

const jsonResponse = (status: number, body: unknown) =>
    response(status, JSON.stringify(body), "application/json; charset=utf-8");

const uploadsResponse = () =>
    jsonResponse(200, { uploads: [], hasMore: false });

test("a Drive server is supported, and the result is cached", async () => {
    fetch.mockResolvedValue(uploadsResponse());

    await ensureDriveServerSupported();
    await ensureDriveServerSupported();

    expect(fetch).toHaveBeenCalledOnce();
    expect(fetch).toHaveBeenCalledWith("http://localhost:8080/files/uploads", {
        headers: {
            "X-Auth-Token": "token",
            "X-Client-Package": "io.ente.drive.web",
        },
        signal: expect.any(AbortSignal) as unknown,
    });
});

test("concurrent callers share a single probe", async () => {
    fetch.mockResolvedValue(uploadsResponse());

    await Promise.all([
        ensureDriveServerSupported(),
        ensureDriveServerSupported(),
        ensureDriveServerSupported(),
    ]);

    expect(fetch).toHaveBeenCalledOnce();
});

test("a server without the uploads route is unsupported", async () => {
    fetch.mockResolvedValue(response(404, "404 page not found", "text/plain"));

    const error = await ensureDriveServerSupported().catch((e: unknown) => e);

    expect(isNamedError(error, "drive_server_unsupported")).toBe(true);
});

test("an expired session surfaces the HTTP 401", async () => {
    fetch.mockResolvedValue(jsonResponse(401, { error: "invalid token" }));

    const error = await ensureDriveServerSupported().catch((e: unknown) => e);

    expect(isHTTP401Error(error)).toBe(true);
});

test("an unexpected 200 response is not treated as support", async () => {
    fetch
        .mockResolvedValueOnce(jsonResponse(200, { uploads: [] }))
        .mockResolvedValueOnce(response(200, "<html></html>", "text/html"))
        .mockResolvedValueOnce(uploadsResponse());

    await expect(ensureDriveServerSupported()).rejects.toThrow();
    await expect(ensureDriveServerSupported()).rejects.toThrow();
    await ensureDriveServerSupported();

    expect(fetch).toHaveBeenCalledTimes(3);
});

test("other failures are thrown, and the next call probes again", async () => {
    fetch
        .mockRejectedValueOnce(new TypeError("Failed to fetch"))
        .mockResolvedValueOnce(jsonResponse(500, {}))
        .mockResolvedValueOnce(jsonResponse(404, { code: "NOT_FOUND" }))
        .mockResolvedValueOnce(uploadsResponse());

    for (let i = 0; i < 3; i++) {
        const error = await ensureDriveServerSupported().catch(
            (e: unknown) => e,
        );
        expect(error).toBeInstanceOf(Error);
        expect(isNamedError(error, "drive_server_unsupported")).toBe(false);
    }
    await ensureDriveServerSupported();
    expect(fetch).toHaveBeenCalledTimes(4);
});
