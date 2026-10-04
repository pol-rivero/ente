import { bootstrapDriveSession, type DriveSession } from "@/services/session";
import { sessionExpiredDialogAttributes } from "ente-accounts/components/utils/dialog";
import { savedPartialLocalUser } from "ente-accounts/services/accounts-db";
import { stashRedirect } from "ente-accounts/services/redirect";
import { useBaseContext } from "ente-base/context";
import { isNamedError } from "ente-base/error";
import { isHTTP401Error } from "ente-base/http";
import log from "ente-base/log";
import { t } from "i18next";
import { useRouter } from "next/router";
import { useCallback, useEffect, useState } from "react";

type DriveSessionState =
    | { status: "loading" }
    | { status: "ready"; user: DriveSession["user"] }
    | { status: "error"; retry: () => void };

export const useDriveSession = (): DriveSessionState => {
    const { logout, showMiniDialog } = useBaseContext();
    const router = useRouter();
    const [attempt, setAttempt] = useState(0);
    const [state, setState] = useState<DriveSessionState>({
        status: "loading",
    });

    const retry = useCallback(() => {
        setState({ status: "loading" });
        setAttempt((n) => n + 1);
    }, []);

    useEffect(() => {
        let isCancelled = false;

        const handleError = (e: unknown) => {
            log.error("Failed to load the Drive session", e);
            if (isNamedError(e, "drive_server_unsupported")) {
                showMiniDialog({
                    title: t("drive_server_unsupported_title"),
                    message: t("drive_server_unsupported_message"),
                    nonClosable: true,
                    nonReplaceable: true,
                    continue: { text: t("logout"), action: logout },
                    cancel: false,
                });
            } else if (
                isHTTP401Error(e) ||
                isNamedError(e, "drive_session_invalid")
            ) {
                showMiniDialog(sessionExpiredDialogAttributes(logout));
            } else {
                setState({ status: "error", retry });
            }
        };

        void bootstrapDriveSession().then(
            (session) => {
                if (isCancelled) return;
                if (session) {
                    setState({ status: "ready", user: session.user });
                } else {
                    stashRedirect(router.asPath);
                    void router.replace(
                        savedPartialLocalUser()?.email ? "/verify" : "/login",
                    );
                }
            },
            (e: unknown) => {
                if (!isCancelled) handleError(e);
            },
        );

        return () => {
            isCancelled = true;
        };
    }, [attempt, router, logout, showMiniDialog, retry]);

    return state;
};
