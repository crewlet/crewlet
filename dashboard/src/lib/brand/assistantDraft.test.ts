// @vitest-environment jsdom

/**
 * A logo the assistant proposes is a file of the person's own conversation:
 * the brand edit names it at once, and its bytes, fetched through the owner's
 * route once the change is approved, follow as the file a person would have
 * picked; bytes that cannot be had put the slot back and say why.
 */

import { describe, expect, test, vi } from "vitest";
import { eventually } from "~/test/settle.ts";
import { parseDraftChange } from "~/app/assistant/draft.ts";
import type { BrandDocument } from "~/protocol/index.ts";
import { applyBrandDraft, brandDraftFields, type BrandDraft } from "./assistantDraft.ts";
import type { BrandEdit } from "./draft.ts";

const DOC: BrandDocument = {
  v: 1,
  name: "FairSplit",
  logos: { primary: "logos/primary.svg" },
  colors: { primary: "#0f7b5f" },
  type: { heading: { kit: "geist" }, body: { kit: "geist" } },
  voice: {},
};
const EDIT: BrandEdit = { document: DOC, files: new Map() };
const REF = {
  kind: "file" as const,
  file_id: "0b7f1c3e-8a4d-4e2b-9c1a-3f5e6d7a8b9c",
  conversation_id: "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f",
  name: "mark.svg",
  media_type: "image/svg+xml",
};

/** A brand edit held as React holds it, with a setter that takes an updater. */
function held(start: BrandEdit) {
  let edit = start;
  return {
    get: () => edit,
    set: (change: (e: BrandEdit) => BrandEdit) => {
      edit = change(edit);
    },
  };
}

function proposed(): BrandDraft {
  const spec = {
    kind: "client" as const,
    scope: "s",
    value: EDIT as BrandDraft,
    fields: brandDraftFields(new Set(["logos"] as const)),
    apply: () => {},
  };
  const parsed = parseDraftChange(spec, { values: { "logos.mark": REF } });
  if (!parsed.ok) throw new Error(parsed.reason);
  return parsed.value.next;
}

describe("a proposed logo", () => {
  test("is named at once and its bytes follow as a picked file, typed from the reference", async () => {
    const edit = held(EDIT);
    const report = vi.fn();
    const fetchFile = vi.fn(async () => new Blob(["<svg/>"], { type: "text/html" }));
    applyBrandDraft(EDIT, proposed(), edit.set, report, fetchFile);
    expect(edit.get().document.logos.mark).toBe("logos/mark.svg");
    expect(fetchFile).toHaveBeenCalledWith(REF);
    await eventually(() => expect(edit.get().files.get("logos/mark.svg")).toBeDefined());
    const file = edit.get().files.get("logos/mark.svg")!;
    expect([file.name, file.type]).toEqual(["mark.svg", "image/svg+xml"]);
    expect(report).not.toHaveBeenCalled();
  });

  test("puts the slot back and says why when its bytes cannot be had", async () => {
    const edit = held(EDIT);
    const report = vi.fn();
    applyBrandDraft(EDIT, proposed(), edit.set, report, () => Promise.reject(new Error("404")));
    await eventually(() => expect(report).toHaveBeenCalled());
    expect(edit.get().document.logos.mark).toBeUndefined();
    expect(edit.get().files.size).toBe(0);
  });

  test("is not a field of a card that is not open", () => {
    const spec = {
      kind: "client" as const,
      scope: "s",
      value: EDIT as BrandDraft,
      fields: brandDraftFields(new Set(["colours"] as const)),
      apply: () => {},
    };
    expect(parseDraftChange(spec, { values: { "logos.mark": REF } }).ok).toBe(false);
  });
});

describe("a proposed logo's card row", () => {
  test("is previewed on the ground its slot is made for, in the draft's own colour", () => {
    const doc = {
      ...DOC,
      colors: { ...DOC.colors, dark_ground: "#0d1717", light_ground: "#f7faf9" },
    };
    const spec = {
      kind: "client" as const,
      scope: "s",
      value: { document: doc, files: new Map() } as BrandDraft,
      fields: brandDraftFields(new Set(["logos"] as const)),
      apply: () => {},
    };
    const parsed = parseDraftChange(spec, {
      values: { "logos.on_dark": REF, "logos.primary": REF },
    });
    if (!parsed.ok) throw new Error(parsed.reason);
    const [dark, light] = parsed.value.rows;
    expect(dark?.fileGround).toEqual({ tone: "dark", color: "#0d1717" });
    expect(light?.fileGround).toEqual({ tone: "light", color: "#f7faf9" });
  });
});
