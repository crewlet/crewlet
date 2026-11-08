/**
 * Which of Crewlet's own services the company runs, as the engine says it.
 *
 * # Four services
 *
 * A company runs Worklet (the engine's own tracker: projects, tasks, the
 * Inbox's updates and My work) or another tracker in its place, Jira, or none
 * at all; Doclet (the engine's own knowledge base: spaces and pages) or
 * Confluence, or none; Chatlet (the engine's own rooms) or the Mattermost or
 * Slack it declares, or none; and it hosts its git repositories in Gitlet
 * (the engine's own git host, `code.backend: gitlet`) or does not. The engine resolves which
 * from the company document and hands it to every reader on the org
 * projection (`services`, `contract/services.ts`): the socket's first frame,
 * every `org` push and `GET /org`. So nothing here probes for it, and a
 * company that switches a backend is drawn anew on the push that publishes it.
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

import type { Service, ServicesRun } from "~/app/nav.ts";
import type { OrgServices } from "~/contract/services.ts";
import { useEffect } from "react";
import { useOrg } from "./store-hooks.ts";
import { STORAGE_KEYS } from "./storage.ts";

export type { Service, ServicesRun } from "~/app/nav.ts";
export type { OrgServices } from "~/contract/services.ts";

/** Each of Crewlet's own services by its product name, as every screen says it. */
export const SERVICE_NAMES: Readonly<Record<Service, string>> = {
  tracker: "Worklet",
  knowledge: "Doclet",
  chat: "Chatlet",
  code: "Gitlet",
};

/**
 * Every answer, each ONE object, so a memo keyed on what runs moves only when
 * the engine's answer does rather than on every render of the reader. Built
 * once over every combination, keyed on the four in order.
 */
const RUN: ReadonlyMap<string, ServicesRun> = new Map(
  [true, false].flatMap((tracker) =>
    [true, false].flatMap((knowledge) =>
      [true, false].flatMap((chat) =>
        [true, false].map((code): [string, ServicesRun] => [
          runKey(tracker, knowledge, chat, code),
          Object.freeze({ tracker, knowledge, chat, code }),
        ]),
      ),
    ),
  ),
);

/** The key of one combination in [RUN]. */
function runKey(tracker: boolean, knowledge: boolean, chat: boolean, code: boolean): string {
  return `${tracker}/${knowledge}/${chat}/${code}`;
}

/** The one object for a combination. */
function run(tracker: boolean, knowledge: boolean, chat: boolean, code: boolean): ServicesRun {
  return RUN.get(runKey(tracker, knowledge, chat, code))!;
}

/** Which services run, from what the engine said: every one, until it has said. */
export function servicesRun(services: OrgServices | null | undefined): ServicesRun {
  if (!services) return run(true, true, true, true);
  return run(
    services.tracker === "worklet",
    services.knowledge === "doclet",
    services.chat === "chatlet",
    services.code === "gitlet",
  );
}

/** What the engine said the company runs, or null until it has. */
export function useBackends(): OrgServices | null {
  return useOrg()?.services ?? null;
}

/**
 * Whether the company runs each of Crewlet's own services, for DRAWING: what
 * the engine said, or until it has, what this browser last heard it say, and
 * only on a first visit every one. Reads wait for the engine ([useServicesAsk]).
 * A tab the engine `turnedAway` (nobody signed in, or access refused) draws
 * nothing it once heard; the rail, which reads the connection, says so.
 */
export function useServicesRun(turnedAway = false): ServicesRun {
  const org = useOrg();
  return servicesRun(
    org !== null ? (org.services ?? null) : turnedAway ? null : rememberedCompany()?.services,
  );
}

/** The company as this browser last heard it, or null for a first visit. */
interface RememberedCompany {
  name: string;
  services: OrgServices | null;
}

const SERVICE_FIELDS = ["tracker", "knowledge", "chat", "code"] as const;

/** What [rememberCompany] last stored, or null where it is absent, refused or malformed. */
export function rememberedCompany(): RememberedCompany | null {
  try {
    const raw = JSON.parse(localStorage.getItem(STORAGE_KEYS.company) ?? "null") as unknown;
    if (!raw || typeof raw !== "object") return null;
    const { name, services } = raw as { name?: unknown; services?: unknown };
    const ok =
      services === null ||
      (typeof services === "object" &&
        SERVICE_FIELDS.every((f) => typeof (services as Record<string, unknown>)[f] === "string"));
    if (typeof name !== "string" || !ok) return null;
    return { name, services: services as OrgServices | null };
  } catch {
    return null;
  }
}

/** Keep what the engine said, for the next cold load. Never throws. */
export function rememberCompany(company: RememberedCompany): void {
  try {
    localStorage.setItem(STORAGE_KEYS.company, JSON.stringify(company));
  } catch {
    // Storage is refused: the next cold load draws every service, as before.
  }
}

/** A tab the engine turned away keeps nothing it was sent. Never throws. */
function forgetCompany(): void {
  try {
    localStorage.removeItem(STORAGE_KEYS.company);
  } catch {
    // Storage is refused: there is nothing kept to forget.
  }
}

/**
 * The company's name for the rail: the engine's, else the one this browser last
 * heard, else "" (the frame's placeholder). Called by the rail, which remembers,
 * and which forgets what it heard once the engine `turnedAway` the tab.
 */
export function useCompanyName(turnedAway = false): string {
  const org = useOrg();
  const name = org?.name ?? "";
  // WRITTEN WHEN WHAT IT SAYS CHANGES, not on every push of the projection.
  const said = org !== null && name ? JSON.stringify({ name, services: org.services ?? null }) : "";
  useEffect(() => {
    if (turnedAway) forgetCompany();
    else if (said) rememberCompany(JSON.parse(said) as RememberedCompany);
  }, [said, turnedAway]);
  if (org !== null) return name;
  return turnedAway ? "" : (rememberedCompany()?.name ?? "");
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
  return org === null ? run(false, false, false, false) : servicesRun(org.services ?? null);
}

/** Whether the org projection has arrived, so what the company runs is no longer a guess. */
export function useOrgArrived(): boolean {
  return useOrg() !== null;
}

/** Whether the engine has said what the company runs. */
export function useServicesKnown(): boolean {
  return useBackends() !== null;
}

/** A vendor chat surface's name as a person reads it. */
const CHAT_VENDOR_NAMES: Readonly<Record<string, string>> = {
  mattermost: "Mattermost",
  slack: "Slack",
};

/**
 * Where a company's chat is when it is a vendor's, as one phrase: "Slack",
 * "Mattermost and Slack", or "a vendor's chat" where nothing names them.
 *
 * ONE SPELLING FOR EVERY PLACE THAT SAYS IT: the notice an address under Chatlet
 * draws (`app/ServiceNotice.tsx`) and Chatlet's card on Company ›
 * Integrations. The names are the `viewer` answer's `chat_vendors`, told to a
 * reader holding Chatlet; the org projection says only that the chat is a
 * vendor's.
 */
export function chatVendorName(vendors: readonly string[]): string {
  const names = vendors.map((vendor) => CHAT_VENDOR_NAMES[vendor] ?? vendor);
  return names.length ? names.join(" and ") : "a vendor's chat";
}
