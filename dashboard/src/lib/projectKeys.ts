/**
 * The keys of the company's Worklet projects, read ONCE for every surface in
 * the frame, so a work item's key written in prose (`GP-66`) is drawn as a
 * link to it (`components/InlineToken.tsx`).
 *
 * WHY A KEY NEEDS ITS PROJECT. A key is only a shape: `ISO-8601`, `UTF-8`,
 * `SHA-256` and `GPT-4` are written exactly like one. A key is a link only
 * when its project is one of these, and every other key-shaped word stays the
 * word it is.
 *
 * WHAT IT READS: the active projects, the engine's own first page of them
 * (`work_projects`, the listing the project switcher reads). A key in an
 * archived project, or in a company with more projects than one page, is
 * drawn as words; it never links to a guess.
 *
 * ONLY FOR A READER WORKLET IS SERVED TO, as every Worklet read in the frame
 * is gated: one without the app could open no item, and a company on another
 * tracker has no Worklet keys at all. None outside the frame, for the reason
 * `lib/linkHosts.ts` gives.
 *
 * NOT POLLED, and asked again when the reader comes back to the tab: a new
 * project is a rare, deliberate act, and the moment it matters to prose is
 * the next time somebody reads it.
 */

import { createContext, createElement, useContext, useMemo, type ReactNode } from "react";
import { useServicesAsk } from "./services.ts";
import { useQuery } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";
import { PROJECT_PAGE } from "./work.ts";

const NONE: ReadonlySet<string> = new Set();

const Keys = createContext<ReadonlySet<string>>(NONE);

export function ProjectKeysProvider({ children }: { children: ReactNode }) {
  const viewer = useViewer();
  const ask = useServicesAsk();
  const { data } = useQuery(
    "work_projects",
    { limit: PROJECT_PAGE },
    { enabled: viewer.worklet && ask.tracker, refetchOnFocus: true },
  );
  const keys = useMemo(
    () => (data?.projects ? new Set(data.projects.map((p) => p.key)) : NONE),
    [data],
  );
  return createElement(Keys.Provider, { value: keys }, children);
}

/** The keys of the company's active projects, or none outside the frame. */
export function useProjectKeys(): ReadonlySet<string> {
  return useContext(Keys);
}
