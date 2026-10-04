import { DriveAuthShell } from "@/components/auth/DriveAuthShell";
import { LoginFrame } from "@/components/auth/LoginFrame";
import type { AuthPageConfig } from "ente-accounts/components/auth/AuthPageProvider";
import { decryptBox } from "ente-drive-wasm";

export const authPageConfig: AuthPageConfig = {
    decryptBox,
    encryptWithRecoveryKey: async (data) => {
        const { encryptWithRecoveryKey } =
            await import("./services/recovery-key");
        return encryptWithRecoveryKey(data);
    },
    Shell: DriveAuthShell,
    LoginFrame,
    keepLoginLoadingOnRedirect: true,
};
