import type { FakeRequest, FakeResponse } from "./helpers";
import { userID } from "./keys";

type JSONValue = Record<string, unknown>;

interface Row extends JSONValue {
    id: number;
    updationTime: number;
}

interface MagicMetadata {
    version: number;
    count: number;
    data: string;
    header: string;
}

export interface FakeTrashEntry extends JSONValue {
    file: Row;
    isDeleted: boolean;
    isRestored: boolean;
    deleteBy: number;
    updatedAt: number;
}

interface FileItem {
    id: number;
    encryptedKey: string;
    keyDecryptionNonce: string;
}

const page = <T extends JSONValue>(
    rows: T[],
    sinceTime: number,
    pageSize: number,
    field: "updationTime" | "updatedAt",
) => {
    const time = (r: T) => r[field] as number;
    const pending = rows
        .filter((r) => time(r) > sinceTime)
        .sort((a, b) => time(a) - time(b));
    let end = Math.min(pageSize, pending.length);
    // Like museum, never split rows that share a timestamp.
    while (
        end > 0 &&
        end < pending.length &&
        time(pending[end]!) == time(pending[end - 1]!)
    )
        end++;
    return { diff: pending.slice(0, end), hasMore: end < pending.length };
};

const empty: FakeResponse = { empty: true };
const notFound: FakeResponse = { status: 404, json: {} };
const coded = (status: number, code: string): FakeResponse => ({
    status,
    json: { code, message: "" },
});

// An in-memory museum for the endpoints the data layer uses, modelled on the
// server's behaviour (see the client guide). Rows are stored as the server
// returns them.
export class FakeMuseum {
    collections = new Map<number, Row>();
    // Collection ID → rows (live and removed) of its files.
    files = new Map<number, Row[]>();
    trash: FakeTrashEntry[] = [];
    deletedCollectionIDs = new Set<number>();
    pageSize = 2500;
    nextID = 5000;
    // Consulted before the built-in handlers.
    overrides: ((r: FakeRequest) => FakeResponse | undefined)[] = [];
    // Requests to handle and then fail as if the response was lost.
    dropResponse: ((r: FakeRequest) => boolean) | undefined;

    private time = 0;

    // A server time after everything stored so far.
    now() {
        let max = this.time;
        for (const c of this.collections.values())
            max = Math.max(max, c.updationTime);
        for (const rows of this.files.values())
            for (const r of rows) max = Math.max(max, r.updationTime);
        for (const t of this.trash) max = Math.max(max, t.updatedAt);
        return (this.time = max + 1);
    }

    putCollection(c: Row) {
        this.collections.set(c.id, c);
    }

    putFile(collectionID: number, row: Row) {
        const rows = (this.files.get(collectionID) ?? []).filter(
            (r) => r.id != row.id,
        );
        this.files.set(collectionID, [...rows, row]);
    }

    fileRow(collectionID: number, fileID: number) {
        return this.files
            .get(collectionID)
            ?.find((r) => r.id == fileID && !r.isDeleted);
    }

    liveCollectionIDsOf(fileID: number) {
        return [...this.files.keys()]
            .filter((cid) => this.fileRow(cid, fileID))
            .sort((a, b) => a - b);
    }

    private touch(...collectionIDs: number[]) {
        const now = this.now();
        for (const id of collectionIDs) {
            const c = this.collections.get(id);
            if (c) this.collections.set(id, { ...c, updationTime: now });
        }
        return now;
    }

    private removeRow(collectionID: number, fileID: number, now: number) {
        const row = this.fileRow(collectionID, fileID);
        if (!row) return;
        this.putFile(collectionID, {
            id: fileID,
            collectionID,
            ownerID: row.ownerID,
            isDeleted: true,
            updationTime: now,
        });
    }

    private trashFile(row: Row, now: number) {
        for (const cid of this.liveCollectionIDsOf(row.id))
            this.removeRow(cid, row.id, now);
        this.trash = this.trash.filter((t) => t.file.id != row.id);
        this.trash.push({
            file: row,
            isDeleted: false,
            isRestored: false,
            deleteBy: now + 30 * 24 * 3600 * 1e6,
            updatedAt: now,
        });
    }

    private subtree(id: number): number[] {
        const children = [...this.collections.values()].filter(
            (c) => c.parentID == id && !c.isDeleted,
        );
        return [id, ...children.flatMap((c) => this.subtree(c.id))];
    }

    handle = (r: FakeRequest): FakeResponse | Promise<FakeResponse> => {
        for (const override of this.overrides) {
            const response = override(r);
            if (response) return response;
        }
        const response = this.route(r);
        return this.dropResponse?.(r) ? { dropped: true } : response;
    };

    private route(r: FakeRequest): FakeResponse {
        const body = (r.body ?? {}) as JSONValue;
        const sinceTime = Number(r.query.get("sinceTime") ?? 0);
        const key = `${r.method} ${r.path}`;
        const byID = /^\/collections\/(\d+)$/.exec(r.path);
        if (r.method == "GET" && byID) {
            const c = this.collections.get(Number(byID[1]));
            return c ? { json: { collection: c } } : notFound;
        }
        const deleteMatch = /^\/collections\/v4\/(\d+)$/.exec(r.path);
        if (r.method == "DELETE" && deleteMatch)
            return this.deleteCollection(Number(deleteMatch[1]), r.query);

        switch (key) {
            case "GET /collections/v2":
                return {
                    json: {
                        collections: [...this.collections.values()].filter(
                            (c) => c.updationTime > sinceTime,
                        ),
                    },
                };
            case "GET /collections/v2/diff": {
                const id = Number(r.query.get("collectionID"));
                if (this.deletedCollectionIDs.has(id)) return notFound;
                return {
                    json: page(
                        this.files.get(id) ?? [],
                        sinceTime,
                        this.pageSize,
                        "updationTime",
                    ),
                };
            }
            case "GET /trash/v2/diff":
                return {
                    json: page(
                        this.trash,
                        sinceTime,
                        this.pageSize,
                        "updatedAt",
                    ),
                };
            case "GET /collections/file": {
                const row = this.fileRow(
                    Number(r.query.get("collectionID")),
                    Number(r.query.get("fileID")),
                );
                return row
                    ? { json: { file: row } }
                    : coded(404, "FILE_NOT_FOUND_IN_ALBUM");
            }
            case "POST /collections": {
                const collection: Row = {
                    ...body,
                    id: this.nextID++,
                    owner: { id: userID, email: "", role: "OWNER" },
                    sharees: [],
                    publicURLs: [],
                    updationTime: this.now(),
                };
                this.putCollection(collection);
                return { json: { collection } };
            }
            case "POST /collections/rename": {
                const c = this.collections.get(body.collectionID as number);
                if (!c || c.isDeleted) return coded(404, "NOT_FOUND");
                this.putCollection({
                    ...c,
                    encryptedName: body.encryptedName,
                    nameDecryptionNonce: body.nameDecryptionNonce,
                });
                this.touch(c.id);
                return empty;
            }
            case "POST /collections/move-collection": {
                const c = this.collections.get(body.collectionID as number);
                if (!c || c.isDeleted) return coded(404, "NOT_FOUND");
                const {
                    parentID: _,
                    parentEncryptedKey: __,
                    parentKeyNonce: ___,
                    ...rest
                } = c;
                this.putCollection({
                    ...rest,
                    id: c.id,
                    updationTime: c.updationTime,
                    ...(body.newParentID != null && {
                        parentID: body.newParentID,
                        parentEncryptedKey: body.parentEncryptedKey,
                        parentKeyNonce: body.parentKeyNonce,
                    }),
                });
                this.touch(c.id);
                return empty;
            }
            case "PUT /collections/magic-metadata":
            case "PUT /collections/sharee-magic-metadata": {
                const c = this.collections.get(body.id as number);
                if (!c) return notFound;
                const mm = body.magicMetadata as MagicMetadata;
                const isSharee = r.path.endsWith("sharee-magic-metadata");
                this.putCollection({
                    ...c,
                    ...(isSharee
                        ? { sharedMagicMetadata: mm }
                        : {
                              magicMetadata: { ...mm, version: mm.version + 1 },
                          }),
                });
                this.touch(c.id);
                return empty;
            }
            case "PUT /files/magic-metadata":
            case "PUT /files/public-magic-metadata":
                return this.updateFileMagicMetadata(
                    r.path.endsWith("public-magic-metadata")
                        ? "pubMagicMetadata"
                        : "magicMetadata",
                    body.metadataList as {
                        id: number;
                        magicMetadata: MagicMetadata;
                    }[],
                );
            case "POST /collections/move-files": {
                const from = body.fromCollectionID as number;
                const to = body.toCollectionID as number;
                const target = this.collections.get(to);
                if (!target || target.isDeleted) return notFound;
                const now = this.touch(from, to);
                for (const f of body.files as FileItem[]) {
                    const row =
                        this.fileRow(from, f.id) ??
                        this.liveCollectionIDsOf(f.id)
                            .map((cid) => this.fileRow(cid, f.id))
                            .find((x) => x);
                    if (!row) continue;
                    this.removeRow(from, f.id, now);
                    this.putFile(to, {
                        ...row,
                        collectionID: to,
                        encryptedKey: f.encryptedKey,
                        keyDecryptionNonce: f.keyDecryptionNonce,
                        updationTime: now,
                    });
                }
                return empty;
            }
            case "POST /files/trash": {
                const now = this.now();
                for (const { fileID, collectionID } of body.items as {
                    fileID: number;
                    collectionID: number;
                }[]) {
                    const row = this.fileRow(collectionID, fileID);
                    if (row) this.trashFile(row, now);
                }
                return empty;
            }
            case "POST /collections/restore-files": {
                const to = body.collectionID as number;
                const target = this.collections.get(to);
                if (!target || target.isDeleted) return notFound;
                const items = body.files as FileItem[];
                const entries = items.map((f) =>
                    this.trash.find(
                        (t) =>
                            t.file.id == f.id && !t.isDeleted && !t.isRestored,
                    ),
                );
                if (entries.some((t) => !t)) return { status: 400, json: {} };
                const now = this.touch(to);
                items.forEach((f, i) => {
                    const entry = entries[i]!;
                    entry.isRestored = true;
                    entry.updatedAt = now;
                    this.putFile(to, {
                        ...entry.file,
                        collectionID: to,
                        encryptedKey: f.encryptedKey,
                        keyDecryptionNonce: f.keyDecryptionNonce,
                        updationTime: now,
                    });
                });
                return empty;
            }
            case "POST /trash/delete": {
                const now = this.now();
                for (const t of this.trash)
                    if ((body.fileIDs as number[]).includes(t.file.id)) {
                        t.isDeleted = true;
                        t.updatedAt = now;
                    }
                return empty;
            }
            case "POST /trash/empty": {
                const now = this.now();
                for (const t of this.trash)
                    if (
                        !t.isDeleted &&
                        !t.isRestored &&
                        t.updatedAt <= (body.lastUpdatedAt as number)
                    ) {
                        t.isDeleted = true;
                        t.updatedAt = now;
                    }
                return empty;
            }
        }
        return { status: 404, text: "404 page not found" };
    }

    private updateFileMagicMetadata(
        field: "magicMetadata" | "pubMagicMetadata",
        list: { id: number; magicMetadata: MagicMetadata }[],
    ): FakeResponse {
        const rowsOf = (id: number) =>
            this.liveCollectionIDsOf(id).map((cid) => this.fileRow(cid, id)!);
        for (const { id, magicMetadata } of list) {
            const [row] = rowsOf(id);
            if (!row) return notFound;
            const current =
                (row[field] as MagicMetadata | undefined)?.version ?? 1;
            if (magicMetadata.version != current)
                return { status: 409, json: {} };
        }
        const now = this.touch(
            ...list.flatMap(({ id }) => this.liveCollectionIDsOf(id)),
        );
        for (const { id, magicMetadata } of list) {
            for (const cid of this.liveCollectionIDsOf(id)) {
                const row = this.fileRow(cid, id)!;
                this.putFile(cid, {
                    ...row,
                    [field]: {
                        ...magicMetadata,
                        version: magicMetadata.version + 1,
                    },
                    updationTime: now,
                });
            }
        }
        return empty;
    }

    private deleteCollection(id: number, query: URLSearchParams): FakeResponse {
        const c = this.collections.get(id);
        if (!c || c.isDeleted) return coded(404, "NOT_FOUND");
        const ids = this.subtree(id);
        if (query.get("recursive") != "true" && ids.length > 1)
            return coded(409, "HAS_CHILDREN");
        const hasFiles = ids.some((cid) =>
            (this.files.get(cid) ?? []).some((f) => !f.isDeleted),
        );
        if (query.get("keepFiles") == "true" && hasFiles)
            return coded(409, "COLLECTION_NOT_EMPTY");
        const now = this.now();
        for (const cid of ids) {
            for (const row of (this.files.get(cid) ?? []).filter(
                (f) => !f.isDeleted,
            )) {
                const elsewhere = this.liveCollectionIDsOf(row.id).some(
                    (other) => !ids.includes(other),
                );
                if (elsewhere) this.removeRow(cid, row.id, now);
                else this.trashFile(row, now);
            }
            this.putCollection({
                ...this.collections.get(cid)!,
                isDeleted: true,
                updationTime: now,
            });
        }
        return empty;
    }
}
