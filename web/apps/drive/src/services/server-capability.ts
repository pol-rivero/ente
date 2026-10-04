import { namedError } from "ente-base/error";
import { authenticatedRequestHeaders, HTTPError } from "ente-base/http";
import log from "ente-base/log";
import { apiURL } from "ente-base/origins";
import { z } from "zod";
import { DriveNetworkError } from "./remote/errors";

// Servers that predate the Drive upload changes don't know Drive's client
// package and treat its requests as Photos requests, so writing to them would
// put Drive data into the user's Photos library. They also lack the
// `GET /files/uploads` route, which is what we probe for. Anything other than a
// well-formed answer from that route fails closed: callers retry or log out.

const UploadsResponse = z.object({
    uploads: z.array(z.unknown()),
    hasMore: z.boolean(),
});

let isSupported = false;
let inflightProbe: Promise<void> | undefined;

export const ensureDriveServerSupported = async () => {
    if (isSupported) return;
    inflightProbe ??= probeDriveServer().finally(() => {
        inflightProbe = undefined;
    });
    await inflightProbe;
};

// A server that accepts the connection but never answers would otherwise
// block every write and sync.
const probeTimeoutMs = 30 * 1000;

const probeDriveServer = async () => {
    const url = await apiURL("/files/uploads");
    const headers = await authenticatedRequestHeaders();
    let res: Response;
    try {
        res = await fetch(url, {
            headers,
            signal: AbortSignal.timeout(probeTimeoutMs),
        });
    } catch (e) {
        throw new DriveNetworkError(e);
    }

    if (res.status == 404 && !isJSONResponse(res)) {
        log.error("The server does not support Drive");
        throw namedError(
            "drive_server_unsupported",
            "The server does not support Drive",
        );
    }
    if (!res.ok) throw new HTTPError(res);

    const body: unknown = await res.json().catch(() => undefined);
    if (!UploadsResponse.safeParse(body).success) {
        throw new Error("Unexpected response when probing for Drive support");
    }
    isSupported = true;
};

const isJSONResponse = (res: Response) =>
    !!res.headers.get("Content-Type")?.includes("application/json");
