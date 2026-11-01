/**
 * The brand setup's rules, read off the draft: when each step is done, where a
 * setup opened again resumes, what a step's Continue refuses, what an adoption
 * still needs, which kit the setup writes, and what the contrast panel offers.
 */

import { describe, expect, test } from "vitest";
import { contrastRatio } from "@crewlethq/ui";
import type { ArtifactListItem, BrandDocument } from "~/protocol/index.ts";
import { brandContrast } from "./brand.ts";
import type { BrandEdit } from "./draft.ts";
import {
  blankBrand,
  draftKit,
  nextStep,
  previousStep,
  resumeStep,
  stepAttention,
  stepDone,
} from "./setup.ts";
import { contrastSuggestions, pairNeeds, pairReason } from "./suggest.ts";
import { adoptionProblem, draftReadiness, partProblem } from "./validate.ts";

const edit = (document: BrandDocument): BrandEdit => ({ document, files: new Map() });

/** A draft with the name, the primary logo and the primary colour: what Colours ends on. */
function complete(over: Partial<BrandDocument> = {}): BrandDocument {
  return {
    ...blankBrand("FairSplit"),
    logos: { primary: "logos/primary.svg" },
    colors: { primary: "#0f7b5f" },
    ...over,
  };
}

function pair(colors: BrandDocument["colors"], foreground: string, ground: string) {
  const found = brandContrast(colors).find(
    (p) => p.foreground === foreground && p.ground === ground,
  );
  if (!found) throw new Error(`no pair ${foreground}/${ground}`);
  return found;
}

describe("each step's Continue", () => {
  test("Identity needs a name, and holds a tagline to its bound and free of links", () => {
    expect(partProblem("identity", edit(blankBrand("")))).toMatch(/Name the brand/);
    expect(partProblem("identity", edit(blankBrand("FairSplit")))).toBeUndefined();
    const linked = { ...blankBrand("FairSplit"), tagline: "See www.example.com" };
    expect(partProblem("identity", edit(linked))).toMatch(/Leave links out/);
    const long = { ...blankBrand("FairSplit"), tagline: "x".repeat(141) };
    expect(partProblem("identity", edit(long))).toMatch(/141 bytes and the most is 140/);
  });

  test("Logos needs the primary logo, and nothing else", () => {
    expect(partProblem("logos", edit(blankBrand("FairSplit")))).toBe(
      "Add the primary logo, for light grounds.",
    );
    expect(partProblem("logos", edit(complete()))).toBeUndefined();
  });

  test("Colours needs the primary colour, and holds only the pair that blocks an adoption", () => {
    expect(partProblem("colours", edit(blankBrand("FairSplit")))).toBe(
      "Choose the primary colour.",
    );
    // A LIGHT PRIMARY IS LOW ON A LIGHT PAGE, and allowed.
    expect(partProblem("colours", edit(complete({ colors: { primary: "#00baba" } })))).toBe(
      undefined,
    );
    // GREY TEXT ON THE LIGHT GROUND blocks.
    expect(
      partProblem("colours", edit(complete({ colors: { primary: "#0f7b5f", text: "#9aa3ad" } }))),
    ).toMatch(/must reach 4\.5:1/);
  });
});

describe("done, resume and the rail", () => {
  test("a fresh setup starts on Identity and a named draft resumes on Logos", () => {
    expect(resumeStep(edit(blankBrand("")))).toBe("identity");
    expect(resumeStep(edit(blankBrand("FairSplit")))).toBe("logos");
  });

  test("a draft with a logo and a colour resumes on Typography, and with every step done on Review", () => {
    expect(resumeStep(edit(complete()))).toBe("type");
    const whole = complete({
      type: { heading: { stack: "system-serif" }, body: { kit: "geist" } },
      voice: { summary: "Plain and warm." },
    });
    expect(stepDone("type", edit(whole))).toBe(true);
    expect(stepDone("voice", edit(whole))).toBe(true);
    expect(resumeStep(edit(whole))).toBe("review");
  });

  test("Review is never done, and the steps walk in order without falling off either end", () => {
    expect(stepDone("review", edit(complete()))).toBe(false);
    expect(previousStep("identity")).toBe("identity");
    expect(nextStep("voice")).toBe("review");
    expect(nextStep("review")).toBe("review");
  });

  test("Colours needs attention while a low pair stands, and Logos while a file was refused", () => {
    expect(stepAttention("colours", edit(complete()), false)).toBe(false);
    expect(
      stepAttention("colours", edit(complete({ colors: { primary: "#00baba" } })), false),
    ).toBe(true);
    expect(stepAttention("logos", edit(complete()), true)).toBe(true);
    expect(stepAttention("identity", edit(complete()), true)).toBe(false);
  });
});

describe("publishing a draft", () => {
  test("names what a draft lacks, in the engine's order, then the pair that refuses", () => {
    expect(adoptionProblem(blankBrand("FairSplit"))).toBe("Add a primary logo and colour.");
    expect(adoptionProblem(complete({ logos: {} }))).toBe("Add a primary logo.");
    expect(adoptionProblem(complete({ colors: {} }))).toBe("Add a primary colour.");
    expect(adoptionProblem(complete({ colors: { primary: "#0f7b5f", text: "#9aa3ad" } }))).toMatch(
      /must reach 4\.5:1/,
    );
    expect(adoptionProblem(complete())).toBeUndefined();
  });

  test("says where a draft is offered that it is not ready, and nothing for one that is", () => {
    expect(draftReadiness(blankBrand("FairSplit"))).toBe(
      "Not ready to publish: add a primary logo and colour.",
    );
    expect(draftReadiness(complete())).toBeUndefined();
  });
});

describe("the kit a setup writes", () => {
  const row = (over: Partial<ArtifactListItem>) =>
    ({ id: "k", kind: "brand", status: "live", role: "owner", ...over }) as ArtifactListItem;

  test("is the newest live brand kit the reader may write, and any for an administrator", () => {
    const kits = [
      row({ id: "doc", kind: "doc" }),
      row({ id: "gone", status: "trashed" }),
      row({ id: "seen", role: "viewer" }),
      row({ id: "mine", role: "editor" }),
    ];
    expect(draftKit(kits, false)?.id).toBe("mine");
    expect(draftKit(kits, true)?.id).toBe("seen");
    expect(draftKit([row({ role: "viewer" })], false)).toBeNull();
  });
});

describe("the contrast panel's suggestions", () => {
  test("a light primary is offered the nearest shade, the shade for text links, and dark grounds instead", () => {
    const colors = { primary: "#00baba" };
    const low = pair(colors, "primary", "light_ground");
    expect(pairReason(low)).toBe("Hard to see as links and buttons on light pages.");
    expect(pairNeeds(low)).toBe("needs 3:1");
    const offers = contrastSuggestions(colors, low);
    expect(offers.map((o) => o.key)).toEqual([
      "primary/light_ground:shade",
      "primary/light_ground:text",
      "primary/light_ground:move",
    ]);
    // MEASURED, never the spec's example: each shade meets what it says.
    const shade = offers[0]!.set.primary!;
    expect(contrastRatio(shade, low.groundColor)!).toBeGreaterThanOrEqual(low.min);
    expect(offers[0]!.label).toBe(shade);
    expect(offers[0]!.detail).toMatch(/^[\d.]+:1, meets the minimum$/);
    expect(offers[0]!.color).toBe(shade);
    const text = offers[1]!.set.primary!;
    expect(contrastRatio(text, low.groundColor)!).toBeGreaterThanOrEqual(4.5);
    expect(offers[1]!.label).toBe(text);
    expect(offers[1]!.detail).toMatch(/^[\d.]+:1, best for text links$/);
    expect(offers[2]).toEqual({
      key: "primary/light_ground:move",
      label: "Use on dark grounds",
      detail: "Keep #00baba, use it on dark grounds",
      set: { primary_on_dark: "#00baba", primary: shade },
    });
  });

  test("the pair that blocks is offered a darker text, and a pair that passes nothing", () => {
    const colors = { primary: "#0f7b5f", text: "#9aa3ad" };
    const blocking = pair(colors, "text", "light_ground");
    const [offer] = contrastSuggestions(colors, blocking);
    expect(contrastRatio(offer!.set.text!, blocking.groundColor)!).toBeGreaterThanOrEqual(4.5);
    expect(contrastSuggestions(colors, pair(colors, "primary", "light_ground"))).toEqual([]);
  });
});
