/**
 * What an address under a service the company does not run draws, in place.
 *
 * # Said where the reader is, to every reader
 *
 * Work and My work are Crewlet Works' pages, Knowledge is Crewlet
 * Knowledge's and Code is the engine's own git host's, so where the company
 * runs another tracker or knowledge base, or none, or hosts no code in the
 * engine, every read those pages make is refused (`lib/services.ts`). A star,
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
import { BookOpenGlyph, CodeGlyph, SquareKanbanGlyph } from "@crewlethq/icons/glyphs";
import { useBackends, type OrgServices } from "~/lib/services.ts";
import { useHeldReading } from "~/lib/viewer.ts";
import { closedBy, workspaceOf, workspaceRow, type Service } from "./nav.ts";
import { href } from "./router.tsx";

/** Where the services built into Crewlet are described, and what each runs. */
const INTEGRATIONS_PATH = ["company", "integrations"] as const;

/** The notice's heading: what the company runs in the service's place. */
export function noticeTitle(service: Service, backends: OrgServices): string {
  if (service === "tracker") {
    return backends.tracker === "jira"
      ? "Jira handles this company's work"
      : "This company runs no work tracker";
  }
  // NO VENDOR STANDS IN THE CODE HOST'S PLACE: a company's repositories on
  // GitHub or GitLab are integrations its seats call, and the engine draws
  // none of them here, so the heading says only what is true of every such
  // company.
  if (service === "code") return "This company does not host its code here";
  return backends.knowledge === "confluence"
    ? "Confluence holds this company's knowledge"
    : "This company runs no knowledge base";
}

/** The sentence under it: why the page the address names is not here. */
export function noticeText(service: Service, path: readonly string[]): string {
  if (service === "knowledge") {
    return "Knowledge is part of Crewlet Knowledge, which is not in use here. Each agent's diary is still on its Memory tab, under the Org chart.";
  }
  if (service === "code") {
    return "Code shows the git repositories the engine hosts for a company, and this one hosts none: its company document does not set code.native.";
  }
  // A NEW DESIGN IS MADE BY AN AGENT WORKING A TASK, so a reader who asked
  // for one is told that, not only that Jira handles work.
  if (path[0] === "artifacts") {
    return "A new design is made by an agent working a task, and Crewlet Works, where tasks live, is not in use here.";
  }
  const label = workspaceRow(workspaceOf(path))?.label ?? "This page";
  return `${label} is part of Crewlet Works, which is not in use here.`;
}

export function ServiceNotice({ service, path }: { service: Service; path: readonly string[] }) {
  const backends = useBackends();
  const held = useHeldReading();
  // THE ENGINE HAS SAID, or this would not be drawn (`App.tsx` gates on it);
  // a reading still missing draws nothing rather than a guess.
  if (!backends) return null;
  // THE SERVICE'S OWN CARD under Company › Integrations, where it has one: the
  // code host has none, since it is set in the company document rather than
  // connected, and the sentence above names the setting.
  const card = SERVICE_CARDS[service];
  const canOpen = held !== null && closedBy(INTEGRATIONS_PATH, held.grants) === null;
  return (
    <EmptyState
      icon={<NoticeGlyph service={service} />}
      title={noticeTitle(service, backends)}
      description={noticeText(service, path)}
      action={
        canOpen && card ? (
          <ButtonLink variant="secondary" href={href([...INTEGRATIONS_PATH, card])}>
            See what is built into Crewlet
          </ButtonLink>
        ) : undefined
      }
    />
  );
}

/** Each service's card under Company › Integrations, or null for one with none. */
const SERVICE_CARDS: Readonly<Record<Service, string | null>> = {
  tracker: "works",
  knowledge: "knowledge",
  code: null,
};

/** The mark the service's own workspace row wears. */
function NoticeGlyph({ service }: { service: Service }) {
  if (service === "tracker") return <SquareKanbanGlyph size={32} />;
  if (service === "code") return <CodeGlyph size={32} />;
  return <BookOpenGlyph size={32} />;
}
