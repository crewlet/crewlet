/**
 * Which of Crewlet's own services the company runs, as the engine says it.
 *
 * # Three services
 *
 * A company runs Crewlet Works (the engine's own tracker: projects, tasks,
 * the Inbox's updates and My work) or another tracker in its place, Jira, or
 * none at all; Crewlet Knowledge (the engine's own pages) or Confluence, or
 * none; and it hosts its git repositories in the engine (`code.native`, the
 * Code workspace) or does not. The engine resolves which from the company
 * document and hands it to every reader on the org projection (`services`,
 * `contract/services.ts`): the socket's first frame, every `org` push and
 * `GET /org`. So nothing here probes for it, and a company that switches a
 * backend is drawn anew on the push that publishes it.
 *
 * # What the dashboard does with it
 *
 * A place that reads a service the company does not run is HIDDEN, never
 * locked (`app/nav.ts`'s `serviceShown`): the sidebar row, the palette's
 * destination, the `g` chord, the Home widget, the control that would file a
 * task. There is nothing to ask an admin for, and a lock would say there was.
 * Access the reader lacks is the other case and keeps its rule: a lock.
 *
 * # Until the engine has said
 *
 * The projection names no services only on a node running no company, which
 * is "not known yet" and never "none": NOTHING IS HIDDEN then, so the frame
 * draws as it always has rather than taking places away on a guess. And a
 * hidden service's address draws its notice (`app/ServiceNotice.tsx`) only
 * once the engine HAS said ([useServicesKnown]), so a deep link is never
 * refused on a guess either.
 */

import type { ServicesRun } from "~/app/nav.ts";
import type { OrgServices } from "~/contract/services.ts";
import { useOrg } from "./store-hooks.ts";

export type { Service, ServicesRun } from "~/app/nav.ts";
export type { OrgServices } from "~/contract/services.ts";

/**
 * Every answer, each ONE object, so a memo keyed on what runs moves only when
 * the engine's answer does rather than on every render of the reader. Built
 * once over every combination, keyed on the three in order.
 */
const RUN = new Map<string, ServicesRun>();
for (const tracker of [true, false]) {
  for (const knowledge of [true, false]) {
    for (const code of [true, false]) {
      RUN.set(runKey(tracker, knowledge, code), { tracker, knowledge, code });
    }
  }
}

/** The key of one combination in [RUN]. */
function runKey(tracker: boolean, knowledge: boolean, code: boolean): string {
  return `${tracker}/${knowledge}/${code}`;
}

/** The one object for a combination. */
function run(tracker: boolean, knowledge: boolean, code: boolean): ServicesRun {
  return RUN.get(runKey(tracker, knowledge, code))!;
}

/** Which services run, from what the engine said: every one, until it has said. */
export function servicesRun(services: OrgServices | null | undefined): ServicesRun {
  if (!services) return run(true, true, true);
  return run(
    services.tracker === "native",
    services.knowledge === "native",
    services.code === "native",
  );
}

/** What the engine said the company runs, or null until it has. */
export function useBackends(): OrgServices | null {
  return useOrg()?.services ?? null;
}

/** Whether the company runs each of Crewlet's own services: every one, until the engine says. */
export function useServicesRun(): ServicesRun {
  return servicesRun(useBackends());
}

/**
 * Which of Crewlet's services may be ASKED anything: none until the org
 * projection has arrived, then those the company runs (every one where the
 * projection names none, a node running no company).
 *
 * DRAWING AND ASKING DIFFER ON A COLD LOAD. Until the projection lands, a
 * place is drawn as if every service ran ([useServicesRun]), so nothing is
 * hidden on a guess; but a read sent then for a service the company does not
 * run is refused (`unknown_query`), and a tab opened on Jira's company asked
 * the tracker for its inbox, its queue and its projects before it knew. A
 * read waits for the projection instead.
 */
export function useServicesAsk(): ServicesRun {
  const org = useOrg();
  return org === null ? run(false, false, false) : servicesRun(org.services ?? null);
}

/** Whether the org projection has arrived, so what the company runs is no longer a guess. */
export function useOrgArrived(): boolean {
  return useOrg() !== null;
}

/** Whether the engine has said what the company runs. */
export function useServicesKnown(): boolean {
  return useBackends() !== null;
}
