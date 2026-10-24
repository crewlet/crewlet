/**
 * The brand as the dashboard reads and writes it: the contrast the edit cards
 * hold Save to (the engine's pairs on the engine's effective colours), the
 * `brand.json` a save publishes, a version read back, and what the adopt
 * dialog lists as changed.
 */

import { describe, expect, test } from "vitest";
import { contrast, parseHex } from "@crewlethq/tokens";
import { BRAND_COLOR_FALLBACKS, BRAND_CONTRAST_PAIRS } from "~/contract/artifacts.ts";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  BRAND_COLOR_ROLES,
  brandChanges,
  brandContrast,
  effectiveColors,
  faceDrawing,
  parseBrandDocument,
  refusalWords,
  refusingPair,
  serializeBrandDocument,
  warningPairs,
} from "./brand.ts";

const ratio = (a: string, b: string) => contrast(parseHex(a)!, parseHex(b)!);

function doc(over: Partial<BrandDocument> = {}): BrandDocument {
  return {
    v: 1,
    name: "FairSplit",
    logos: { primary: "logos/primary.svg" },
    colors: { primary: "#0f7b5f" },
    type: { heading: { kit: "geist" }, body: { kit: "geist" } },
    voice: {},
    ...over,
  };
}

describe("the contrast every adoption is held to", () => {
  test("an unset role takes the engine's fallback, and the accent on dark falls back to the primary", () => {
    const e = effectiveColors({ primary: "#0f7b5f" });
    expect(e.text).toBe(BRAND_COLOR_FALLBACKS.text);
    expect(e.light_ground).toBe(BRAND_COLOR_FALLBACKS.light_ground);
    expect(e.dark_ground).toBe(BRAND_COLOR_FALLBACKS.dark_ground);
    expect(e.dark_text).toBe(BRAND_COLOR_FALLBACKS.dark_text);
    expect(e.primary_on_dark).toBe("#0f7b5f");
  });

  test("the label on the primary is whichever of white and black reads better on it", () => {
    expect(effectiveColors({ primary: "#0f7b5f" }).on_primary).toBe("#ffffff");
    expect(effectiveColors({ primary: "#f5d90a" }).on_primary).toBe("#000000");
  });

  test("every pair the engine measures is measured, in the engine's order, unrounded", () => {
    const results = brandContrast({ primary: "#0f7b5f", text: "#111418", light_ground: "#ffffff" });
    expect(results.map((r) => `${r.foreground}/${r.ground}`)).toEqual(
      BRAND_CONTRAST_PAIRS.map((p) => `${p.foreground}/${p.ground}`),
    );
    expect(results[0]!.ratio).toBe(ratio("#111418", "#ffffff"));
    expect(results[0]!.label).toBe("Text on light ground");
    expect(results[1]!.label).toBe("White on primary");
    expect(results.every((r) => r.scale === (r.min >= 4.5 ? "body" : "large"))).toBe(true);
  });

  test("text that does not read on the light ground refuses, with the engine's bound in the reason", () => {
    const results = brandContrast({ primary: "#0f7b5f", text: "#9aa3ad", light_ground: "#ffffff" });
    const refusing = refusingPair(results);
    expect(refusing?.foreground).toBe("text");
    expect(refusalWords(refusing!)).toMatch(/must reach 4\.5:1/);
    expect(refusalWords(refusing!)).not.toMatch(/\u2014/);
  });

  test("a pair that warns never refuses, and a pair not yet measurable does neither", () => {
    const warns = brandContrast({ primary: "#c8f0e0", text: "#111418" });
    expect(refusingPair(warns)).toBeUndefined();
    expect(warningPairs(warns).map((w) => w.foreground)).toContain("primary");
    const unset = brandContrast({ primary: "" });
    expect(refusingPair(unset)).toBeUndefined();
    expect(warningPairs(unset).every((w) => w.ratio !== null)).toBe(true);
  });
});

describe("the brand.json a save publishes", () => {
  test("is the format's keys in the format's order, with an empty optional left out", () => {
    const text = serializeBrandDocument(
      doc({
        tagline: "  ",
        colors: { primary: "#0f7b5f", secondary: "", text: "#111418" },
        voice: { summary: " Plain and warm. ", write_like: ["Say it once.", " "], avoid: [] },
      }),
    );
    expect(text.endsWith("}\n")).toBe(true);
    const parsed = JSON.parse(text);
    expect(Object.keys(parsed)).toEqual(["v", "name", "logos", "colors", "type", "voice"]);
    expect(parsed.colors).toEqual({ primary: "#0f7b5f", text: "#111418" });
    expect(parsed.voice).toEqual({ summary: "Plain and warm.", write_like: ["Say it once."] });
  });

  test("a face names exactly one source", () => {
    const text = serializeBrandDocument(
      doc({
        type: {
          heading: {
            family: "Fraunces",
            files: [{ path: "fonts/heading-0.woff2", weight: 600, style: "normal" }],
            licensed: true,
            kit: "",
          },
          body: { stack: "system-serif" },
        },
      }),
    );
    const parsed = JSON.parse(text);
    expect(parsed.type.heading).toEqual({
      family: "Fraunces",
      files: [{ path: "fonts/heading-0.woff2", weight: 600, style: "normal" }],
      licensed: true,
    });
    expect(parsed.type.body).toEqual({ stack: "system-serif" });
  });

  test("reads back as the document it was written from", () => {
    const written = doc({
      tagline: "Split the trip",
      logos: { primary: "logos/primary.svg", mark: "logos/mark.png" },
      colors: { primary: "#0f7b5f", dark_ground: "#0e1116" },
      voice: { summary: "Plain.", avoid: ["Jargon."] },
    });
    expect(parseBrandDocument(serializeBrandDocument(written))).toEqual(written);
  });
});

describe("a version read back", () => {
  test("refuses what no published version holds", () => {
    expect(parseBrandDocument("not json")).toBeNull();
    expect(parseBrandDocument(JSON.stringify({ v: 2, name: "x" }))).toBeNull();
    expect(parseBrandDocument(JSON.stringify({ ...doc(), colors: { primary: "red" } }))).toBeNull();
    expect(parseBrandDocument(JSON.stringify({ ...doc(), logos: {} }))).toBeNull();
  });

  test("keeps nothing the format does not name, and never a colour that is not #rrggbb", () => {
    const read = parseBrandDocument(
      JSON.stringify({
        ...doc(),
        extra: "<script>",
        colors: { primary: "#0f7b5f", text: "url(x)", secondary: "#1d2433" },
      }),
    );
    expect(read).not.toBeNull();
    expect(read).not.toHaveProperty("extra");
    expect(read!.colors).toEqual({ primary: "#0f7b5f", secondary: "#1d2433" });
  });
});

describe("what adopting changes", () => {
  test("each colour with both values, each logo by its bytes, the voice by its lines", () => {
    const before = doc({ voice: { summary: "Plain.", write_like: ["One."] } });
    const after = doc({
      colors: { primary: "#0b6e55", text: "#111418" },
      logos: { primary: "logos/primary.svg", mark: "logos/mark.svg" },
      voice: { summary: "Plain and warm.", write_like: ["One.", "Two."] },
    });
    const changes = brandChanges(before, after, {
      before: { primary: "aaa" },
      after: { primary: "aaa", mark: "bbb" },
    });
    expect(changes).toEqual([
      { key: "logo.mark", label: "Mark", now: "Added" },
      { key: "color.primary", label: "Primary colour", was: "#0f7b5f", now: "#0b6e55" },
      { key: "color.text", label: "Text colour", was: "Not set", now: "#111418" },
      { key: "voice", label: "Voice", now: "2 lines changed" },
    ]);
  });

  test("a replaced logo, a new face, and a first adoption that names everything as new", () => {
    const changes = brandChanges(
      doc(),
      doc({ type: { heading: { stack: "system-serif" }, body: { kit: "geist" } } }),
      { before: { primary: "aaa" }, after: { primary: "path:logos/primary.svg" } },
    );
    expect(changes).toEqual([
      { key: "logo.primary", label: "Primary logo", now: "Replaced" },
      { key: "face.heading", label: "Headings typeface", was: "Geist", now: "System serif" },
    ]);
    const first = brandChanges(null, doc(), { before: {}, after: { primary: "aaa" } });
    expect(first[0]).toEqual({ key: "name", label: "Name", now: "FairSplit" });
  });

  // A LOGO AND A COLOUR SHARE THEIR ROLE'S WORD, so each change names its
  // card and carries a key of its own: two rows both saying "Primary" were
  // ambiguous, and drew two children under one key.
  test("the primary logo and the primary colour changing at once are two distinct rows", () => {
    const changes = brandChanges(doc(), doc({ colors: { primary: "#34b38a" } }), {
      before: { primary: "aaa" },
      after: { primary: "bbb" },
    });
    expect(changes.map((c) => c.label)).toEqual(["Primary logo", "Primary colour"]);
    expect(new Set(changes.map((c) => c.key)).size).toBe(changes.length);
  });

  test("nothing changed lists nothing", () => {
    expect(brandChanges(doc(), doc(), { before: {}, after: {} })).toEqual([]);
  });
});

describe("a face as the Typography card draws it", () => {
  test("a kit face and a system stack are drawn in their own family list", () => {
    expect(faceDrawing({ kit: "geist" }).name).toBe("Geist");
    expect(faceDrawing({ stack: "system-serif" }).stack).toMatch(/serif$/);
  });

  test("an uploaded family is drawn only under the name this page registered it by", () => {
    const face = {
      family: "Fraunces",
      files: [{ path: "f.woff2", weight: 600, style: "normal" as const }],
      licensed: true,
    };
    expect(faceDrawing(face).stack).toBe("");
    expect(faceDrawing(face, "crewlet-brand-heading-1").stack).toMatch(
      /^"crewlet-brand-heading-1", /,
    );
    expect(faceDrawing(face).detail).toBe("Uploaded, 1 file, licensed to embed");
  });
});

test("the Colours card lists every role the format has", () => {
  expect(BRAND_COLOR_ROLES.map((r) => r.role).sort()).toEqual([
    "dark_ground",
    "light_ground",
    "primary",
    "primary_on_dark",
    "secondary",
    "text",
  ]);
});
