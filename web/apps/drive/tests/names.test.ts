import { expect, test } from "vitest";
import { fileCategory } from "../src/services/file-types";
import { normalizeItemName, uniqueName } from "../src/services/names";

test.each([
    ["Report.pdf", [], "Report.pdf"],
    ["Report.pdf", ["report.PDF"], "Report (1).pdf"],
    ["Report.pdf", ["Report.pdf", "Report (1).pdf"], "Report (2).pdf"],
    ["Report (1).pdf", ["Report (1).pdf"], "Report (2).pdf"],
    ["Notes", ["Notes"], "Notes (1)"],
    [".bashrc", [".bashrc"], ".bashrc (1)"],
    ["backup.tar.gz", ["backup.tar.gz"], "backup (1).tar.gz"],
    ["a.b.c", ["a.b.c"], "a.b (1).c"],
    ["Café", ["Café"], "Café (1)"],
])("uniqueName(%j, %j) is %j", (name, taken, expected) => {
    expect(uniqueName(name, taken)).toBe(expected);
});

test("folder names have no extension", () => {
    expect(uniqueName("v1.2", ["V1.2"], "folder")).toBe("v1.2 (1)");
    expect(uniqueName("a.tar.gz", ["a.tar.gz"], "folder")).toBe("a.tar.gz (1)");
});

test("normalizes names", () => {
    expect(normalizeItemName("  Plans  ")).toBe("Plans");
    expect(normalizeItemName("Cafe\u0301")).toBe("Café");
    expect(normalizeItemName("é".repeat(127))).toBe("é".repeat(127));
    for (const bad of [
        "",
        "   ",
        ".",
        "..",
        "a/b",
        "a\\b",
        "a\0b",
        "é".repeat(128),
    ])
        expect(normalizeItemName(bad)).toBeUndefined();
});

test("file categories ignore inherited object keys", () => {
    for (const name of ["a.constructor", "a.__proto__", "a.toString"])
        expect(fileCategory(name)).toBe("other");
});

test.each([
    ["Thesis.docx", undefined, "document"],
    ["budget.XLSX", undefined, "spreadsheet"],
    ["deck.key", undefined, "presentation"],
    ["scan.pdf", undefined, "pdf"],
    ["IMG_0001.HEIC", undefined, "image"],
    ["clip", "video/mp4", "video"],
    ["song.flac", undefined, "audio"],
    ["src.zip", undefined, "archive"],
    ["main.rs", undefined, "code"],
    ["README.md", undefined, "text"],
    ["notes", "text/plain", "text"],
    ["blob.bin", "application/octet-stream", "other"],
    [".env", undefined, "other"],
])("fileCategory(%j, %j) is %j", (name, mime, expected) => {
    expect(fileCategory(name, mime)).toBe(expected);
});
