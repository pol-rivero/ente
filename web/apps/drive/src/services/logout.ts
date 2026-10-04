import {
    accountLogout,
    logoutClearStateAgain,
} from "ente-accounts/services/logout";
import log from "ente-base/log";
import { clearAuthenticatedSession } from "./authenticated-session";
import { clearDriveLocalState, stopDriveSync } from "./sync";
import { postToOtherTabs } from "./tabs";

export const driveLogout = async () => {
    const ignoreError = (label: string, error: unknown) =>
        log.error(`Ignoring error during logout (${label})`, error);

    log.info("logout (drive)");

    // Stop syncing first, so that nothing writes while the state is cleared.
    stopDriveSync();

    // Session

    try {
        clearAuthenticatedSession();
    } catch (error) {
        ignoreError("Authenticated session", error);
    }

    // Remote logout and clear state

    await accountLogout();

    try {
        await clearDriveLocalState();
    } catch (error) {
        ignoreError("Drive local state", error);
    }

    // Final sweep and reload

    await logoutClearStateAgain();

    // A sync that was already running may have written meanwhile.
    try {
        await clearDriveLocalState();
    } catch (error) {
        ignoreError("Drive local state (final sweep)", error);
    }
    postToOtherTabs({ type: "logout" });
    window.location.replace("/login");
};
