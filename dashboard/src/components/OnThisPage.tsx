/**
 * "On this page": a document's outline beside it, following the scroll. One
 * component for every Markdown document the dashboard draws (a Doclet page,
 * a doc's tab), so a heading is found by the renderer's own walk ([outline])
 * and its entry points at the id [renderMarkdown] gave it.
 */

import { useEffect, useMemo, useState } from "react";
import { Outline, scrollToHeading, useActiveHeading } from "@crewlethq/ui";
import { outline, type Heading } from "~/lib/markdown.ts";
import { scrollerOf } from "~/lib/scroller.ts";

/**
 * Where a heading counts as the one being read: a fifth of the way down the
 * scroller, so the section filling the screen is the one marked.
 */
const READING_LINE = 0.2;

/**
 * The headings a body's outline lists, or none when it would list fewer than
 * two: one entry is not a map of anything. A screen asks this before it gives
 * the outline a column, so a body with no outline is not set beside an empty
 * one.
 */
export function pageOutline(body: string): Heading[] {
  const all = outline(body);
  if (all.length === 0) return [];
  // THE TWO TOP LEVELS THE DOCUMENT USES, its sections and their parts: a
  // third would make the outline the document.
  const top = Math.min(...all.map((h) => h.level));
  const listed = all.filter((h) => h.level <= top + 1);
  return listed.length < 2 ? [] : listed;
}

export function OnThisPage({ body }: { body: string }) {
  const headings = useMemo(() => pageOutline(body), [body]);
  // THE SCROLLER IS THE ONE THAT MOVES THE HEADINGS, found from the first of
  // them once they are drawn: a Doclet page scrolls the shell, a doc
  // scrolls its artifact's own body.
  const [root, setRoot] = useState<HTMLElement | null>(null);
  const first = headings[0]?.id;
  useEffect(() => setRoot(scrollerOf(first ? document.getElementById(first) : null)), [first]);
  const ids = useMemo(() => headings.map((h) => h.id), [headings]);
  const active = useActiveHeading(ids, {
    root,
    offset: Math.round((root?.clientHeight ?? 0) * READING_LINE),
  });
  if (headings.length === 0) return null;
  return (
    <Outline
      label="On this page"
      items={headings.map((h) => ({ id: h.id, label: h.text, level: h.level }))}
      activeId={active}
      onSelect={(id) => scrollToHeading(id)}
    />
  );
}
