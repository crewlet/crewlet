/**
 * A mark named by a value, and the three drawings the design system has not
 * vendored yet.
 *
 * # One vocabulary
 *
 * Every mark in this dashboard is named by `@crewlethq/icons`' own `GlyphName`,
 * so a navigation table, an attention reason and a work type all spell a mark
 * the way the design system spells it. This module replaced a second set — ~70
 * Feather paths under our own names — and the second set was never the problem
 * a second NAME SET is: two vocabularies mean a translation, a translation
 * means a table, and a table is a place for the two to disagree about which
 * drawing a word means.
 *
 * # And one family
 *
 * The vendored glyphs are Material Symbols: filled drawings on a `0 -960 960
 * 960` grid, at two optical sizes that are different drawings rather than one
 * scaled. A stroked outline beside them is visibly from somewhere else — which
 * is why the three marks below are not ours in any sense except that we are
 * holding them. They are the same Material Symbols, at the same pinned commit,
 * copied verbatim; `src/ui/symbols/README.md` records where each came from and
 * why the vendored set has no word for it, and `NOTICE` carries the Apache-2.0
 * attribution their license asks for.
 *
 * `cssLength` and `glyphOpticalSize` are the design system's own published
 * answer to exactly this: its doc says a drawing that is not a Glyph at all is
 * sized by a consumer choosing between it and a glyph, and that a second
 * reading of what `sm` is worth is how one stops matching the other. So the
 * three below take their size from the same two functions every vendored glyph
 * does, and pick between the 20 px and 24 px drawing by the same rule.
 *
 * # This file shrinks to nothing
 *
 * `LOCAL` is a gap in `@crewlethq/icons`, not a decision, and the package's own
 * README documents the fix as one command there. When it lands, `LOCAL` empties,
 * `MarkName` becomes `GlyphName`, and `markByName` becomes `glyphByName` at
 * every call site. The keys here are upstream's own names so that is a deletion
 * rather than a rename.
 */

import type { ComponentType } from "react";
import {
  cssLength,
  glyphOpticalSize,
  type GlyphName,
  type GlyphProps,
} from "@crewlethq/icons/glyphs";
import { glyphByName } from "@crewlethq/icons/glyphs/registry";

/**
 * The drawings `@crewlethq/icons` does not vendor, by their upstream names.
 *
 * Both optical sizes, because the axis is a different drawing: the 20 px one is
 * a lighter stroke on the 960 grid than the 24 px one, so rendering either at
 * both sizes reads as the wrong weight beside a glyph that picked correctly.
 */
export const LOCAL_DRAWINGS = {
  star: {
    20: "m352-293 128-76 129 76-34-144 111-95-147-13-59-137-59 137-147 13 112 95-34 144ZM243-144l63-266L96-589l276-24 108-251 108 252 276 23-210 179 63 266-237-141-237 141Zm237-333Z",
    24: "m354-287 126-76 126 77-33-144 111-96-146-13-58-136-58 135-146 13 111 97-33 143ZM233-120l65-281L80-590l288-25 112-265 112 265 288 25-218 189 65 281-247-149-247 149Zm247-350Z",
  },
  /**
   * THE KEPT STATE IS ITS OWN DRAWING, not the outline with a fill applied.
   * That was our Feather star's trick and it worked because a stroked outline
   * has an inside to fill; a Material Symbol is already a filled path, so
   * `fill` on it is the colour of the whole mark rather than of its middle.
   * Upstream draws the pair, and `-fill` is the name its own vendoring script
   * spells the fill-1 variant with — the same name `@crewlethq/icons` carries
   * `check_circle-fill`, `error-fill`, `info-fill` and `warning-fill` under.
   */
  "star-fill": {
    20: "m243-144 63-266L96-589l276-24 108-251 108 252 276 23-210 179 63 266-237-141-237 141Z",
    24: "m233-120 65-281L80-590l288-25 112-265 112 265 288 25-218 189 65 281-247-149-247 149Z",
  },
  bug_report: {
    20: "M480-216q48.67 0 83.34-35Q598-286 600-336v-192q2-50-33.5-85t-86-35q-50.5 0-85 35T360-528v192q-1 50 34 85t86 35Zm-72-120h144v-72H408v72Zm0-120h144v-72H408v72Zm72 26Zm0 286q-60 0-109-32.5T302-264H192v-72h96v-60h-96v-72h96v-60h-96v-72h110q8-26 25.8-47.09Q345.6-668.18 369-684l-81-81 51-51 101 100q19.86-5 40.43-5t40.57 5l100-100 51 51-81 81q23 16 40 37t27 47h110v72h-96v60h96v72h-96v60h96v72H658q-20 55-69 87.5T480-144Z",
    24: "M480-200q66 0 113-47t47-113v-160q0-66-47-113t-113-47q-66 0-113 47t-47 113v160q0 66 47 113t113 47Zm-80-120h160v-80H400v80Zm0-160h160v-80H400v80Zm80 40Zm0 320q-65 0-120.5-32T272-240H160v-80h84q-3-20-3.5-40t-.5-40h-80v-80h80q0-20 .5-40t3.5-40h-84v-80h112q14-23 31.5-43t40.5-35l-64-66 56-56 86 86q28-9 57-9t57 9l88-86 56 56-66 66q23 15 41.5 34.5T688-640h112v80h-84q3 20 3.5 40t.5 40h80v80h-80q0 20-.5 40t-3.5 40h84v80H688q-32 56-87.5 88T480-120Z",
  },
  view_column: {
    20: "M144-312v-336q0-29 21.15-50.5T216-720h528q29.7 0 50.85 21.5Q816-677 816-648v336q0 29-21.15 50.5T744-240H216q-29.7 0-50.85-21.5Q144-283 144-312Zm72 0h128v-336H216v336Zm200 0h128v-336H416v336Zm200 0h128v-336H616v336Z",
    24: "M121-280v-400q0-33 23.5-56.5T201-760h559q33 0 56.5 23.5T840-680v400q0 33-23.5 56.5T760-200H201q-33 0-56.5-23.5T121-280Zm79 0h133v-400H200v400Zm213 0h133v-400H413v400Zm213 0h133v-400H626v400Z",
  },
} as const satisfies Record<string, Record<20 | 24, string>>;

/** Short, because every line below reads it. */
const LOCAL = LOCAL_DRAWINGS;

/** A name `@crewlethq/icons` draws, or one of the three it has not vendored. */
export type MarkName = GlyphName | keyof typeof LOCAL;

/** Whether this build draws the name itself rather than asking the package. */
export function isLocalMark(name: MarkName): name is keyof typeof LOCAL {
  return name in LOCAL;
}

function localMark(name: keyof typeof LOCAL): ComponentType<GlyphProps> {
  const drawing = LOCAL[name];
  return function LocalGlyph({ size, title, opsz, ...rest }: GlyphProps) {
    const length = cssLength(size);
    return (
      <svg
        width={length}
        height={length}
        viewBox="0 -960 960 960"
        fill="currentColor"
        // A GLYPH IS DECORATION UNLESS IT IS NAMED, which is the vendored
        // components' own rule: the meaning is carried by the label beside it,
        // and a mark that announces itself in a row that already says the word
        // reads the word twice.
        role={title ? "img" : undefined}
        aria-hidden={title ? undefined : true}
        focusable="false"
        {...rest}
      >
        {title && <title>{title}</title>}
        <path d={drawing[opsz ?? glyphOpticalSize(size)]} />
      </svg>
    );
  };
}

/**
 * The component that draws a name.
 *
 * Reaching `glyphByName` pulls in every vendored glyph, which its own doc calls
 * the right trade where the name really is data — a navigation table, a work
 * type, an attention reason — and this module exists for exactly those.
 */
export function markByName(name: MarkName): ComponentType<GlyphProps> {
  return isLocalMark(name) ? localMark(name) : glyphByName(name);
}

/** A mark drawn from a name, for a caller that has a value rather than a glyph. */
export function Mark({ name, ...rest }: GlyphProps & { name: MarkName }) {
  const Drawing = markByName(name);
  return <Drawing {...rest} />;
}
