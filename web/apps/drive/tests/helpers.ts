import {
    encryptBlob,
    encryptBox,
    encryptBoxBytes,
    generateKey,
} from "ente-drive-wasm";
import type { DriveDBChanges, DriveDBSnapshot } from "../src/services/db";
import type {
    StoredCollection,
    StoredFile,
    StoredPendingTrash,
    StoredTrashItem,
} from "../src/services/records";
import { keys, userID } from "./keys";

export { keys, userID } from "./keys";

export const setupKeys = async () => {
    keys.masterKey = await generateKey();
    keys.shared.clear();
    keys.failingOpens = 0;
};

const utf8 = (s: string) => new TextEncoder().encode(s);

const encryptMagic = async (value: object, key: string, version = 1) => {
    const blob = await encryptBlob(utf8(JSON.stringify(value)), key);
    return {
        version,
        count: Object.keys(value).length,
        data: blob.encryptedData,
        header: blob.decryptionHeader,
    };
};

export interface CollectionSpec {
    id: number;
    name: string;
    type?: string;
    ownerID?: number;
    updationTime?: number;
    parent?: { id: number; key: string };
    // Claims `parent.id` but wraps the key with this other key instead.
    forgedParentKey?: string;
    magic?: object;
    sharees?: { id: number; email: string; role: string }[];
}

export const makeCollection = async (spec: CollectionSpec) => {
    const key = await generateKey();
    const ownerID = spec.ownerID ?? userID;
    const name = await encryptBoxBytes(utf8(spec.name), key);
    let keyFields: Record<string, string>;
    if (ownerID == userID) {
        const box = await encryptBox(key, keys.masterKey);
        keyFields = {
            encryptedKey: box.encryptedData,
            keyDecryptionNonce: box.nonce,
        };
    } else {
        const sealed = `sealed-${spec.id}-${Math.random()}`;
        keys.shared.set(sealed, key);
        keyFields = { encryptedKey: sealed };
    }
    const parentWrap = spec.parent
        ? await encryptBox(key, spec.forgedParentKey ?? spec.parent.key)
        : undefined;
    const remote = {
        id: spec.id,
        owner: {
            id: ownerID,
            email: ownerID == userID ? "" : `user${ownerID}@example.org`,
            role: "OWNER",
        },
        ...keyFields,
        encryptedName: name.encryptedData,
        nameDecryptionNonce: name.nonce,
        type: spec.type ?? "folder",
        attributes: {},
        sharees: spec.sharees ?? [],
        publicURLs: [],
        updationTime: spec.updationTime ?? 1000,
        app: "drive",
        ...(spec.magic && {
            magicMetadata: await encryptMagic(spec.magic, key),
        }),
        ...(spec.parent &&
            parentWrap && {
                parentID: spec.parent.id,
                parentEncryptedKey: parentWrap.encryptedData,
                parentKeyNonce: parentWrap.nonce,
            }),
    };
    return { remote, key };
};

export interface FileSpec {
    id: number;
    collectionID: number;
    collectionKey: string;
    title: string;
    ownerID?: number;
    updationTime?: number;
    metadata?: object;
    publicMagic?: object;
    publicMagicVersion?: number;
    privateMagic?: object;
    encryptedSize?: number;
    fileKey?: string;
}

export const makeFile = async (spec: FileSpec) => {
    const fileKey = spec.fileKey ?? (await generateKey());
    const box = await encryptBox(fileKey, spec.collectionKey);
    const metadata = await encryptBlob(
        utf8(
            JSON.stringify({
                title: spec.title,
                fileType: 3,
                creationTime: 1_700_000_000_000_000,
                modificationTime: 1_700_000_000_000_000,
                ...spec.metadata,
            }),
        ),
        fileKey,
    );
    const remote = {
        id: spec.id,
        ownerID: spec.ownerID ?? userID,
        collectionID: spec.collectionID,
        encryptedKey: box.encryptedData,
        keyDecryptionNonce: box.nonce,
        file: { decryptionHeader: "file-header" },
        thumbnail: { decryptionHeader: "thumb-header" },
        metadata: {
            encryptedData: metadata.encryptedData,
            decryptionHeader: metadata.decryptionHeader,
        },
        isDeleted: false,
        updationTime: spec.updationTime ?? 1000,
        ...(spec.publicMagic && {
            pubMagicMetadata: await encryptMagic(
                spec.publicMagic,
                fileKey,
                spec.publicMagicVersion,
            ),
        }),
        ...(spec.privateMagic && {
            magicMetadata: await encryptMagic(spec.privateMagic, fileKey),
        }),
        info: { fileSize: spec.encryptedSize ?? 1017 },
    };
    return { remote, fileKey };
};

export const deletedFileRow = (
    id: number,
    collectionID: number,
    updationTime: number,
) => ({
    id,
    ownerID: userID,
    collectionID,
    isDeleted: true,
    updationTime,
    metadata: { encryptedData: "-" },
});

// An in-memory stand-in for the IndexedDB module.
export const createFakeDB = () => {
    const collections = new Map<number, StoredCollection>();
    const files = new Map<string, StoredFile>();
    const trash = new Map<number, StoredTrashItem>();
    const pendingTrash = new Map<number, StoredPendingTrash>();
    const meta = new Map<string, number>();
    const writes: DriveDBChanges[] = [];
    const failures = { load: false, write: false };

    const load = (): Promise<DriveDBSnapshot> =>
        failures.load
            ? Promise.reject(new Error("IndexedDB is unusable"))
            : Promise.resolve({
                  collections: [...collections.values()],
                  files: [...files.values()],
                  trash: [...trash.values()],
                  pendingTrash: [...pendingTrash.values()],
                  meta: new Map(meta),
              });

    const write = (_: number, changes: DriveDBChanges) => {
        if (failures.write)
            return Promise.reject(new Error("IndexedDB is unusable"));
        writes.push(changes);
        const cleared = new Set(changes.clearCollections);
        for (const [key, f] of files)
            if (cleared.has(f.collectionID)) files.delete(key);
        for (const id of cleared)
            for (const key of [...meta.keys()])
                if (key.endsWith(`:${id}`)) meta.delete(key);
        for (const id of changes.deleteCollections ?? [])
            collections.delete(id);
        for (const c of changes.putCollections ?? []) collections.set(c.id, c);
        for (const [id, cid] of changes.deleteFiles ?? [])
            files.delete(`${id}:${cid}`);
        for (const f of changes.putFiles ?? [])
            files.set(`${f.id}:${f.collectionID}`, f);
        for (const id of changes.deleteTrash ?? []) trash.delete(id);
        for (const t of changes.putTrash ?? []) trash.set(t.id, t);
        for (const id of changes.deletePendingTrash ?? [])
            pendingTrash.delete(id);
        for (const p of changes.putPendingTrash ?? [])
            pendingTrash.set(p.folderID, p);
        for (const [k, v] of Object.entries(changes.meta ?? {})) meta.set(k, v);
        return Promise.resolve();
    };

    const clear = () => {
        collections.clear();
        files.clear();
        trash.clear();
        pendingTrash.clear();
        meta.clear();
        return Promise.resolve();
    };

    return {
        collections,
        files,
        trash,
        pendingTrash,
        meta,
        writes,
        failures,
        load,
        write,
        clear,
    };
};

export interface FakeRequest {
    method: string;
    path: string;
    query: URLSearchParams;
    body: unknown;
    headers: Headers;
}

export type FakeResponse =
    | { status?: number; json: unknown }
    | { status: number; text: string }
    | { status?: number; empty: true }
    // The server handled the request, but the response never arrived.
    | { dropped: true };

export const bodiesOf = <T>(requests: FakeRequest[], path: string) =>
    requests
        .filter((r) => r.path == path && r.method != "GET")
        .map((r) => r.body as T);

// A fetch stand-in that routes museum requests to `handle`. A handler that
// throws makes the request fail without a response.
export const fakeFetch = (
    handle: (request: FakeRequest) => FakeResponse | Promise<FakeResponse>,
) => {
    const requests: FakeRequest[] = [];
    const fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(
            typeof input == "string"
                ? input
                : input instanceof URL
                  ? input.href
                  : input.url,
        );
        const request: FakeRequest = {
            method: init?.method ?? "GET",
            path: url.pathname,
            query: url.searchParams,
            body:
                typeof init?.body == "string"
                    ? (JSON.parse(init.body) as unknown)
                    : undefined,
            headers: new Headers(init?.headers),
        };
        requests.push(request);
        const response = await handle(request);
        if ("dropped" in response) throw new TypeError("Network error");
        const status = response.status ?? 200;
        if ("text" in response)
            return new Response(response.text, {
                status,
                headers: { "Content-Type": "text/plain" },
            });
        if ("empty" in response) return new Response(null, { status });
        return new Response(JSON.stringify(response.json), {
            status,
            headers: { "Content-Type": "application/json" },
        });
    };
    return { fetch, requests };
};
