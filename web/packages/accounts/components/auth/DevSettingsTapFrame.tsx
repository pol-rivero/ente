import { styled } from "@mui/material";
import React, { useRef, useState } from "react";
import type { AuthLoginFrameProps } from "./AuthPageProvider";

interface DevSettingsTapFrameProps extends AuthLoginFrameProps {
    // The developer settings live in ente-new, which depends on this package,
    // so the app renders them.
    renderDevSettings: (open: boolean, onClose: () => void) => React.ReactNode;
}

export const DevSettingsTapFrame: React.FC<DevSettingsTapFrameProps> = ({
    children,
    onHostChanged,
    renderDevSettings,
}) => {
    const [showDevSettings, setShowDevSettings] = useState(false);
    const tapCount = useRef(0);

    const handleBackgroundClick: React.MouseEventHandler = (event) => {
        if (!canChangeAPIOrigin() || showDevSettings) return;
        if (
            event.target instanceof Element &&
            event.target.closest(
                'button, a, input, textarea, select, [role="button"]',
            )
        ) {
            return;
        }
        tapCount.current += 1;
        if (tapCount.current == 7) {
            tapCount.current = 0;
            setShowDevSettings(true);
        }
    };

    const handleClose = () => {
        setShowDevSettings(false);
        onHostChanged();
    };

    return (
        <Root onClick={handleBackgroundClick}>
            {children}
            {renderDevSettings(showDevSettings, handleClose)}
        </Root>
    );
};

const Root = styled("div")({ width: "100%", minHeight: "100svh" });

const canChangeAPIOrigin = () => {
    const hostname = new URL(window.location.origin).hostname;
    return !(
        hostname.endsWith(".ente.com") ||
        hostname.endsWith(".ente.io") ||
        hostname.endsWith(".ente.sh")
    );
};
