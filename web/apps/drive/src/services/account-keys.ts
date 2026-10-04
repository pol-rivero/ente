import { readMasterKeyFromSession } from "ente-base/session-storage";
import { decryptBox } from "ente-drive-wasm";

export const masterKeyFromSession = () => readMasterKeyFromSession(decryptBox);
