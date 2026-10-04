import type { AuthLoginFrameProps } from "ente-accounts/components/auth/AuthPageProvider";
import { DevSettingsDialog } from "ente-accounts/components/auth/DevSettingsDialog";
import { DevSettingsTapFrame } from "ente-accounts/components/auth/DevSettingsTapFrame";
import { DevSettings } from "ente-new/photos/components/DevSettings";
import React from "react";

export const LoginFrame: React.FC<AuthLoginFrameProps> = (props) => (
    <DevSettingsTapFrame
        {...props}
        renderDevSettings={(open, onClose) => (
            <DevSettings
                open={open}
                onClose={onClose}
                presentation={DevSettingsDialog}
            />
        )}
    />
);
