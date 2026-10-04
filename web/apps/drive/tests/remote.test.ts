import { expect, test, vi } from "vitest";
import {
    collectionDetails,
    storedCollection,
    storedFile,
    storedTrashItem,
} from "../src/services/records";
import {
    driveApiErrorFromResponse,
    isRouteMissingError,
    isTargetFolderGoneError,
    isTreeBusyError,
    isVersionConflictError,
} from "../src/services/remote/errors";
import {
    parseItems,
    RemoteCollection,
    RemoteFile,
    RemoteTrashItem,
} from "../src/services/remote/schemas";

vi.mock("ente-base/log", () => ({
    default: { warn: vi.fn(), info: vi.fn(), error: vi.fn() },
}));

const json = (status: number, body: unknown) =>
    new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json; charset=utf-8" },
    });

test.each([
    [
        json(409, { code: "HAS_CHILDREN", message: "x" }),
        409,
        "HAS_CHILDREN",
        "code",
    ],
    [json(404, {}), 404, undefined, "empty"],
    [json(429, { error: "too many requests" }), 429, undefined, "middleware"],
    [
        new Response("404 page not found", {
            status: 404,
            headers: { "Content-Type": "text/plain" },
        }),
        404,
        undefined,
        "text",
    ],
    [new Response("{}", { status: 426 }), 426, undefined, "empty"],
    [new Response(null, { status: 500 }), 500, undefined, "text"],
])("parses error responses (case %#)", async (res, status, code, body) => {
    const e = await driveApiErrorFromResponse(res, "/x");
    expect([e.status, e.code, e.body]).toEqual([status, code, body]);
});

test("error predicates distinguish shapes with the same status", async () => {
    const plain404 = await driveApiErrorFromResponse(
        new Response("404 page not found", { status: 404 }),
        "/collections/move-collection",
    );
    const empty404 = await driveApiErrorFromResponse(json(404, {}), "/x");
    const deleted = await driveApiErrorFromResponse(
        json(404, { code: "COLLECTION_DELETED" }),
        "/x",
    );
    const notFound = await driveApiErrorFromResponse(
        json(404, { code: "NOT_FOUND" }),
        "/x",
    );
    expect(isRouteMissingError(plain404)).toBe(true);
    expect(isRouteMissingError(empty404)).toBe(false);
    expect(isTargetFolderGoneError(empty404)).toBe(true);
    expect(isTargetFolderGoneError(deleted)).toBe(true);
    expect(isTargetFolderGoneError(notFound)).toBe(false);
    expect(isTargetFolderGoneError(plain404)).toBe(false);
    expect(
        isTreeBusyError(
            await driveApiErrorFromResponse(
                json(503, { code: "COLLECTION_TREE_BUSY" }),
                "/x",
            ),
        ),
    ).toBe(true);
    expect(
        isVersionConflictError(
            await driveApiErrorFromResponse(json(409, {}), "/x"),
        ),
    ).toBe(true);
});

const owned = {
    id: 10,
    owner: { id: 1, email: "", name: "", role: "OWNER" },
    encryptedKey: "ek",
    keyDecryptionNonce: "kn",
    name: "",
    encryptedName: "en",
    nameDecryptionNonce: "nn",
    type: "folder",
    attributes: {},
    sharees: [{ id: 2, email: "b@example.org", role: "viewer" }],
    publicURLs: [{ url: "https://l/c/t", validTill: 0, deviceLimit: 0 }],
    updationTime: 5,
    app: "drive",
    parentID: 9,
    parentEncryptedKey: "pk",
    parentKeyNonce: "pn",
    unknownFutureField: { nested: true },
};

test("decodes an owned collection", () => {
    const [c] = parseItems(RemoteCollection, [owned], "collection");
    const record = storedCollection(c!, undefined);
    expect(record).toMatchObject({
        id: 10,
        ownerID: 1,
        type: "folder",
        keyDecryptionNonce: "kn",
        parentID: 9,
        parentEncryptedKey: "pk",
        parentKeyNonce: "pn",
        isDeleted: false,
    });
    expect(record).not.toHaveProperty("unknownFutureField");
    expect(collectionDetails(c!)).toEqual({
        owner: { id: 1, email: undefined, role: "OWNER" },
        sharees: [{ id: 2, email: "b@example.org", role: "VIEWER" }],
        publicLinks: [
            {
                url: "https://l/c/t",
                validTill: 0,
                deviceLimit: 0,
                enableDownload: true,
                enableCollect: false,
                passwordEnabled: false,
            },
        ],
        legacyName: undefined,
    });
});

test("decodes shared collections, tombstones and deleted collections", () => {
    const shared = {
        id: 11,
        owner: { id: 2, email: "b@example.org", role: "OWNER" },
        encryptedKey: "sealed",
        encryptedName: "en",
        nameDecryptionNonce: "nn",
        type: "folder",
        sharees: null,
        publicURLs: null,
        updationTime: 6,
    };
    const tombstone = {
        id: 12,
        owner: { id: 2, email: "" },
        encryptedKey: "sealed",
        type: "folder",
        attributes: {},
        updationTime: 7,
        isDeleted: true,
    };
    const deleted = { ...owned, id: 13, isDeleted: true };
    const parsed = parseItems(
        RemoteCollection,
        [shared, tombstone, deleted, { id: "bad" }],
        "collection",
    );
    expect(parsed.map((c) => c.id)).toEqual([11, 12, 13]);
    const [s, t, d] = parsed.map((c) => storedCollection(c, undefined));
    expect(s).toMatchObject({ ownerID: 2, keyDecryptionNonce: undefined });
    expect(t).toMatchObject({ isDeleted: true, encryptedName: undefined });
    expect(d).toMatchObject({ isDeleted: true });
});

test("decodes live and scrubbed file rows, and trash items", () => {
    const live = {
        id: 100,
        ownerID: 1,
        collectionID: 10,
        collectionOwnerID: 1,
        encryptedKey: "fk",
        keyDecryptionNonce: "fn",
        file: { decryptionHeader: "fh", size: 0 },
        thumbnail: { decryptionHeader: "th" },
        metadata: { encryptedData: "md", decryptionHeader: "mh" },
        isDeleted: false,
        updationTime: 9,
        pubMagicMetadata: { version: 2, count: 1, data: "d", header: "h" },
        info: { fileSize: 1234, thumbSize: 10 },
    };
    const scrubbed = {
        id: 101,
        ownerID: 1,
        collectionID: 10,
        isDeleted: true,
        updationTime: 9,
        metadata: { encryptedData: "-" },
    };
    const [l, s] = parseItems(RemoteFile, [live, scrubbed], "file");
    expect(storedFile(l!)).toMatchObject({
        id: 100,
        fileDecryptionHeader: "fh",
        thumbnailDecryptionHeader: "th",
        encryptedSize: 1234,
        pubMagicMetadata: { version: 2, count: 1 },
    });
    expect(s!.isDeleted).toBe(true);
    expect(storedFile(s!)).toBeUndefined();

    const [trash] = parseItems(
        RemoteTrashItem,
        [
            {
                file: live,
                isDeleted: false,
                isRestored: false,
                deleteBy: 99,
                createdAt: 1,
                updatedAt: 2,
            },
        ],
        "trash item",
    );
    expect(storedTrashItem(trash!)).toMatchObject({
        id: 100,
        updatedAt: 2,
        deleteBy: 99,
    });
});
