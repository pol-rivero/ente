import type { ItemKind } from "./model/types";

const compoundExtensions = [".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"];

const splitFileName = (name: string): [string, string] => {
    const lower = name.toLowerCase();
    const compound = compoundExtensions.find(
        (ext) => lower.endsWith(ext) && lower.length > ext.length,
    );
    if (compound) {
        const at = name.length - compound.length;
        return [name.slice(0, at), name.slice(at)];
    }
    const dot = name.lastIndexOf(".");
    // A leading dot (".bashrc") starts the name, not an extension.
    return dot > 0 ? [name.slice(0, dot), name.slice(dot)] : [name, ""];
};

export const nameKey = (name: string) => name.normalize("NFC").toLowerCase();

// Returns `name`, or the first free "name (n).ext" if `name` is taken.
// Comparison ignores case, since folders get downloaded to file systems
// that do. Folder names have no extension ("v1.2" → "v1.2 (1)").
export const uniqueName = (
    name: string,
    takenNames: Iterable<string>,
    kind: ItemKind = "file",
) => {
    const taken = new Set([...takenNames].map(nameKey));
    if (!taken.has(nameKey(name))) return name;
    const [base, extension] = kind == "file" ? splitFileName(name) : [name, ""];
    const numbered = /^(.*) \((\d+)\)$/.exec(base);
    const stem = numbered ? numbered[1]! : base;
    for (let n = numbered ? Number(numbered[2]) + 1 : 1; ; n++) {
        const candidate = `${stem} (${n})${extension}`;
        if (!taken.has(nameKey(candidate))) return candidate;
    }
};

// Most file systems cap a name at 255 bytes.
const maxNameBytes = 255;

export const normalizeItemName = (name: string) => {
    const trimmed = name.trim().normalize("NFC");
    if (!trimmed || trimmed == "." || trimmed == "..") return undefined;
    if (/[/\\\0]/.test(trimmed)) return undefined;
    if (new TextEncoder().encode(trimmed).length > maxNameBytes)
        return undefined;
    return trimmed;
};
