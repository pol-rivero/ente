import { InformationCircleIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { CircularProgress, styled, type ModalProps } from "@mui/material";
import { t } from "i18next";
import type React from "react";
import {
    AuthDialog,
    AuthDialogHeader,
    AuthDialogText,
    AuthDialogTitle,
} from "./AuthDialog";
import { Button } from "./Button";
import {
    authBodyTypography,
    authDialogContentLayout,
    authFocusRing,
} from "./styles";
import { TextField } from "./TextField";

// Matches DevSettingsPresentationProps in ente-new, which depends on this
// package.
interface DevSettingsDialogProps {
    open: boolean;
    onDialogClose: ModalProps["onClose"];
    value: string;
    onChange: React.ChangeEventHandler<HTMLInputElement | HTMLTextAreaElement>;
    onBlur: React.FocusEventHandler<HTMLInputElement | HTMLTextAreaElement>;
    error?: string;
    isChecking: boolean;
    canSave: boolean;
    onSubmit: React.SubmitEventHandler<HTMLFormElement>;
    onClose: () => void;
}

export const DevSettingsDialog: React.FC<DevSettingsDialogProps> = ({
    open,
    onDialogClose,
    value,
    onChange,
    onBlur,
    error,
    isChecking,
    canSave,
    onSubmit,
    onClose,
}) => (
    <AuthDialog
        open={open}
        onClose={onDialogClose}
        ariaLabelledby="developer-settings-title"
    >
        <FormRoot onSubmit={onSubmit}>
            <AuthDialogHeader>
                <AuthDialogTitle id="developer-settings-title">
                    {t("developer_settings")}
                </AuthDialogTitle>
                <AuthDialogText>
                    {t("auth_developer_settings_subtitle")}
                </AuthDialogText>
            </AuthDialogHeader>
            <TextField
                name="apiOrigin"
                label={t("server_endpoint")}
                value={value}
                onChange={onChange}
                onBlur={onBlur}
                placeholder="https://api.example.org"
                autoFocus
                disabled={isChecking}
                error={Boolean(error)}
                helperText={
                    error == "Invalid endpoint"
                        ? t("auth_endpoint_invalid_url")
                        : error
                          ? t("auth_endpoint_unreachable")
                          : undefined
                }
                trailing={
                    <InfoLink
                        href="https://ente.com/help/self-hosting/installation/post-install/#step-6-configure-apps-to-use-your-server"
                        target="_blank"
                        rel="noopener"
                        aria-label={t("more_information")}
                    >
                        <HugeiconsIcon
                            icon={InformationCircleIcon}
                            size={20}
                            strokeWidth={2}
                            aria-hidden="true"
                        />
                    </InfoLink>
                }
            />
            {isChecking && (
                <StatusPill role="status">
                    <CircularProgress
                        size={18}
                        sx={{ color: "var(--auth-ui-primary)" }}
                    />
                    <span>{t("auth_endpoint_checking")}</span>
                </StatusPill>
            )}
            <ButtonRow>
                <Button
                    type="submit"
                    variant="primary"
                    fullWidth
                    disabled={!canSave}
                >
                    {t("save")}
                </Button>
                <Button variant="secondary" fullWidth onClick={onClose}>
                    {t("cancel")}
                </Button>
            </ButtonRow>
        </FormRoot>
    </AuthDialog>
);

const FormRoot = styled("form")(authDialogContentLayout);

const InfoLink = styled("a")({
    width: "20px",
    height: "20px",
    flex: "0 0 20px",
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    borderRadius: "4px",
    color: "var(--auth-ui-text-faint)",
    "&:hover": { color: "var(--auth-ui-text-muted)" },
    "&:focus-visible": authFocusRing,
});

const StatusPill = styled("div")({
    ...authBodyTypography,
    display: "flex",
    alignItems: "center",
    gap: "10px",
    padding: "12px 14px",
    borderRadius: "20px",
    backgroundColor: "var(--auth-ui-button-secondary)",
    boxShadow: "inset 0 0 0 1px var(--auth-ui-stroke)",
    color: "var(--auth-ui-text-muted)",
    "& > svg, & > .MuiCircularProgress-root": { flexShrink: 0 },
});

const ButtonRow = styled("div")({ display: "flex", gap: "12px" });
