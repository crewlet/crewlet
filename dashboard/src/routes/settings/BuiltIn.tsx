/**
 * Company › Integrations, its first row: the services BUILT INTO Crewlet
 * (Crewlet Works, Crewlet Knowledge, and Crewlet Chat once it ships), above
 * the third-party catalogue, so a company sees what it already has before
 * what it could connect.
 *
 * # Drawn as the catalogue draws a tool, and saying only what the engine serves
 *
 * Each card is the catalogue's tile ([IntegrationTile]'s markup: the mark,
 * the name, the engine's word for its state, one sentence, one way in), with
 * Crewlet's own mark (`VendorMark`'s `crewlet`). What it says comes from two
 * answers and nothing else:
 *
 *  - WHICH SYSTEM THE COMPANY RUNS is the engine's word (`lib/services.ts`):
 *    its own, another in its place (Jira, Confluence), or none.
 *  - The fact on a card in use is a count the service already answers: the
 *    projects Works holds, the spaces Knowledge holds.
 *
 * Turning a service on or off is not offered: the company document decides
 * it, and a switch here that did nothing would be a lie.
 *
 * # Its own section, sized for its own tiles
 *
 * The section has its own heading and its own grid, three tracks for its
 * three tiles, so it never shares the catalogue's four-up grid and leaves a
 * quarter of a row empty. A tile with no way in (Chat, or a service the
 * company runs elsewhere) draws no footer: a rule with nothing under it reads
 * as a missing control. The catalogue's filter and the base-address note are
 * the catalogue's, drawn under its own heading below this section.
 *
 * Where the company does not run a service, its places are hidden, and an
 * address under one says so in place (`app/ServiceNotice.tsx`), offering this
 * page only to a reader who may open it.
 */

import { ButtonLink, Eyebrow, Tag, type Tone } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useBackends, type OrgServices, useServicesAsk } from "~/lib/services.ts";

/** What a card says about its service: the engine's word, and the sentence under it. */
interface Status {
  tag: string;
  tone: Tone;
  outline: boolean;
  text: string;
  /** In use: where it opens. */
  open?: { label: string; path: string[] };
  /** In use: what it holds, once counted. */
  facts?: string;
}

const NOT_IN_USE = { tag: "Not in use", tone: "neutral", outline: true } as const;
const OFF = { tag: "Off", tone: "neutral", outline: true } as const;
const ON = { tag: "On", tone: "success", outline: false } as const;

/** A node running no company has nothing to say about either service. */
const NO_COMPANY: Status = { ...OFF, text: "This node runs no company yet." };

function worksStatus(tracker: OrgServices["tracker"], projects: number | null): Status {
  switch (tracker) {
    case "native":
      return {
        ...ON,
        text: "Projects, tasks, your inbox and My work.",
        open: { label: "Open Work", path: ["work"] },
        ...(projects !== null ? { facts: plural(projects, "project") } : {}),
      };
    case "jira":
      return { ...NOT_IN_USE, text: "Jira handles work items." };
    case "none":
      return { ...OFF, text: "The company runs no work tracker." };
  }
}

function knowledgeStatus(knowledge: OrgServices["knowledge"], spaces: number | null): Status {
  switch (knowledge) {
    case "native":
      return {
        ...ON,
        text: "Spaces and pages, agent skills and diaries.",
        open: { label: "Open Knowledge", path: ["knowledge"] },
        ...(spaces !== null ? { facts: plural(spaces, "space") } : {}),
      };
    case "confluence":
      return { ...NOT_IN_USE, text: "Confluence holds the knowledge base." };
    case "none":
      return { ...OFF, text: "The company runs no knowledge base." };
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
    <section className="col gap-3" aria-labelledby="builtin-heading">
      <div className="col gap-1">
        <Eyebrow as="p" id="builtin-heading">
          Built into Crewlet
        </Eyebrow>
        <PageNote>Crewlet&rsquo;s own services. Nothing to connect.</PageNote>
      </div>
      <div className="int-grid int-grid-builtin">
        <BuiltInTile
          id="works"
          name="Crewlet Works"
          status={statusOf(backends, (b) => worksStatus(b.tracker, projects.data?.total ?? null))}
        />
        <BuiltInTile
          id="knowledge"
          name="Crewlet Knowledge"
          status={statusOf(backends, (b) =>
            knowledgeStatus(b.knowledge, spaces.data?.containers.length ?? null),
          )}
        />
        <BuiltInTile id="chat" name="Crewlet Chat" status={null} />
      </div>
    </section>
  );
}

/** A card's status from the engine's answer, or what a node with no company says. */
function statusOf(backends: OrgServices | null, of: (b: OrgServices) => Status): Status {
  return backends ? of(backends) : NO_COMPANY;
}

/**
 * One built-in service, as a catalogue tile. `status` null is a service that
 * has not shipped yet: Coming soon, and nothing to open.
 */
function BuiltInTile({ id, name, status }: { id: string; name: string; status: Status | null }) {
  const inUse = status?.open !== undefined;
  const nameID = `builtin-${id}`;
  return (
    <article className={inUse ? "int-tile" : "int-tile is-absent"} aria-labelledby={nameID}>
      <div className="int-tile-head">
        <span className="int-brand" aria-hidden>
          <VendorMark vendor="crewlet" size="lg" muted={!inUse} />
        </span>
        <span className="int-tile-title">
          <h2 className="int-name" id={nameID}>
            {name}
          </h2>
          {status ? (
            <Tag size="sm" variant={status.tone} appearance={status.outline ? "outline" : "soft"}>
              {status.tag}
            </Tag>
          ) : (
            // THE SAME NEUTRAL TREATMENT AS NOT IN USE: both say "nothing
            // here to open", and two looks for one state read as two states.
            <Tag size="sm" variant="neutral" appearance="outline">
              Coming soon
            </Tag>
          )}
        </span>
      </div>
      <p className="int-tile-text">
        {status ? status.text : "Talk with your agents in Crewlet itself."}
      </p>
      {status?.open && (
        <div className="int-tile-foot">
          <span className="int-tile-meta">{status.facts ?? ""}</span>
          <ButtonLink variant="secondary" size="small" href={href(status.open.path)}>
            {status.open.label}
          </ButtonLink>
        </div>
      )}
    </article>
  );
}
