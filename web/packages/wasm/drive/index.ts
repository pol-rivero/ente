import type {
    EncryptedBlob,
    EncryptedBox,
    OpenSessionInput,
    Session,
} from "./pkg/ente_drive_wasm";

export type {
    EncryptedBlob,
    EncryptedBox,
    Session,
} from "./pkg/ente_drive_wasm";

const wasm = () => import("./pkg/ente_drive_wasm");

export const openSession = async (input: OpenSessionInput): Promise<Session> =>
    (await wasm()).openSession(input);

export const recoveryKeyMnemonic = async (session: Session) =>
    (await wasm()).authRecoveryKeyMnemonic(session);

export const encryptBoxWithRecoveryKey = async (
    session: Session,
    dataB64: string,
) => (await wasm()).authEncryptWithRecoveryKey(session, dataB64);

export const openCollectionKey = async (
    session: Session,
    ownerID: number,
    encryptedKey: string,
    keyDecryptionNonce?: string,
) =>
    (await wasm()).collectionsOpenKey(
        session,
        BigInt(ownerID),
        encryptedKey,
        keyDecryptionNonce,
    );

export const generateKey = async () => (await wasm()).cryptoGenerateKey();

export const encryptBox = async (
    dataB64: string,
    keyB64: string,
): Promise<EncryptedBox> => (await wasm()).cryptoEncryptBox(dataB64, keyB64);

export const encryptBoxBytes = async (
    data: Uint8Array,
    keyB64: string,
): Promise<EncryptedBox> => (await wasm()).cryptoEncryptBoxBytes(data, keyB64);

export const decryptBox = async (
    box: EncryptedBox,
    keyB64: string,
): Promise<string> =>
    (await wasm()).cryptoDecryptBox(box.encryptedData, box.nonce, keyB64);

export const decryptBoxBytes = async (
    box: EncryptedBox,
    keyB64: string,
): Promise<Uint8Array> =>
    (await wasm()).cryptoDecryptBoxBytes(box.encryptedData, box.nonce, keyB64);

export const encryptBlob = async (
    data: Uint8Array,
    keyB64: string,
): Promise<EncryptedBlob> => (await wasm()).cryptoEncryptBlob(data, keyB64);

export const decryptBlob = async (
    blob: EncryptedBlob,
    keyB64: string,
): Promise<Uint8Array> =>
    (await wasm()).cryptoDecryptBlobLegacy(
        blob.encryptedData,
        blob.decryptionHeader,
        keyB64,
    );
