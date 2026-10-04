import log from "ente-base/log";
import { z } from "zod";

export const RemoteMagicMetadata = z.object({
    version: z.number(),
    count: z.number().nullish(),
    data: z.string(),
    header: z.string(),
});

export type RemoteMagicMetadata = z.infer<typeof RemoteMagicMetadata>;

const RemoteCollectionUser = z.object({
    id: z.number(),
    email: z.string().nullish(),
    role: z.string().nullish(),
});

const RemotePublicURL = z.object({
    url: z.string(),
    validTill: z.number().nullish(),
    deviceLimit: z.number().nullish(),
    enableDownload: z.boolean().nullish(),
    enableCollect: z.boolean().nullish(),
    passwordEnabled: z.boolean().nullish(),
});

// Shared collections have no keyDecryptionNonce (the key is a sealed box),
// and tombstones of unshared collections lack names and most other fields.
export const RemoteCollection = z.object({
    id: z.number(),
    owner: RemoteCollectionUser,
    encryptedKey: z.string().nullish(),
    keyDecryptionNonce: z.string().nullish(),
    name: z.string().nullish(),
    encryptedName: z.string().nullish(),
    nameDecryptionNonce: z.string().nullish(),
    type: z.string().nullish(),
    sharees: z.array(RemoteCollectionUser).nullish(),
    publicURLs: z.array(RemotePublicURL).nullish(),
    updationTime: z.number(),
    isDeleted: z.boolean().nullish(),
    magicMetadata: RemoteMagicMetadata.nullish(),
    pubMagicMetadata: RemoteMagicMetadata.nullish(),
    sharedMagicMetadata: RemoteMagicMetadata.nullish(),
    parentID: z.number().nullish(),
    parentEncryptedKey: z.string().nullish(),
    parentKeyNonce: z.string().nullish(),
});

export type RemoteCollection = z.infer<typeof RemoteCollection>;

const RemoteFileAttributes = z.object({
    encryptedData: z.string().nullish(),
    decryptionHeader: z.string().nullish(),
});

// Rows removed from a collection are scrubbed down to IDs and times.
export const RemoteFile = z.object({
    id: z.number(),
    ownerID: z.number().nullish(),
    collectionID: z.number(),
    encryptedKey: z.string().nullish(),
    keyDecryptionNonce: z.string().nullish(),
    file: RemoteFileAttributes.nullish(),
    thumbnail: RemoteFileAttributes.nullish(),
    metadata: RemoteFileAttributes.nullish(),
    isDeleted: z.boolean().nullish(),
    updationTime: z.number(),
    magicMetadata: RemoteMagicMetadata.nullish(),
    pubMagicMetadata: RemoteMagicMetadata.nullish(),
    info: z
        .object({
            fileSize: z.number().nullish(),
            thumbSize: z.number().nullish(),
        })
        .nullish(),
});

export type RemoteFile = z.infer<typeof RemoteFile>;

export const RemoteTrashItem = z.object({
    file: RemoteFile,
    isDeleted: z.boolean().nullish(),
    isRestored: z.boolean().nullish(),
    deleteBy: z.number().nullish(),
    createdAt: z.number().nullish(),
    updatedAt: z.number(),
});

export type RemoteTrashItem = z.infer<typeof RemoteTrashItem>;

export const CollectionsResponse = z.object({
    collections: z.array(z.unknown()).nullish(),
});

export const CollectionResponse = z.object({ collection: RemoteCollection });

export const DiffResponse = z.object({
    diff: z.array(z.unknown()).nullish(),
    hasMore: z.boolean().nullish(),
});

export const FileResponse = z.object({ file: RemoteFile });

// Each item is validated on its own so that one malformed record doesn't
// stop the sync; rejected items are logged by ID only.
export const parseItems = <T extends z.ZodType>(
    schema: T,
    items: readonly unknown[] | null | undefined,
    label: string,
): z.infer<T>[] => {
    const parsed: z.infer<T>[] = [];
    for (const item of items ?? []) {
        const result = schema.safeParse(item);
        if (result.success) {
            parsed.push(result.data);
        } else {
            const id = (item as { id?: unknown } | null)?.id;
            log.warn(
                `Skipping malformed ${label}${typeof id == "number" ? ` ${id}` : ""}`,
            );
        }
    }
    return parsed;
};
