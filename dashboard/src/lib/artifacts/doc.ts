/**
 * A doc as the dashboard reads and edits it: where a reader is (the tab, as a
 * presence region), which pictures each tab shows, and a person's save as the
 * one exact edit the engine applies to the tab's file.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives: what a
 * save sends and what following moves are held by tests rather than a screen.
 */

import { imageSources } from "../markdown.ts";
import { resolveVersionPath } from "./versionImages.ts";
import type { ArtifactDoc, DocTab } from "~/protocol/index.ts";

/** A presence region naming the tab a member is on. */
export function tabRegion(path: string): string {
  return `tab:${path}`;
}

/** The tab a presence region names, or "" for a region that names none. */
export function tabOfRegion(region: string | undefined): string {
  return region?.startsWith("tab:") ? region.slice("tab:".length) : "";
}

/** The tab a `doctab=` address opens, the first when it names none of the doc's. */
export function currentTab(doc: ArtifactDoc, path: string): DocTab {
  return doc.tabs.find((t) => t.path === path) ?? doc.tabs[0]!;
}

/** Every picture the doc's tabs show, as version paths, once each. */
export function docImagePaths(doc: ArtifactDoc): string[] {
  const out = new Set<string>();
  for (const tab of doc.tabs) {
    for (const src of imageSources(tab.text)) {
      const p = resolveVersionPath(tab.path, src);
      if (p) out.add(p);
    }
  }
  return [...out];
}

/** How a tab's image sources are shown: through the version's loaded pictures, by path. */
export function tabImages(
  tab: Pick<DocTab, "path">,
  loaded: (path: string) => string | null,
): (src: string) => string | null {
  return (src) => {
    const p = resolveVersionPath(tab.path, src);
    return p ? loaded(p) : null;
  };
}

/** One exact replacement in one file, as the engine's `edits` take it. */
export interface DocEdit {
  path: string;
  old_str: string;
  new_str: string;
}

/**
 * The one edit that turns a tab's `base` text into `draft`: the changed span,
 * widened a line at a time on each side until it occurs exactly once in
 * `base`, since the engine applies an edit only where its text is unique. A
 * save therefore sends the passage a person changed, not the whole tab.
 *
 * Null when nothing changed, and when `base` is empty, which no edit can
 * address (an edit names text to replace, and there is none).
 */
export function editOf(path: string, base: string, draft: string): DocEdit | null {
  if (base === draft || base === "") return null;
  let start = 0;
  const limit = Math.min(base.length, draft.length);
  while (start < limit && base[start] === draft[start]) start++;
  let end = 0;
  while (end < limit - start && base[base.length - 1 - end] === draft[draft.length - 1 - end]) {
    end++;
  }
  // WHOLE CHARACTERS: a span that starts or ends inside a surrogate pair would
  // send half a character, which the engine refuses as text that is not UTF-8.
  if (start > 0 && isLowSurrogate(base.charCodeAt(start))) start--;
  if (end > 0 && isHighSurrogate(base.charCodeAt(base.length - end - 1))) end--;
  let from = start;
  let to = base.length - end;
  for (;;) {
    const old = base.slice(from, to);
    if (old !== "" && unique(base, old)) {
      return {
        path,
        old_str: old,
        new_str: draft.slice(from, draft.length - (base.length - to)),
      };
    }
    if (from === 0 && to === base.length) return null;
    from = lineStartBefore(base, from);
    to = lineEndAfter(base, to);
  }
}

function unique(text: string, part: string): boolean {
  const first = text.indexOf(part);
  return first >= 0 && text.indexOf(part, first + 1) < 0;
}

/** The start of the line before `at`, or of the line `at` is inside: one line wider. */
function lineStartBefore(text: string, at: number): number {
  if (at === 0) return 0;
  const prev = text.lastIndexOf("\n", at - 2);
  return prev < 0 ? 0 : prev + 1;
}

/** The end of the line after `at`, including its newline: one line wider. */
function lineEndAfter(text: string, at: number): number {
  if (at >= text.length) return text.length;
  const next = text.indexOf("\n", at);
  return next < 0 ? text.length : next + 1;
}

function isHighSurrogate(c: number): boolean {
  return c >= 0xd800 && c <= 0xdbff;
}

function isLowSurrogate(c: number): boolean {
  return c >= 0xdc00 && c <= 0xdfff;
}
