/**
 * How a screen describes itself to the frame: the labels its trail needs, the
 * coverage of the answer it drew from, its sections' figures, its phone menu.
 *
 * # Why this is not in `Shell.tsx`
 *
 * It was, and every screen that published a label imported the frame to do
 * it — and the frame imports the palette, both sidebars and the chunk loader,
 * whose `import()` names every screen there is. A screen's own static import
 * graph therefore reached every other workspace, which is harmless to the
 * bundler (an `import()` is a chunk boundary) and fatal to the one question a
 * reviewer of the two shells has to be able to ask: what does a person-shell
 * screen reach? `app/shell.test.ts` answers it by walking that graph, and a
 * walk through the frame answers "everything". So the context a screen writes
 * into lives here, beside nothing, and the frame provides it
 * ([PageContextValue]).
 */

import { createContext, useContext, useEffect, useLayoutEffect, useRef } from "react";
import type { Labels } from "./crumbs.ts";
import type { PageMenuEntry } from "./header/PageHeader.tsx";
import type { CoverageFacts } from "~/components/work.tsx";

/**
 * What a screen tells the frame about itself.
 *
 * A SCREEN NEVER RENDERS CHROME. It publishes what the chrome needs — the
 * labels the route cannot supply, and the coverage of the answer it drew from
 * — and the frame draws both in the same place on every screen. The
 * alternative, which this replaces, is twenty screens each drawing their own
 * header, and five of them quietly dropping the coverage badge.
 *
 * A SCREEN'S CONTROLS DO NOT COME THROUGH HERE. They are a portal —
 * `frame/PageActions.tsx`, whose own doc argues the case — and this interface
 * briefly carried a `setActions` beside it that no screen ever called, so the
 * documented route rendered nothing while the working one sat next door.
 */
export interface PageContext {
  setLabels: (labels: Labels) => void;
  setCoverage: (coverage: CoverageFacts | null) => void;
  setCounts: (counts: Record<string, string>) => void;
  setShowWorking: (show: boolean) => void;
  setMenu: (entries: PageMenuEntry[]) => void;
}

const noop: PageContext = {
  setLabels: () => {},
  setCoverage: () => {},
  setCounts: () => {},
  setShowWorking: () => {},
  setMenu: () => {},
};

/** What the frame provides, and every hook below reads. */
export const PageContextValue = createContext<PageContext>(noop);

/** The frame's own hooks, for a screen to describe itself. */
export function usePageContext(): PageContext {
  return useContext(PageContextValue);
}

/**
 * Publish the labels the breadcrumb needs.
 *
 * A screen calls this with whatever it has resolved — a project's name for its
 * key, a seat's display name for its handle. Until it does, the trail renders
 * the raw segments, which are identifiers a reader can still act on.
 */
export function usePageLabels(labels: Labels): void {
  const { setLabels } = usePageContext();
  const key = JSON.stringify(labels);
  useEffect(() => {
    setLabels(labels);
    // AND THEY GO WHEN THE SCREEN DOES, for the reason `usePageCoverage` below
    // spells out: the Shell is mounted once for the life of the tab and only the
    // screen inside it remounts, so what a screen published is what the frame
    // keeps showing. A label map is keyed on the ROUTE SEGMENT, so a seat's name
    // left behind would title an identically spelled segment on the next screen
    // — a node id, a tool name — as that person. A cleanup here runs in the
    // right order (outgoing destroy before incoming create) and both writes land
    // in one batch, so nothing flickers through the raw segment.
    return () => setLabels({});
    // The labels object is rebuilt every render by most callers, so the
    // dependency is its CONTENT: an identity dependency here is an infinite
    // render loop, and one on `labels` alone would be exactly that.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setLabels]);
}

/**
 * Publish the coverage of the answer this screen was drawn from.
 *
 * THE COVERAGE BELONGS TO THE SCREEN AND THE FRAME OUTLIVES IT, which is why
 * the effect returns a reset. The Shell never unmounts — only the screen
 * inside it does — so without one the inbox's freshness badge and its "this
 * answer is incomplete" banner stayed in the state bar over every screen the
 * reader visited afterwards, as claims about data those screens never read.
 * On Connect › Secrets, which somebody opens precisely to judge whether what
 * they are seeing is trustworthy, that is the worst possible stale value.
 *
 * A reset in the Shell keyed on the route would not do: React flushes a
 * child's effects before a parent's, so it would wipe the coverage the
 * incoming screen had just published. A cleanup here runs in the right order —
 * the outgoing screen's destroy before the incoming screen's create — and
 * both writes land in one batch, so nothing flickers through null.
 */
export function usePageCoverage(coverage: CoverageFacts | null | undefined): void {
  const { setCoverage } = usePageContext();
  const level = coverage?.read_level;
  const complete = coverage?.complete;
  const applied = coverage?.applied_through;
  const seq = coverage?.log_seq;
  useEffect(() => {
    setCoverage(coverage ?? null);
    return () => setCoverage(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [level, complete, applied, seq, setCoverage]);
}

/**
 * Publish each section's figure, for the page header's section tabs.
 *
 * THE SCREEN OWNS THE NUMBER. Only the screen knows whether a count is a
 * total, a floor the engine stopped counting at, or not answered yet — so it
 * hands the header STRINGS it has already written ("12", "20+"), keyed on the
 * section, and omits a section it has no figure for. Cleared when the screen
 * goes, for the reason [usePageLabels] gives: the frame outlives it.
 */
export function useSectionCounts(counts: Record<string, string>): void {
  const { setCounts } = usePageContext();
  const key = JSON.stringify(counts);
  useEffect(() => {
    setCounts(counts);
    return () => setCounts({});
    // The dependency is the CONTENT, for the reason `usePageLabels` gives.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setCounts]);
}

/**
 * Draw "who is working" in the page bar — the Overview's, and nobody else's.
 *
 * ONE PAGE ASKS FOR IT, the way the approved Main artboard draws it: the
 * company's pulse belongs at the head of the page a reader opens to see how
 * the company is. Everywhere else in the console the sidebar's Live row already carries the
 * count, and the chip drawn in every bar was a second copy of it — on a
 * task page or a profile it sat between the trail and the page's own actions,
 * and at a phone's width it pushed them past the edge. Cleared when the screen
 * goes, for the reason [usePageLabels] gives: the frame outlives it.
 */
export function useWorkingNow(): void {
  const { setShowWorking } = usePageContext();
  useEffect(() => {
    setShowWorking(true);
    return () => setShowWorking(false);
  }, [setShowWorking]);
}

/**
 * Put a screen's SECONDARY actions in the page bar's "More" menu on a phone.
 *
 * ON A PHONE'S WIDTH THE BAR KEEPS ONE ACTION IN VIEW — the screen's primary
 * one — and folds the rest into one menu at its end: the frame's own star and
 * Copy link, and whatever a screen publishes here. A screen drawing a
 * secondary control inline (Edit project) marks it `page-action-folds` so the
 * bar hides it at that width, and publishes the same action here so it is
 * still one press away. Laid out whole, a project's bar stood three rows tall
 * on a 390px screen — the trail, the lenses, then four controls — about 105px
 * of chrome above the work.
 *
 * A HOOK, where the controls themselves are a portal: a menu's items are
 * values the kit's `Menu` lays out, not elements a screen could render into
 * it. Every press goes through the entry the screen published LAST, so a
 * callback that closes over newer state is the one that runs; what is laid
 * out is re-published only when a label, a description or a disabled state
 * moves. Cleared when the screen goes, for the reason [usePageLabels] gives.
 */
export function usePageMenu(entries: readonly PageMenuEntry[]): void {
  const { setMenu } = usePageContext();
  const latest = useRef(entries);
  useLayoutEffect(() => {
    latest.current = entries;
  });
  const key = JSON.stringify(
    entries.map((e) => [e.key, e.label, e.disabled ?? false, e.description ?? ""]),
  );
  useEffect(() => {
    setMenu(
      latest.current.map((entry) => ({
        ...entry,
        onSelect: () => latest.current.find((e) => e.key === entry.key)?.onSelect(),
      })),
    );
    return () => setMenu([]);
    // The dependency is the CONTENT, for the reason `usePageLabels` gives.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setMenu]);
}
