import { isHTTP401Error } from "ente-base/http";

// Museum answers errors in several shapes: `{code, message}` from typed API
// errors, `{}` from mapped sentinel errors, `{"error": ...}` from middleware,
// and a plain-text 404 for routes the server doesn't have.
export type DriveApiErrorBody = "code" | "empty" | "middleware" | "text";

export class DriveApiError extends Error {
    readonly status: number;
    readonly code: string | undefined;
    readonly body: DriveApiErrorBody;

    constructor(
        status: number,
        code: string | undefined,
        body: DriveApiErrorBody,
        path: string,
    ) {
        super(`HTTP ${status}${code ? ` ${code}` : ""} (${path})`);
        this.name = "DriveApiError";
        this.status = status;
        this.code = code;
        this.body = body;
    }
}

export const driveApiErrorFromResponse = async (
    res: Response,
    path: string,
): Promise<DriveApiError> => {
    let text = "";
    try {
        text = await res.text();
    } catch {
        // An unreadable body carries no code.
    }
    const isJSON = !!res.headers.get("Content-Type")?.includes("json");
    let parsed: unknown;
    if (isJSON || text.trimStart().startsWith("{")) {
        try {
            parsed = JSON.parse(text);
        } catch {
            parsed = undefined;
        }
    }
    if (parsed && typeof parsed == "object") {
        const { code, error } = parsed as Record<string, unknown>;
        if (typeof code == "string" && code)
            return new DriveApiError(res.status, code, "code", path);
        if (error !== undefined)
            return new DriveApiError(res.status, undefined, "middleware", path);
        return new DriveApiError(res.status, undefined, "empty", path);
    }
    return new DriveApiError(res.status, undefined, "text", path);
};

export const isDriveApiError = (
    e: unknown,
    status?: number,
    code?: string,
): e is DriveApiError =>
    e instanceof DriveApiError &&
    (status === undefined || e.status == status) &&
    (code === undefined || e.code == code);

// A plain-text 404 means the route doesn't exist: the server predates it.
export const isRouteMissingError = (e: unknown): boolean =>
    isDriveApiError(e, 404) && e.body == "text";

export const isUnauthorizedError = (e: unknown): boolean =>
    isDriveApiError(e, 401) || isHTTP401Error(e);

export const isTreeBusyError = (e: unknown): boolean =>
    isDriveApiError(e, 503, "COLLECTION_TREE_BUSY");

export const isRateLimitedError = (e: unknown): boolean =>
    isDriveApiError(e, 429);

export const isVersionConflictError = (e: unknown): boolean =>
    isDriveApiError(e, 409);

// File membership operations into a deleted folder answer a plain `{}` 404
// or COLLECTION_DELETED depending on timing (client guide §6.2).
export const isTargetFolderGoneError = (e: unknown): boolean =>
    isDriveApiError(e, 404, "COLLECTION_DELETED") ||
    (isDriveApiError(e, 404) && e.body == "empty");

// The request didn't get a response: offline, unreachable or timed out. Its
// effect on the server is unknown.
export class DriveNetworkError extends Error {
    constructor(cause: unknown) {
        super("The request did not get a response", { cause });
        this.name = "DriveNetworkError";
    }
}

export const isNetworkError = (e: unknown): e is DriveNetworkError =>
    e instanceof DriveNetworkError;
