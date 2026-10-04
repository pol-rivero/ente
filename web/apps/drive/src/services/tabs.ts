import log from "ente-base/log";

// Coordination between browser tabs of the Drive web app sharing one
// IndexedDB database: one sync at a time, and the others reload what it
// wrote.

type TabMessage = { type: "changed" } | { type: "logout" };

const isBrowserTab = () =>
    typeof window != "undefined" && typeof BroadcastChannel != "undefined";

let channel: BroadcastChannel | undefined;

const ensureChannel = () => {
    if (!isBrowserTab()) return undefined;
    channel ??= new BroadcastChannel("ente-drive");
    return channel;
};

export const postToOtherTabs = (message: TabMessage) => {
    try {
        ensureChannel()?.postMessage(message);
    } catch (e) {
        log.warn("Failed to notify other tabs", e);
    }
};

export const onOtherTabMessage = (handler: (message: TabMessage) => void) => {
    const c = ensureChannel();
    if (!c) return () => undefined;
    const listener = (event: MessageEvent<TabMessage>) => handler(event.data);
    c.addEventListener("message", listener);
    return () => c.removeEventListener("message", listener);
};

export const withTabLock = async <T>(
    name: string,
    task: () => Promise<T>,
): Promise<T> => {
    if (!isBrowserTab() || !("locks" in navigator)) return task();
    const lock = { isGranted: false };
    try {
        return await navigator.locks.request(name, () => {
            lock.isGranted = true;
            return task();
        });
    } catch (e) {
        if (lock.isGranted) throw e;
        log.warn("Web Locks are unavailable, continuing without one", e);
        return task();
    }
};
