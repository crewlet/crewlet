/**
 * Company › Integrations, its first row: the services BUILT INTO Crewlet
 * (Crewlet Works, Crewlet Knowledge, and Crewlet Chat once it ships), above
 * the third-party catalogue, so a company sees what it already has before
 * what it could connect.
 *
 * # Drawn as the catalogue draws a tool, and saying only what the engine serves
 *
 * Each card is the catalogue's tile (the kit's `CatalogTile`: the mark, the
 * name, a state when it carries one, one sentence, one way in), with
 * Crewlet's own mark (`VendorMark`'s `crewlet`). What it says comes from two
 * answers and nothing else:
 *
 *  - WHICH SYSTEM THE COMPANY RUNS is the engine's word (`lib/services.ts`):
 *    its own, another in its place (Jira, Confluence), or none.
 *  - The fact on a card in use is a count the service already answers: the
 *    projects Works holds, the spaces Knowledge holds.
 *
 * A STATE ONLY WHERE IT IS NEWS. A service the company runs says On, and Chat
 * says Coming soon; a service the company runs elsewhere or not at all carries
 * no pill, sits on the quieter ground and says in its sentence what runs
 * instead. Turning a service on or off is not offered: the company document
 * decides it, and a switch here that did nothing would be a lie.
 *
 * # One grid with the catalogue below it
 *
 * The section has its own heading (the kit's `Section`, as the catalogue's
 * is) and the same three-track `TileGrid` as the third-party apps, so every
 * tile edge on the page lines up and three tiles fill their row. A tile with
 * no way in (Chat, or a service the company runs elsewhere) draws no footer:
 * a rule with nothing under it reads as a missing control.
 *
 * Where the company does not run a service, its places are hidden, and an
 * address under one says so in place (`app/ServiceNotice.tsx`), offering this
 * page only to a reader who may open it.
 */

import { ButtonLink, CatalogTile, Section, Tag, TileGrid, type Tone } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useBackends, type OrgServices, useServicesAsk } from "~/lib/services.ts";

/** What a card says about its service: the state worth reading, and the sentence under it. */
interface Status {
  /** The service is in use: the pill under its name. Absent otherwise. */
  tag?: { label: string; tone: Tone };
  text: string;
  /** In use: where it opens. */
  open?: { label: string; path: string[] };
  /** In use: what it holds, once counted. */
  facts?: string;
}

const ON = { label: "On", tone: "success" } as const;

/** A node running no company has nothing to say about either service. */
const NO_COMPANY: Status = { text: "This node runs no company yet." };

function worksStatus(tracker: OrgServices["tracker"], projects: number | null): Status {
  switch (tracker) {
    case "native":
      return {
        tag: ON,
        text: "Projects, tasks, your inbox and My work.",
        open: { label: "Open Work", path: ["work"] },
        ...(projects !== null ? { facts: plural(projects, "project") } : {}),
      };
    case "jira":
      return { text: "Jira handles work items." };
    case "none":
      return { text: "The company runs no work tracker." };
  }
}

function knowledgeStatus(knowledge: OrgServices["knowledge"], spaces: number | null): Status {
  switch (knowledge) {
    case "native":
      return {
        tag: ON,
        text: "Spaces and pages, agent skills and diaries.",
        open: { label: "Open Knowledge", path: ["knowledge"] },
        ...(spaces !== null ? { facts: plural(spaces, "space") } : {}),
      };
    case "confluence":
      return { text: "Confluence holds the knowledge base." };
    case "none":
      return { text: "The company runs no knowledge base." };
  }
}

/** The services built into Crewlet, with what the company runs of each. */
export function BuiltIn() {
  const backends = useBackends();
  const ask = useServicesAsk();
  // THE COUNTS, asked only of a service the company runs: elsewhere the
  // engine refuses both, and the card says what runs instead.
  const projects = useQuery("work_projects", { limit: 1 }, { enabled: ask.tracker });
  const spaces = useQuery("containers", undefined, { enabled: ask.knowledge });
  return (
    <Section
      title="Built into Crewlet"
      description="Crewlet’s own services, with nothing to connect."
    >
      <TileGrid columns={3}>
        <BuiltInTile
          name="Crewlet Works"
          status={statusOf(backends, (b) => worksStatus(b.tracker, projects.data?.total ?? null))}
        />
        <BuiltInTile
          name="Crewlet Knowledge"
          status={statusOf(backends, (b) =>
            knowledgeStatus(b.knowledge, spaces.data?.containers.length ?? null),
          )}
        />
        <BuiltInTile name="Crewlet Chat" status={null} />
      </TileGrid>
    </Section>
  );
}

/** A card's status from the engine's answer, or what a node with no company says. */
function statusOf(backends: OrgServices | null, of: (b: OrgServices) => Status): Status {
  return backends ? of(backends) : NO_COMPANY;
}

/**
 * One built-in service, as a catalogue tile. `status` null is a service that
 * has not shipped yet: Coming soon, its mark drained, and nothing to open.
 */
function BuiltInTile({ name, status }: { name: string; status: Status | null }) {
  const open = status?.open;
  return (
    <CatalogTile
      quiet={open === undefined}
      mark={<VendorMark vendor="crewlet" size="lg" muted={status === null} />}
      title={name}
      status={
        status === null ? (
          <Tag size="sm" variant="neutral" appearance="outline">
            Coming soon
          </Tag>
        ) : status.tag ? (
          <Tag size="sm" variant={status.tag.tone}>
            {status.tag.label}
          </Tag>
        ) : undefined
      }
      description={status ? status.text : "Talk with your agents in Crewlet itself."}
      meta={open ? (status?.facts ?? "") : undefined}
      action={
        open ? (
          <ButtonLink variant="secondary" size="small" href={href(open.path)}>
            {open.label}
          </ButtonLink>
        ) : undefined
      }
    />
  );
}
