import {
    decryptBlob,
    decryptBox,
    decryptBoxBytes,
    encryptBlob,
    encryptBox,
    encryptBoxBytes,
    openCollectionKey,
    type EncryptedBlob,
    type EncryptedBox,
} from "ente-drive-wasm";
import { masterKeyFromSession } from "./account-keys";
import { ensureAuthenticatedSession } from "./authenticated-session";

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

export const ensureMasterKey = async () => {
    const masterKey = await masterKeyFromSession();
    if (!masterKey) throw new Error("Missing master key");
    return masterKey;
};

export const openCollectionKeyWithSession = async (
    ownerID: number,
    encryptedKey: string,
    keyDecryptionNonce: string | undefined,
) =>
    openCollectionKey(
        await ensureAuthenticatedSession(),
        ownerID,
        encryptedKey,
        keyDecryptionNonce,
    );

export const wrapKey = (keyB64: string, wrappingKeyB64: string) =>
    encryptBox(keyB64, wrappingKeyB64);

export const unwrapKey = (box: EncryptedBox, wrappingKeyB64: string) =>
    decryptBox(box, wrappingKeyB64);

export const encryptText = (text: string, keyB64: string) =>
    encryptBoxBytes(textEncoder.encode(text), keyB64);

export const decryptText = async (box: EncryptedBox, keyB64: string) =>
    textDecoder.decode(await decryptBoxBytes(box, keyB64));

export const encryptJSONBox = (value: unknown, keyB64: string) =>
    encryptText(JSON.stringify(value), keyB64);

export const decryptJSONBox = async (
    box: EncryptedBox,
    keyB64: string,
): Promise<unknown> => JSON.parse(await decryptText(box, keyB64));

export const encryptJSONBlob = (value: unknown, keyB64: string) =>
    encryptBlob(textEncoder.encode(JSON.stringify(value)), keyB64);

export const decryptJSONBlob = async (
    blob: EncryptedBlob,
    keyB64: string,
): Promise<unknown> =>
    JSON.parse(textDecoder.decode(await decryptBlob(blob, keyB64)));
