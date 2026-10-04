import { authenticatedRequestHeaders } from "ente-base/http";
import { apiURL } from "ente-base/origins";
import type { z } from "zod";
import { driveApiErrorFromResponse, DriveNetworkError } from "./errors";

const defaultTimeoutMs = 60 * 1000;

let writesSent = 0;

// How many requests that may change server state were made so far.
export const writeRequestCount = () => writesSent;

interface DriveRequestOptions {
    query?: Record<string, string | number | boolean>;
    body?: unknown;
    timeoutMs?: number;
    signal?: AbortSignal;
}

const send = async (
    method: string,
    path: string,
    {
        query,
        body,
        timeoutMs = defaultTimeoutMs,
        signal,
    }: DriveRequestOptions = {},
) => {
    if (method != "GET") writesSent++;
    const url = await apiURL(path, query);
    const headers = {
        ...(await authenticatedRequestHeaders()),
        ...(body !== undefined && { "Content-Type": "application/json" }),
    };
    const timeout = AbortSignal.timeout(timeoutMs);
    let res: Response;
    try {
        res = await fetch(url, {
            method,
            headers,
            ...(body !== undefined && { body: JSON.stringify(body) }),
            signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
        });
    } catch (e) {
        if (signal?.aborted) throw e;
        throw new DriveNetworkError(e);
    }
    if (!res.ok) throw await driveApiErrorFromResponse(res, path);
    return res;
};

export const driveRequest = async (
    method: "GET" | "POST" | "PUT" | "DELETE",
    path: string,
    options?: DriveRequestOptions,
): Promise<void> => {
    await (await send(method, path, options)).body?.cancel();
};

export const driveRequestJSON = async <T extends z.ZodType>(
    method: "GET" | "POST" | "PUT",
    path: string,
    schema: T,
    options?: DriveRequestOptions,
): Promise<z.infer<T>> => {
    const res = await send(method, path, options);
    let json: unknown;
    try {
        json = await res.json();
    } catch (e) {
        if (e instanceof SyntaxError) throw e;
        throw new DriveNetworkError(e);
    }
    return schema.parse(json);
};
