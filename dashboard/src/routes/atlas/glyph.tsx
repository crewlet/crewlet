/**
 * An entry's kind as its glyph. A module of its own, with no sheet behind it,
 * so a screen outside the atlas (a task naming the entries it touches) draws
 * an entry's kind without taking the atlas's styles into its chunk.
 */

import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import type { AtlasKind } from "~/contract/atlas.ts";
import { KINDS } from "./words.ts";

/** The kind's glyph, decorative: the kind word is always said beside it. */
export function KindGlyph({ kind, size = "sm" }: { kind: AtlasKind; size?: "xs" | "sm" | "md" }) {
  return <NamedGlyph name={KINDS[kind].glyph} size={size} />;
}
