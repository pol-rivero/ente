import { savedLocalUser } from "ente-accounts/services/accounts-db";
import { namedError } from "ente-base/error";
import { savedAuthToken } from "ente-base/token";
import { masterKeyFromSession } from "./account-keys";
import { openAuthenticatedSession } from "./authenticated-session";
import { ensureDriveServerSupported } from "./server-capability";

export interface DriveSession {
    user: { id: number; email: string };
}

// Resolves to undefined when the user needs to log in.
export const bootstrapDriveSession = async (): Promise<
    DriveSession | undefined
> => {
    const [masterKey, token] = await Promise.all([
        masterKeyFromSession(),
        savedAuthToken(),
    ]);
    const user = savedLocalUser();
    if (!token || !masterKey || !user) return undefined;

    const { id, email } = user;
    await openSessionOrThrowInvalid(id, token, masterKey);
    await ensureDriveServerSupported();
    return { user: { id, email } };
};

const openSessionOrThrowInvalid = async (
    userID: number,
    token: string,
    masterKey: string,
) => {
    try {
        await openAuthenticatedSession(userID, token, masterKey);
    } catch (e) {
        // Opening the session is local crypto, so a failure means the saved
        // credentials are unusable. Failing to load the WASM module itself is
        // likely transient.
        if (isModuleLoadError(e)) throw e;
        throw namedError(
            "drive_session_invalid",
            "Failed to open the Drive session",
            { cause: e },
        );
    }
};

const isModuleLoadError = (e: unknown) =>
    e instanceof Error && (e.name == "TypeError" || e.name == "ChunkLoadError");
