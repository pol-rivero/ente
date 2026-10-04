import type { EncryptedBlob, EncryptedBox } from "ente-drive-wasm";
import type {
    RemoteCollection,
    RemoteFile,
    RemoteMagicMetadata,
    RemoteTrashItem,
} from "./remote/schemas";

// Records as persisted in IndexedDB. Everything that reveals names, emails or
// file contents stays encrypted: names and metadata as the server sent them,
// and the collection's owner/sharee/link details in a box encrypted with the
// collection key (`details`).

export interface StoredMagicMetadata {
    version: number;
    count: number;
    data: string;
    header: string;
}

export interface StoredCollection {
    id: number;
    ownerID: number;
    type: string;
    encryptedKey?: string;
    keyDecryptionNonce?: string;
    encryptedName?: string;
    nameDecryptionNonce?: string;
    parentID?: number;
    parentEncryptedKey?: string;
    parentKeyNonce?: string;
    magicMetadata?: StoredMagicMetadata;
    pubMagicMetadata?: StoredMagicMetadata;
    // A sharee's own magic metadata for a collection shared with them.
    shareeMagicMetadata?: StoredMagicMetadata;
    details?: EncryptedBox;
    isDeleted: boolean;
    updationTime: number;
}

export interface StoredFile {
    id: number;
    collectionID: number;
    ownerID: number;
    encryptedKey: string;
    keyDecryptionNonce: string;
    fileDecryptionHeader: string;
    thumbnailDecryptionHeader?: string;
    metadata: EncryptedBlob;
    magicMetadata?: StoredMagicMetadata;
    pubMagicMetadata?: StoredMagicMetadata;
    encryptedSize?: number;
    updationTime: number;
}

export interface StoredTrashItem extends StoredFile {
    updatedAt: number;
    deleteBy: number;
}

// A folder delete whose files are still on their way to the trash (C11).
// Holds IDs only, so it can be kept unencrypted.
export interface StoredPendingTrash {
    folderID: number;
    folderIDs: number[];
    fileIDs: number[];
    // Local epoch milliseconds.
    startedAt: number;
}

export interface CollectionParticipant {
    id: number;
    email?: string;
    role?: string;
}

export interface PublicLink {
    url: string;
    validTill: number;
    deviceLimit: number;
    enableDownload: boolean;
    enableCollect: boolean;
    passwordEnabled: boolean;
}

export interface CollectionDetails {
    owner: CollectionParticipant;
    sharees: CollectionParticipant[];
    publicLinks: PublicLink[];
    legacyName?: string;
}

const storedMagicMetadata = (
    mm: RemoteMagicMetadata | null | undefined,
): StoredMagicMetadata | undefined =>
    mm
        ? {
              version: mm.version,
              count: mm.count ?? 0,
              data: mm.data,
              header: mm.header,
          }
        : undefined;

const optional = <T>(value: T | null | undefined) => value ?? undefined;

export const storedCollection = (
    c: RemoteCollection,
    details: EncryptedBox | undefined,
): StoredCollection => ({
    id: c.id,
    ownerID: c.owner.id,
    type: c.type ?? "",
    encryptedKey: optional(c.encryptedKey),
    keyDecryptionNonce: optional(c.keyDecryptionNonce),
    encryptedName: optional(c.encryptedName),
    nameDecryptionNonce: optional(c.nameDecryptionNonce),
    parentID: optional(c.parentID),
    parentEncryptedKey: optional(c.parentEncryptedKey),
    parentKeyNonce: optional(c.parentKeyNonce),
    magicMetadata: storedMagicMetadata(c.magicMetadata),
    pubMagicMetadata: storedMagicMetadata(c.pubMagicMetadata),
    shareeMagicMetadata: storedMagicMetadata(c.sharedMagicMetadata),
    details,
    isDeleted: !!c.isDeleted,
    updationTime: c.updationTime,
});

export const collectionDetails = (c: RemoteCollection): CollectionDetails => ({
    owner: { id: c.owner.id, email: c.owner.email || undefined, role: "OWNER" },
    sharees: (c.sharees ?? []).map((s) => ({
        id: s.id,
        email: s.email || undefined,
        role: s.role?.toUpperCase() || undefined,
    })),
    publicLinks: (c.publicURLs ?? []).map((u) => ({
        url: u.url,
        validTill: u.validTill ?? 0,
        deviceLimit: u.deviceLimit ?? 0,
        enableDownload: u.enableDownload ?? true,
        enableCollect: u.enableCollect ?? false,
        passwordEnabled: u.passwordEnabled ?? false,
    })),
    legacyName: (!c.encryptedName && c.name) || undefined,
});

// Returns undefined for rows that lack what a live file needs.
export const storedFile = (f: RemoteFile): StoredFile | undefined => {
    if (
        !f.encryptedKey ||
        !f.keyDecryptionNonce ||
        !f.metadata?.encryptedData ||
        !f.metadata.decryptionHeader
    )
        return undefined;
    return {
        id: f.id,
        collectionID: f.collectionID,
        ownerID: f.ownerID ?? 0,
        encryptedKey: f.encryptedKey,
        keyDecryptionNonce: f.keyDecryptionNonce,
        fileDecryptionHeader: f.file?.decryptionHeader ?? "",
        thumbnailDecryptionHeader: optional(f.thumbnail?.decryptionHeader),
        metadata: {
            encryptedData: f.metadata.encryptedData,
            decryptionHeader: f.metadata.decryptionHeader,
        },
        magicMetadata: storedMagicMetadata(f.magicMetadata),
        pubMagicMetadata: storedMagicMetadata(f.pubMagicMetadata),
        encryptedSize: optional(f.info?.fileSize),
        updationTime: f.updationTime,
    };
};

export const storedTrashItem = (
    t: RemoteTrashItem,
): StoredTrashItem | undefined => {
    const file = storedFile(t.file);
    return (
        file && { ...file, updatedAt: t.updatedAt, deleteBy: t.deleteBy ?? 0 }
    );
};

const sameMagicMetadata = (
    a: StoredMagicMetadata | undefined,
    b: StoredMagicMetadata | undefined,
) => a?.version == b?.version && a?.header == b?.header;

// Rows the server sends again (overlapping diffs, ingested uploads) are
// recognised as unchanged so that they don't trigger a new model.
export const isSameFileRecord = (a: StoredFile, b: StoredFile) =>
    a.updationTime == b.updationTime &&
    a.encryptedKey == b.encryptedKey &&
    a.metadata.decryptionHeader == b.metadata.decryptionHeader &&
    sameMagicMetadata(a.magicMetadata, b.magicMetadata) &&
    sameMagicMetadata(a.pubMagicMetadata, b.pubMagicMetadata);

export const isSameTrashRecord = (a: StoredTrashItem, b: StoredTrashItem) =>
    a.updatedAt == b.updatedAt && isSameFileRecord(a, b);

// Collections compare by what the server changes along with updationTime
// (the details box is encrypted locally). A stored record without details
// (its key didn't open) is replaced once a fetch brings them.
export const isSameCollectionRecord = (
    stored: StoredCollection,
    fetched: StoredCollection,
) =>
    stored.updationTime == fetched.updationTime &&
    stored.isDeleted == fetched.isDeleted &&
    stored.encryptedKey == fetched.encryptedKey &&
    (stored.isDeleted || !!stored.details || !fetched.details);
