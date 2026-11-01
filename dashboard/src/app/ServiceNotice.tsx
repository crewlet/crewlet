/**
 * What an address under a service the company does not run draws, in place.
 *
 * # Said where the reader is, to every reader
 *
 * The Worklet workspace and My work read Worklet, the Doclet workspace reads
 * Doclet, Chatlet reads Chatlet and Code reads the engine's own git host, so
 * where the company runs another tracker, knowledge base or chat, or none, or
 * hosts no code in the engine, every read those pages make is refused
 * (`lib/services.ts`). A star,
 * a recent or a link pasted before the company changed what it runs still
 * opens one. It draws this notice in its place: what the company runs
 * instead, in the engine's word, and why the page asked for is not here.
 *
 * It was a redirect to the service's page on Company › Integrations once,
 * which took a reader away from the address they followed to a page about
 * what the company runs. So the notice is drawn where the address is,
 * readable by anybody signed in, and offers Integrations to a reader who may
 * open it (`closedBy`, the rule every lock is drawn by), which is every reader
 * now: the services' pages are the overview everybody holds. The code host
 * has no page there to offer — it is turned on in the company document, which
 * the notice names instead.
 */

import { ButtonLink, EmptyState } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  CodeGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
} from "@crewlethq/icons/glyphs";
import { chatVendorName, useBackends, type OrgServices } from "~/lib/services.ts";
import { useHeldReading, useViewer } from "~/lib/viewer.ts";
import { closedBy, sectionOf, workspaceOf, workspaceRow, type Service } from "./nav.ts";
import { href } from "./router.tsx";

/** Where the services built into Crewlet are described, and what each runs. */
const INTEGRATIONS_PATH = ["company", "integrations"] as const;

/**
 * Each service's line in the directory, which "See what is built into
 * Crewlet" opens: one per service, so a service with no line is a type error
 * rather than a link to another service's page. The code host has none (null),
 * since it is set in the company document rather than connected, and the
 * sentence under the heading names the setting.
 */
const LINES = {
  tracker: "worklet",
  knowledge: "doclet",
  chat: "chat",
  code: null,
} satisfies Record<Service, string | null>;

/** Each service's mark, the one its workspace's row wears. */
const GLYPHS = {
  tracker: SquareKanbanGlyph,
  knowledge: BookOpenGlyph,
  chat: MessageSquareGlyph,
  code: CodeGlyph,
} satisfies Record<Service, unknown>;

/**
 * The notice's heading: what the company runs in the service's place. A
 * vendor chat is named from `vendors` (the `viewer` answer's, told to a reader
 * holding Chatlet), and called a vendor's to anybody else.
 */
export function noticeTitle(
  service: Service,
  backends: OrgServices,
  vendors: readonly string[] = [],
): string {
  switch (service) {
    case "tracker":
      return backends.tracker === "jira"
        ? "Jira handles this company's work"
        : "This company runs no work tracker";
    case "knowledge":
      return backends.knowledge === "confluence"
        ? "Confluence holds this company's knowledge"
        : "This company runs no knowledge base";
    case "chat":
      return backends.chat === "vendor"
        ? `This company talks on ${chatVendorName(vendors)}`
        : "This company runs no chat";
    case "code":
      // NO VENDOR STANDS IN THE CODE HOST'S PLACE: a company's repositories on
      // GitHub or GitLab are integrations its seats call, and the engine draws
      // none of them here, so the heading says only what is true of every
      // such company.
      return "This company does not host its code here";
  }
}

/** The sentence under it: why the page the address names is not here. */
export function noticeText(service: Service, path: readonly string[]): string {
  if (service === "knowledge") {
    return "Doclet is not in use here. Each agent's diary is still on its Memory tab, under the Org chart.";
  }
  if (service === "chat") {
    return "Chatlet is not in use here.";
  }
  if (service === "code") {
    return "Code shows the git repositories the engine hosts for a company, and this one hosts none: its company document does not set code.native.";
  }
  // A NEW DESIGN IS MADE BY AN AGENT WORKING A TASK, so a reader who asked
  // for one is told that, not only that Jira handles work.
  if (path[0] === "artifacts") {
    return "A new design is made by an agent working a task, and Worklet, where tasks live, is not in use here.";
  }
  // WORKLET'S OWN PAGES ARE THE SERVICE, so they name it once. Elsewhere the
  // place is named as the reader knows it: the workspace where all of it
  // reads Worklet (My work), else the one section that does (Settings ›
  // Expensive tasks).
  const row = workspaceRow(workspaceOf(path));
  if (row?.key === "worklet") return "Worklet is not in use here.";
  const label = (row?.service ? row.label : sectionOf(path)?.label) ?? "This page";
  return `${label} is part of Worklet, which is not in use here.`;
}

export function ServiceNotice({ service, path }: { service: Service; path: readonly string[] }) {
  const backends = useBackends();
  const held = useHeldReading();
  const vendors = useViewer().chatVendors;
  // THE ENGINE HAS SAID, or this would not be drawn (`App.tsx` gates on it);
  // a reading still missing draws nothing rather than a guess.
  if (!backends) return null;
  const line = LINES[service];
  const canOpen = held !== null && closedBy(INTEGRATIONS_PATH, held.grants) === null;
  const Glyph = GLYPHS[service];
  return (
    <EmptyState
      icon={<Glyph size={32} />}
      title={noticeTitle(service, backends, vendors)}
      description={noticeText(service, path)}
      action={
        canOpen && line ? (
          <ButtonLink variant="secondary" href={href([...INTEGRATIONS_PATH, line])}>
            See what is built into Crewlet
          </ButtonLink>
        ) : undefined
      }
    />
  );
}
