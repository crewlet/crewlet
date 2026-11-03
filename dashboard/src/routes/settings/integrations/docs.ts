/**
 * Where an integration's documentation is published, for every link the
 * integrations screens draw to it: a tile's Learn more and a section a step
 * points at. One base, so a move of the site is one edit.
 *
 * Each page is `docs/integrations/<doc>.md` in the engine's tree, and each
 * anchor a heading on it, which `app/links.test.ts` holds to the file.
 */

/**
 * The newest release's copy of the docs (`/engine/latest/`; main is
 * `/engine/next/`, an older line `/engine/vX.Y/`). See `docs/index.md`.
 */
const DOCS_BASE = "https://docs.crewlet.ai/engine/latest/";

/** A section of an integration's page: the page's slug and a heading's anchor. */
export interface DocSection {
  doc: string;
  anchor: string;
}

/**
 * The published address of an integration's page, or of one section of it.
 *
 * WITH THE PAGE'S TRAILING SLASH, because every page on the site is a
 * directory: the slashless spelling answers a redirect to this one, a round
 * trip on every click before the page loads.
 */
export function integrationDocHref(doc: string, anchor?: string): string {
  return `${DOCS_BASE}integrations/${doc}/${anchor ? `#${anchor}` : ""}`;
}
