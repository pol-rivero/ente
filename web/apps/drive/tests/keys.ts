export const userID = 1;

// Shared crypto context for the mocks: the user's master key, and keys of
// collections shared with them (sealed boxes can't be made without a key
// pair, so the mock looks those up). Kept on globalThis so that module
// resets between tests don't fork it.
const holder = globalThis as typeof globalThis & {
    driveTestKeys?: {
        masterKey: string;
        shared: Map<string, string>;
        // Make this many next key opens fail.
        failingOpens: number;
    };
};
holder.driveTestKeys ??= { masterKey: "", shared: new Map(), failingOpens: 0 };
export const keys = holder.driveTestKeys;

type DecryptBox = typeof import("ente-drive-wasm").decryptBox;

export const fakeOpenCollectionKey =
    (decryptBox: DecryptBox) =>
    async (
        _session: unknown,
        ownerID: number,
        encryptedKey: string,
        nonce?: string,
    ) => {
        if (keys.failingOpens > 0) {
            keys.failingOpens--;
            throw new Error("Transient failure");
        }
        if (ownerID == userID) {
            if (!nonce) throw new Error("Missing nonce");
            return decryptBox(
                { encryptedData: encryptedKey, nonce },
                keys.masterKey,
            );
        }
        const key = keys.shared.get(encryptedKey);
        if (!key) throw new Error("Cannot open sealed box");
        return key;
    };
