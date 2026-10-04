import log from "ente-base/log";
import {
    decryptJSONBlob,
    decryptJSONBox,
    decryptText,
    openCollectionKeyWithSession,
    unwrapKey,
} from "../crypto";
import type {
    CollectionDetails,
    CollectionParticipant,
    PublicLink,
    StoredCollection,
    StoredFile,
    StoredMagicMetadata,
    StoredTrashItem,
} from "../records";

export type JSONObject = Record<string, unknown>;

export interface DecryptedCollection {
    record: StoredCollection;
    key: string;
    name: string;
    details: CollectionDetails;
    // From the owner's private, or a sharee's own, magic metadata.
    starred: boolean;
}

// Only what the model needs from a file's encrypted metadata.
export interface DecryptedFileRow<R extends StoredFile = StoredFile> {
    record: R;
    title: string;
    editedName: string | undefined;
    mimeType: string | undefined;
    fileType: number | undefined;
    // Epoch microseconds.
    creationTime: number | undefined;
    modificationTime: number | undefined;
    hash: string | undefined;
    noThumb: boolean;
    // undefined when the row has no private magic metadata.
    starred: boolean | undefined;
}

const asObject = (value: unknown): JSONObject =>
    value && typeof value == "object" && !Array.isArray(value)
        ? (value as JSONObject)
        : {};

const string = (value: unknown) =>
    typeof value == "string" && value ? value : undefined;

const number = (value: unknown) =>
    typeof value == "number" && Number.isFinite(value) ? value : undefined;

// Drive writes times in microseconds; other clients (e.g. Locker) wrote
// milliseconds, and some seconds.
const toMicroseconds = (value: unknown) => {
    const n = number(value);
    if (n === undefined || n <= 0) return undefined;
    if (n < 1e11) return Math.round(n * 1e6);
    if (n < 1e14) return Math.round(n * 1e3);
    return Math.round(n);
};

const describe = (e: unknown) => (e instanceof Error ? e.name : typeof e);

export const decryptMagicMetadata = async (
    mm: StoredMagicMetadata | undefined,
    key: string,
): Promise<JSONObject> =>
    mm
        ? asObject(
              await decryptJSONBlob(
                  { encryptedData: mm.data, decryptionHeader: mm.header },
                  key,
              ),
          )
        : {};

const participant = (value: unknown): CollectionParticipant | undefined => {
    const { id, email, role } = asObject(value);
    if (typeof id != "number") return undefined;
    return { id, email: string(email), role: string(role) };
};

const publicLink = (value: unknown): PublicLink | undefined => {
    const v = asObject(value);
    if (typeof v.url != "string") return undefined;
    return {
        url: v.url,
        validTill: number(v.validTill) ?? 0,
        deviceLimit: number(v.deviceLimit) ?? 0,
        enableDownload: v.enableDownload !== false,
        enableCollect: v.enableCollect === true,
        passwordEnabled: v.passwordEnabled === true,
    };
};

const isDefined = <T>(value: T | undefined): value is T => value !== undefined;

const parseDetails = (
    value: unknown,
    record: StoredCollection,
): CollectionDetails => {
    const v = asObject(value);
    return {
        owner: participant(v.owner) ?? { id: record.ownerID, role: "OWNER" },
        sharees: Array.isArray(v.sharees)
            ? v.sharees.map(participant).filter(isDefined)
            : [],
        publicLinks: Array.isArray(v.publicLinks)
            ? v.publicLinks.map(publicLink).filter(isDefined)
            : [],
        legacyName: string(v.legacyName),
    };
};

// A long decryption pass yields now and then so that the UI stays
// responsive.
const yieldInterval = 12;

// Timers are throttled in background tabs, a message round trip isn't.
const yieldToEventLoop = (): Promise<void> => {
    const { scheduler } = globalThis as {
        scheduler?: { yield?: () => Promise<void> };
    };
    if (scheduler?.yield) return scheduler.yield();
    return new Promise((resolve) => {
        const { port1, port2 } = new MessageChannel();
        port1.onmessage = () => {
            port1.close();
            resolve();
        };
        port2.postMessage(undefined);
    });
};

// Returns a promise to await only when it's time to yield, so that the
// (mostly cached) steps in between stay synchronous.
const createYielder = () => {
    let last = performance.now();
    return () => {
        if (performance.now() - last < yieldInterval) return undefined;
        return yieldToEventLoop().then(() => {
            last = performance.now();
        });
    };
};

interface CachedRow {
    record: StoredFile;
    collectionKey: string;
    // undefined: the row failed to decrypt; not tried again until it changes.
    row: DecryptedFileRow | undefined;
    // The last decryption pass that used it.
    pass: number;
}

// Whether two versions of a record decrypt to the same row.
const isSameEncryptedRow = (a: StoredFile, b: StoredFile) =>
    a === b ||
    (a.updationTime == b.updationTime &&
        a.encryptedKey == b.encryptedKey &&
        a.metadata.decryptionHeader == b.metadata.decryptionHeader &&
        a.magicMetadata?.header == b.magicMetadata?.header &&
        a.pubMagicMetadata?.header == b.pubMagicMetadata?.header &&
        (a as Partial<StoredTrashItem>).updatedAt ==
            (b as Partial<StoredTrashItem>).updatedAt);

const sameBox = (
    a: { encryptedData: string; nonce: string } | undefined,
    b: { encryptedData: string; nonce: string } | undefined,
) => a?.encryptedData == b?.encryptedData && a?.nonce == b?.nonce;

// Whether two versions of a collection record decrypt to the same result
// (records are new objects after the database is read again).
const isSameEncryptedCollection = (a: StoredCollection, b: StoredCollection) =>
    a === b ||
    (a.updationTime == b.updationTime &&
        a.ownerID == b.ownerID &&
        a.type == b.type &&
        a.isDeleted == b.isDeleted &&
        a.encryptedKey == b.encryptedKey &&
        a.encryptedName == b.encryptedName &&
        a.nameDecryptionNonce == b.nameDecryptionNonce &&
        a.parentID == b.parentID &&
        a.parentEncryptedKey == b.parentEncryptedKey &&
        a.parentKeyNonce == b.parentKeyNonce &&
        a.magicMetadata?.header == b.magicMetadata?.header &&
        a.shareeMagicMetadata?.header == b.shareeMagicMetadata?.header &&
        sameBox(a.details, b.details));

// Keeps decrypted results across syncs so that only new or changed records
// are decrypted again. Lives only in memory, and returns the same objects for
// unchanged records so that the model can reuse what it built from them.
export class DriveDecryptor {
    private collectionKeys = new Map<
        number,
        { encryptedKey: string; key: string }
    >();
    // undefined: the record failed to decrypt; not tried again until it
    // changes.
    private collections = new Map<
        number,
        { record: StoredCollection; decrypted: DecryptedCollection | undefined }
    >();
    private parentWraps = new Map<
        number,
        { record: StoredCollection; parentKey: string; isValid: boolean }
    >();
    // (Trash or live) → collection ID → file ID → row.
    private fileRows = new Map<string, Map<number, Map<number, CachedRow>>>();
    private pass = 0;

    async collectionKey(record: StoredCollection) {
        const { id, ownerID, encryptedKey, keyDecryptionNonce } = record;
        if (!encryptedKey) throw new Error(`Collection ${id} has no key`);
        const cached = this.collectionKeys.get(id);
        if (cached?.encryptedKey == encryptedKey) return cached.key;
        const key = await openCollectionKeyWithSession(
            ownerID,
            encryptedKey,
            keyDecryptionNonce,
        );
        this.collectionKeys.set(id, { encryptedKey, key });
        return key;
    }

    async decryptCollection(
        record: StoredCollection,
        userID: number,
    ): Promise<DecryptedCollection | undefined> {
        const cached = this.collections.get(record.id);
        if (cached && isSameEncryptedCollection(cached.record, record))
            return cached.decrypted;
        const decrypted = await this.decryptCollectionRecord(record, userID);
        this.collections.set(record.id, { record, decrypted });
        return decrypted;
    }

    private async decryptCollectionRecord(
        record: StoredCollection,
        userID: number,
    ): Promise<DecryptedCollection | undefined> {
        try {
            const key = await this.collectionKey(record);
            const details = parseDetails(
                record.details
                    ? await decryptJSONBox(record.details, key)
                    : undefined,
                record,
            );
            const name =
                record.encryptedName && record.nameDecryptionNonce
                    ? await decryptText(
                          {
                              encryptedData: record.encryptedName,
                              nonce: record.nameDecryptionNonce,
                          },
                          key,
                      )
                    : (details.legacyName ?? "");
            let starred = false;
            try {
                const magic = await decryptMagicMetadata(
                    record.ownerID == userID
                        ? record.magicMetadata
                        : record.shareeMagicMetadata,
                    key,
                );
                starred = magic.starred === true;
            } catch (e) {
                log.warn(
                    `Ignoring unreadable magic metadata of collection ${record.id} (${describe(e)})`,
                );
            }
            return { record, key, name, details, starred };
        } catch (e) {
            log.error(
                `Failed to decrypt collection ${record.id} (${describe(e)})`,
            );
            return undefined;
        }
    }

    // The server stores parentID in plaintext and could lie about it, but it
    // can't forge a parent wrap: the folder key secretbox'd with the parent's
    // key must open to the same key that encryptedKey yields (guide §5.1).
    async verifyParentWrap(child: DecryptedCollection, parentKey: string) {
        const cached = this.parentWraps.get(child.record.id);
        if (
            cached?.parentKey == parentKey &&
            isSameEncryptedCollection(cached.record, child.record)
        )
            return cached.isValid;
        const { parentEncryptedKey, parentKeyNonce } = child.record;
        let isValid = false;
        if (parentEncryptedKey && parentKeyNonce) {
            try {
                const unwrapped = await unwrapKey(
                    {
                        encryptedData: parentEncryptedKey,
                        nonce: parentKeyNonce,
                    },
                    parentKey,
                );
                isValid = unwrapped == child.key;
            } catch {
                isValid = false;
            }
        }
        this.parentWraps.set(child.record.id, {
            record: child.record,
            parentKey,
            isValid,
        });
        return isValid;
    }

    fileKey(record: StoredFile, collectionKey: string) {
        return unwrapKey(
            {
                encryptedData: record.encryptedKey,
                nonce: record.keyDecryptionNonce,
            },
            collectionKey,
        );
    }

    beginPass() {
        this.pass++;
    }

    // Forgets rows that the last pass didn't use.
    endPass() {
        for (const byCollection of this.fileRows.values())
            for (const [cid, rows] of byCollection) {
                for (const [id, cached] of rows)
                    if (cached.pass != this.pass) rows.delete(id);
                if (!rows.size) byCollection.delete(cid);
            }
    }

    // The cached row when the record is unchanged, otherwise a promise of it.
    fileRow<R extends StoredFile>(
        record: R,
        collectionKey: string,
        isTrash = false,
    ):
        | DecryptedFileRow<R>
        | undefined
        | Promise<DecryptedFileRow<R> | undefined> {
        const kind = isTrash ? "trash" : "live";
        let byCollection = this.fileRows.get(kind);
        if (!byCollection)
            this.fileRows.set(
                kind,
                (byCollection = new Map<number, Map<number, CachedRow>>()),
            );
        let rows = byCollection.get(record.collectionID);
        if (!rows)
            byCollection.set(
                record.collectionID,
                (rows = new Map<number, CachedRow>()),
            );
        const cached = rows.get(record.id);
        if (
            cached?.collectionKey == collectionKey &&
            isSameEncryptedRow(cached.record, record)
        ) {
            cached.pass = this.pass;
            return cached.row as DecryptedFileRow<R> | undefined;
        }
        const pass = this.pass;
        return this.decryptFile(record, collectionKey).then((row) => {
            rows.set(record.id, { record, collectionKey, row, pass });
            return row;
        });
    }

    private async decryptFile<R extends StoredFile>(
        record: R,
        collectionKey: string,
    ): Promise<DecryptedFileRow<R> | undefined> {
        try {
            const fileKey = await this.fileKey(record, collectionKey);
            const metadata = asObject(
                await decryptJSONBlob(record.metadata, fileKey),
            );
            const optionalMagic = async (
                mm: StoredMagicMetadata | undefined,
            ) => {
                try {
                    return await decryptMagicMetadata(mm, fileKey);
                } catch (e) {
                    log.warn(
                        `Ignoring unreadable magic metadata of file ${record.id} (${describe(e)})`,
                    );
                    return {};
                }
            };
            const publicMagic = await optionalMagic(record.pubMagicMetadata);
            const privateMagic = record.magicMetadata
                ? await optionalMagic(record.magicMetadata)
                : undefined;
            return {
                record,
                title: string(metadata.title) ?? "",
                editedName: string(publicMagic.editedName),
                mimeType: string(metadata.mimeType),
                fileType: number(metadata.fileType),
                creationTime: toMicroseconds(metadata.creationTime),
                modificationTime: toMicroseconds(metadata.modificationTime),
                hash: string(metadata.hash),
                noThumb: publicMagic.noThumb === true,
                starred:
                    privateMagic && Object.keys(privateMagic).length
                        ? privateMagic.starred === true
                        : undefined,
            };
        } catch (e) {
            log.error(`Failed to decrypt file ${record.id} (${describe(e)})`);
            return undefined;
        }
    }
}

export interface DecryptedState {
    collections: DecryptedCollection[];
    // Whether an owned collection's claimed (live, known) parent verified.
    parentWrapValid: Map<number, boolean>;
    files: DecryptedFileRow[];
    trash: DecryptedFileRow<StoredTrashItem>[];
}

export const decryptState = async (
    decryptor: DriveDecryptor,
    userID: number,
    collections: Iterable<StoredCollection>,
    files: Iterable<StoredFile>,
    trash: Iterable<StoredTrashItem>,
): Promise<DecryptedState> => {
    const pause = createYielder();
    decryptor.beginPass();
    const allCollections = new Map<number, StoredCollection>();
    for (const c of collections) allCollections.set(c.id, c);

    const live = new Map<number, DecryptedCollection>();
    for (const record of allCollections.values()) {
        if (record.isDeleted) continue;
        const decrypted = await decryptor.decryptCollection(record, userID);
        if (decrypted) live.set(record.id, decrypted);
        await pause();
    }

    const parentWrapValid = new Map<number, boolean>();
    for (const c of live.values()) {
        const { parentID, ownerID } = c.record;
        if (parentID === undefined || ownerID != userID) continue;
        const parent = live.get(parentID);
        // A missing or deleted parent makes an orphan, shown at the root
        // without a warning (the server repairs those).
        if (!parent) continue;
        parentWrapValid.set(
            c.record.id,
            parent.record.ownerID == userID &&
                (await decryptor.verifyParentWrap(c, parent.key)),
        );
        await pause();
    }

    const decryptedFiles: DecryptedFileRow[] = [];
    for (const record of files) {
        const collection = live.get(record.collectionID);
        if (!collection) continue;
        const result = decryptor.fileRow(record, collection.key);
        const row = result instanceof Promise ? await result : result;
        if (row) decryptedFiles.push(row);
        const yielding = pause();
        if (yielding) await yielding;
    }

    const decryptedTrash: DecryptedFileRow<StoredTrashItem>[] = [];
    for (const record of trash) {
        const collection = allCollections.get(record.collectionID);
        if (!collection) {
            log.warn(`Skipping trash item ${record.id} without its collection`);
            continue;
        }
        try {
            const key = await decryptor.collectionKey(collection);
            const row = await decryptor.fileRow(record, key, true);
            if (row) decryptedTrash.push(row);
        } catch (e) {
            log.error(
                `Failed to decrypt trash item ${record.id} (${describe(e)})`,
            );
        }
        await pause();
    }
    decryptor.endPass();

    return {
        collections: [...live.values()],
        parentWrapValid,
        files: decryptedFiles,
        trash: decryptedTrash,
    };
};
