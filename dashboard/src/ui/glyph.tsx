/**
 * A mark named by a value.
 *
 * # One vocabulary, and it is the design system's
 *
 * Every mark in this dashboard is named by `@crewlethq/icons`' own `GlyphName`
 * — Lucide's names — so a navigation table, an attention reason and a work type
 * all spell a mark the way the design system spells it. There is no name set
 * of our own and no drawing of our own: a second vocabulary means a
 * translation, a translation means a table, and a table is a place for the two
 * to disagree about which drawing a word means. The drawings this tree once
 * held locally (a star, its kept state, a bug and a board) were a gap in the
 * package, and the package closed it — `star` takes `filled`, and `bug` and
 * `columns-3` are vendored.
 *
 * # Why a component rather than `glyphByName` at every call site
 *
 * `glyphByName` answers the component registered under a name WITH ITS OWN
 * TYPE, so a caller holding a `GlyphName` union gets a union of components it
 * cannot render without narrowing. `Mark` is that narrowing, once: a name, the
 * props every glyph takes, and nothing a single glyph adds. A caller that wants
 * `filled` knows which glyph it is drawing and imports it by name.
 *
 * Reaching `glyphByName` pulls in every vendored glyph, which its own doc calls
 * the right trade where the name really is data — a navigation table, a work
 * type, an attention reason — and this module exists for exactly those.
 */

import type { ComponentType } from "react";
import type { GlyphName, GlyphProps } from "@crewlethq/icons/glyphs";
import { glyphByName } from "@crewlethq/icons/glyphs/registry";

/** The component that draws a name, typed to the props every glyph takes. */
export function glyphFor(name: GlyphName): ComponentType<GlyphProps> {
  return glyphByName(name) as ComponentType<GlyphProps>;
}

/** A mark drawn from a name, for a caller that has a value rather than a glyph. */
export function Mark({ name, ...rest }: GlyphProps & { name: GlyphName }) {
  const Drawing = glyphFor(name);
  return <Drawing {...rest} />;
}
