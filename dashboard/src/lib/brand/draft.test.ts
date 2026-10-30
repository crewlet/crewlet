/**
 * An edit before it is saved: a picked file refused at the format's bound
 * before any upload, the paths a save names, and the files a save brings,
 * each from the edit or from the adopted brand.
 */

import { describe, expect, test } from "vitest";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  draftSource,
  familyProblem,
  fontPath,
  fontProblem,
  logoPath,
  logoProblem,
  saveFiles,
  type BrandEdit,
} from "./draft.ts";
import type { BrandSource } from "./useBrand.ts";

function file(name: string, size: number, type = ""): File {
  return new File([new Uint8Array(size)], name, { type });
}

const DOC: BrandDocument = {
  v: 1,
  name: "FairSplit",
  logos: { primary: "logos/primary.svg", mark: "logos/mark.png" },
  colors: { primary: "#0f7b5f" },
  type: {
    heading: {
      family: "Fraunces",
      files: [{ path: "fonts/heading-0.woff2", weight: 600, style: "normal" }],
      licensed: true,
    },
    body: { kit: "geist" },
  },
  voice: {},
};

/** The adopted brand as a source whose every file is its own path's bytes. */
function adopted(): BrandSource & { reads: string[] } {
  const reads: string[] = [];
  const paths = ["logos/primary.svg", "logos/mark.png", "fonts/heading-0.woff2"];
  return {
    key: "adopted:abc",
    document: DOC,
    files: new Map(paths.map((p) => [p, { path: p, type: "x", hash: `h-${p}` }])),
    reads,
    read: async (f) => {
      reads.push(f.path);
      return new Blob([f.path]);
    },
  };
}

describe("a picked logo", () => {
  test("is one of the three types, under its bound, saved under its slot's path", () => {
    expect(logoProblem(file("logo.svg", 10, "image/svg+xml"))).toBeNull();
    expect(logoProblem(file("logo.gif", 10, "image/gif"))).toMatch(/SVG, PNG or WebP/);
    expect(logoProblem(file("logo.svg", 600 << 10, "image/svg+xml"))).toMatch(/512 KiB/);
    expect(logoProblem(file("logo.png", 3 << 20, "image/png"))).toMatch(/2 MiB/);
    expect(logoPath("on_dark", file("x.webp", 1, "image/webp"))).toBe("logos/on-dark.webp");
    // THE EXTENSION DECIDES where the browser named no type.
    expect(logoPath("mark", file("x.PNG", 1))).toBe("logos/mark.png");
  });
});

describe("a picked face", () => {
  test("is WOFF2, at most four files of at most 2 MiB, under a name the format admits", () => {
    expect(fontProblem([file("a.woff2", 10)])).toBeNull();
    expect(fontProblem([file("a.ttf", 10)])).toMatch(/not a WOFF2/);
    expect(fontProblem([file("a.woff2", 3 << 20)])).toMatch(/2 MiB/);
    expect(fontProblem([1, 2, 3, 4, 5].map((i) => file(`${i}.woff2`, 1)))).toMatch(/at most 4/);
    expect(fontPath("body", 2)).toBe("fonts/body-2.woff2");
    expect(familyProblem("Inter Display")).toBeNull();
    expect(familyProblem("")).toMatch(/Name the family/);
    expect(familyProblem("Two  spaces")).toMatch(/single spaces/);
    expect(familyProblem("9 Starts with a digit")).toMatch(/starting with a letter/);
    expect(familyProblem("x".repeat(41))).toMatch(/40/);
  });
});

describe("what a save brings", () => {
  test("every file the document names, picked ones from the edit and the rest from the adopted brand", async () => {
    const base = adopted();
    const picked = file("new.svg", 5, "image/svg+xml");
    const edit: BrandEdit = { document: DOC, files: new Map([["logos/primary.svg", picked]]) };
    const files = await saveFiles(base, edit);
    expect(files.map((f) => f.path)).toEqual([
      "logos/primary.svg",
      "logos/mark.png",
      "fonts/heading-0.woff2",
    ]);
    expect(files[0]).toEqual({ path: "logos/primary.svg", body: picked });
    expect(base.reads).toEqual(["logos/mark.png", "fonts/heading-0.woff2"]);
  });

  test("a path neither the edit nor the adopted brand holds is said, never published missing", async () => {
    const edit: BrandEdit = {
      document: { ...DOC, logos: { primary: "logos/primary.svg", on_dark: "logos/on-dark.svg" } },
      files: new Map(),
    };
    await expect(saveFiles(adopted(), edit)).rejects.toThrow(/logos\/on-dark\.svg/);
    await expect(saveFiles(null, edit)).rejects.toThrow(/no file for it/);
  });
});

describe("the edit as a source", () => {
  test("reads a picked file from memory and the rest from the adopted brand, and changes key with a pick", async () => {
    const base = adopted();
    const picked = file("p.png", 3, "image/png");
    const before = draftSource(base, { document: DOC, files: new Map() });
    const after = draftSource(base, {
      document: { ...DOC, logos: { ...DOC.logos, primary: "logos/primary.png" } },
      files: new Map([["logos/primary.png", picked]]),
    });
    expect(after.key).not.toBe(before.key);
    const spec = after.files.get("logos/primary.png")!;
    expect(spec.type).toBe("image/png");
    expect(await after.read(spec, new AbortController().signal)).toBe(picked);
    await after.read(after.files.get("logos/mark.png")!, new AbortController().signal);
    expect(base.reads).toEqual(["logos/mark.png"]);
  });

  test("a name typed changes no key, so the logos are not read again while it is typed", () => {
    const base = adopted();
    const a = draftSource(base, { document: DOC, files: new Map() });
    const b = draftSource(base, { document: { ...DOC, name: "FairSplit Inc" }, files: new Map() });
    expect(b.key).toBe(a.key);
  });
});
