import { lowercaseExtension } from "ente-base/file-name";

export type FileCategory =
    | "document"
    | "spreadsheet"
    | "presentation"
    | "pdf"
    | "image"
    | "video"
    | "audio"
    | "archive"
    | "code"
    | "text"
    | "other";

const extensionCategories = new Map<string, FileCategory>();
const register = (category: FileCategory, extensions: string) => {
    for (const ext of extensions.split(" "))
        extensionCategories.set(ext, category);
};
register("document", "doc docx odt rtf pages wpd");
register("spreadsheet", "xls xlsx xlsm ods csv tsv numbers");
register("presentation", "ppt pptx odp key");
register("pdf", "pdf");
register(
    "image",
    "jpg jpeg png gif webp heic heif avif bmp tif tiff svg ico raw cr2 nef arw dng psd",
);
register("video", "mp4 mov m4v mkv webm avi wmv flv mpg mpeg 3gp");
register("audio", "mp3 m4a aac wav flac ogg oga opus wma aiff");
register("archive", "zip rar 7z tar gz tgz bz2 xz zst dmg iso");
register(
    "code",
    "js jsx ts tsx mjs cjs json html htm css scss py rb go rs java kt swift c h cc cpp hpp cs php sh bash zsh sql yaml yml toml xml dart lua",
);
register("text", "txt md markdown log ini cfg conf");

const mimeCategories: [RegExp, FileCategory][] = [
    [/^application\/pdf$/, "pdf"],
    [/^image\//, "image"],
    [/^video\//, "video"],
    [/^audio\//, "audio"],
    [/spreadsheet|ms-excel|text\/csv/, "spreadsheet"],
    [/presentation|ms-powerpoint/, "presentation"],
    [/wordprocessing|msword|opendocument\.text|rtf/, "document"],
    [/zip|x-tar|x-7z|x-rar|gzip|x-bzip|x-xz|zstd/, "archive"],
    [/javascript|typescript|json|xml|x-sh|x-python/, "code"],
    [/^text\//, "text"],
];

export const fileCategory = (name: string, mimeType?: string): FileCategory => {
    const extension = lowercaseExtension(name);
    const byExtension = extension && extensionCategories.get(extension);
    if (byExtension) return byExtension;
    const mime = mimeType?.toLowerCase();
    if (mime) {
        for (const [pattern, category] of mimeCategories)
            if (pattern.test(mime)) return category;
    }
    return "other";
};
