import { useDriveSession } from "@/components/drive/use-drive-session";
import { useDriveSyncLifecycle } from "@/services/drive-api";
import { Stack, Typography } from "@mui/material";
import { LoadingIndicator } from "ente-base/components/loaders";
import { FocusVisibleButton } from "ente-base/components/mui/FocusVisibleButton";
import { useBaseContext } from "ente-base/context";
import { t } from "i18next";
import React from "react";

export const DrivePage: React.FC = () => {
    const { logout } = useBaseContext();
    const session = useDriveSession();
    useDriveSyncLifecycle(session.status == "ready");

    if (session.status == "loading") return <LoadingIndicator />;

    return (
        <Stack
            sx={{
                height: "100svh",
                gap: 1.5,
                px: 3,
                alignItems: "center",
                justifyContent: "center",
                textAlign: "center",
            }}
        >
            {session.status == "error" ? (
                <>
                    <Typography variant="h5">
                        {t("drive_load_error_title")}
                    </Typography>
                    <Typography sx={{ color: "text.muted", maxWidth: 360 }}>
                        {t("drive_load_error_message")}
                    </Typography>
                    <FocusVisibleButton
                        color="accent"
                        onClick={session.retry}
                        sx={{ mt: 1.5 }}
                    >
                        {t("retry")}
                    </FocusVisibleButton>
                </>
            ) : (
                <>
                    <Typography variant="h5">{t("drive_my_drive")}</Typography>
                    <Typography sx={{ color: "text.muted" }}>
                        {session.user.email}
                    </Typography>
                    <FocusVisibleButton
                        color="secondary"
                        onClick={logout}
                        sx={{ mt: 1.5 }}
                    >
                        {t("logout")}
                    </FocusVisibleButton>
                </>
            )}
        </Stack>
    );
};
