import i18n from "i18next";
import { useEffect, useState } from "react";
import enUS from "../locales/en-US/translation.json";

// App-specific strings, merged into the shared namespace. Other locales fall
// back to en-US until translations exist.
export const useSetupDriveI18n = (isI18nReady: boolean) => {
    const [isDriveI18nReady, setIsDriveI18nReady] = useState(false);

    useEffect(() => {
        if (!isI18nReady) return;
        i18n.addResourceBundle("en-US", "translation", enUS, true, true);
        setIsDriveI18nReady(true);
    }, [isI18nReady]);

    return isDriveI18nReady;
};
