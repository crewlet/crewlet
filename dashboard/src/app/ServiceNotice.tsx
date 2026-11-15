/**
 * What an address under a service the company does not run draws, in place.
 *
 * # Said where the reader is, to every reader
 *
 * The Worklet workspace and My work read Worklet, the Doclet workspace reads
 * Doclet, Chatlet reads Chatlet and the Gitlet workspace reads Gitlet, so
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
 * now: the services' pages are the overview everybody holds. Gitlet's is not
 * the one offered: nothing runs in Gitlet's place for its page to name, so
 * the notice offers the step that turns it on, the company document on
 * Configuration, instead.
 *
 * # A service this node is waiting to start
 *
 * A node starts Crewlet's own services as it boots, so a company that turns
 * one on while the node is running has it only after the node restarts, and
 * until then every read of it there is refused (`lib/services.ts`'s
 * [awaitsRestart]). That is not "not in use here": the notice says the
 * company uses it, that this node starts it when it restarts, and who does
 * that, with the nodes' page for a reader who may open it. It said only "Not
 * available on this node" on every screen of the workspace, under a sidebar
 * row that looked like any other.
 */

import { ButtonLink, EmptyState } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  CodeGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
} from "@crewlethq/icons/glyphs";
import {
  awaitsRestart,
  chatVendorName,
  SERVICE_NAMES,
  useBackends,
  type OrgServices,
} from "~/lib/services.ts";
import { useHeldReading, useViewer } from "~/lib/viewer.ts";
import { closedBy, sectionOf, workspaceOf, workspaceRow, type Service } from "./nav.ts";
import { href } from "./router.tsx";

/** Where the services built into Crewlet are described, and what each runs. */
const INTEGRATIONS_PATH = ["company", "integrations"] as const;

/**
 * Each service's line in the directory, which "See what is built into
 * Crewlet" opens: one per service, so a service with no line is a type error
 * rather than a link to another service's page. Gitlet's is not offered
 * (null): its page names nothing in its place, so the notice offers the step
 * that turns it on instead ([CodeHostingStep]).
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
      // NO VENDOR STANDS IN GITLET'S PLACE: a company's repositories on
      // GitHub or GitLab are integrations its seats call, and the engine draws
      // none of them here, so the heading says only what is true of every
      // such company.
      return "Crewlet is not hosting git repositories for this company";
  }
}

/** The sentence under it: why the page the address names is not here. */
export function noticeText(service: Service, path: readonly string[]): string {
  if (service === "knowledge") {
    return "Doclet is not in use here. Each agent's diary is still on its Memory tab, under Structure.";
  }
  if (service === "chat") {
    return "Chatlet is not in use here.";
  }
  if (service === "code") {
    return "Gitlet shows the changes, repositories and pushes of the git repositories Crewlet hosts for a company, and it is turned off here.";
  }
  // A NEW DESIGN IS MADE BY AN AGENT WORKING A TASK, so a reader who asked
  // for one is told that, not only that Jira handles work.
  if (path[0] === "artifacts") {
    return "A new design is made by an agent working a task, and Worklet, where tasks live, is not in use here.";
  }
  // WORKLET'S OWN PAGES ARE THE SERVICE, so they name it once. Elsewhere the
  // place is named as the reader knows it: a workspace whose row reads
  // Worklet by its name, else the section the address shows, since the
  // workspace as a whole does not read it (My work's Queue, beside a Reviews
  // tab that is Gitlet's; Settings › Expensive tasks).
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
  if (awaitsRestart(backends, service)) {
    return (
      <EmptyState
        icon={<Glyph size={32} />}
        title={restartTitle(service)}
        description={restartText(service)}
        action={<RestartStep grants={held?.grants ?? null} />}
      />
    );
  }
  return (
    <EmptyState
      icon={<Glyph size={32} />}
      title={noticeTitle(service, backends, vendors)}
      description={noticeText(service, path)}
      action={
        service === "code" ? (
          <CodeHostingStep grants={held?.grants ?? null} />
        ) : canOpen && line ? (
          <ButtonLink variant="secondary" href={href([...INTEGRATIONS_PATH, line])}>
            See what is built into Crewlet
          </ButtonLink>
        ) : undefined
      }
    />
  );
}

/** The heading where the company runs the service and this node has not started it. */
export function restartTitle(service: Service): string {
  return `${SERVICE_NAMES[service]} starts when this node restarts`;
}

/** The sentence under it: why it is not here yet, and what brings it. */
export function restartText(service: Service): string {
  const name = SERVICE_NAMES[service];
  return `This company turned ${name} on while this node was running, and a node starts ${name} as it boots. Restart the engine on each node, one at a time, and ${name} opens here.`;
}

/** Where the engine's nodes are listed, for whoever restarts one. */
const NODES_PATH = ["settings", "nodes"] as const;

/**
 * The step a reader takes where the node is waiting to start a service: a
 * reader who may open the nodes' page is sent there to see which node this
 * is and what each is running; anybody else is told who can restart it.
 */
function RestartStep({ grants }: { grants: readonly string[] | null }) {
  if (grants === null) return null;
  if (closedBy(NODES_PATH, grants) === null) {
    return (
      <ButtonLink variant="secondary" href={href([...NODES_PATH])}>
        See the engine’s nodes
      </ButtonLink>
    );
  }
  return <p className="t-caption">Ask whoever runs the engine to restart it.</p>;
}

/** Where Gitlet is turned on: the company document, on Configuration. */
const CONFIGURATION_PATH = ["settings", "config"] as const;

/**
 * The step a reader takes where the company hosts no code: a reader who may
 * open Configuration, where the company document turns it on, is sent there;
 * anybody else is told who can.
 */
function CodeHostingStep({ grants }: { grants: readonly string[] | null }) {
  if (grants === null) return null;
  if (closedBy(CONFIGURATION_PATH, grants) === null) {
    return (
      <ButtonLink variant="secondary" href={href([...CONFIGURATION_PATH])}>
        Open Configuration
      </ButtonLink>
    );
  }
  return <p className="t-caption">Ask an admin to turn on Gitlet.</p>;
}
