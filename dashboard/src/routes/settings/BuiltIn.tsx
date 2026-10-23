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
 * # A hidden place lands here
 *
 * Where the company does not run a service, its places are hidden, and an
 * address under one opens this page with that service's card brought into
 * view (`?builtin=works|knowledge`, `app/App.tsx`), since the card says what
 * runs in its place.
 */

import { useEffect, useRef } from "react";
import { ButtonLink, Eyebrow, Tag, type Tone } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href, useRoute } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { SERVICE_CARDS, servicesRun, useBackends, type OrgServices } from "~/lib/services.ts";

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
  const route = useRoute();
  const pointed = route.query.get("builtin") ?? "";
  const backends = useBackends();
  const runs = servicesRun(backends);
  // THE COUNTS, asked only of a service the company runs: elsewhere the
  // engine refuses both, and the card says what runs instead.
  const projects = useQuery("work_projects", { limit: 1 }, { enabled: runs.tracker });
  const spaces = useQuery("containers", undefined, { enabled: runs.knowledge });
  return (
    <section className="col gap-3" aria-labelledby="builtin-heading">
      <div className="col gap-1">
        <Eyebrow as="p" id="builtin-heading">
          Built into Crewlet
        </Eyebrow>
        <PageNote>Crewlet&rsquo;s own services. Nothing to connect.</PageNote>
      </div>
      <div className="int-grid">
        <BuiltInTile
          id={SERVICE_CARDS.tracker}
          name="Crewlet Works"
          focus={pointed === SERVICE_CARDS.tracker}
          status={statusOf(backends, (b) => worksStatus(b.tracker, projects.data?.total ?? null))}
        />
        <BuiltInTile
          id={SERVICE_CARDS.knowledge}
          name="Crewlet Knowledge"
          focus={pointed === SERVICE_CARDS.knowledge}
          status={statusOf(backends, (b) =>
            knowledgeStatus(b.knowledge, spaces.data?.containers.length ?? null),
          )}
        />
        <BuiltInTile id="chat" name="Crewlet Chat" status={null} focus={false} />
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
function BuiltInTile({
  id,
  name,
  status,
  focus,
}: {
  id: string;
  name: string;
  status: Status | null;
  focus: boolean;
}) {
  const heading = useRef<HTMLHeadingElement>(null);
  // THE CARD A HIDDEN PLACE'S ADDRESS POINTS AT is brought into view and
  // focused: a reader who followed a link to Work, on a company that runs
  // Jira, lands on the sentence that says so.
  useEffect(() => {
    if (!focus) return;
    heading.current?.scrollIntoView({ block: "center" });
    heading.current?.focus({ preventScroll: true });
  }, [focus]);
  const inUse = status?.open !== undefined;
  const nameID = `builtin-${id}`;
  return (
    <article className={inUse ? "int-tile" : "int-tile is-absent"} aria-labelledby={nameID}>
      <div className="int-tile-head">
        <span className="int-brand" aria-hidden>
          <VendorMark vendor="crewlet" size="lg" muted={!inUse} />
        </span>
        <span className="int-tile-title">
          <h2 className="int-name" id={nameID} ref={heading} tabIndex={-1}>
            {name}
          </h2>
          {status ? (
            <Tag size="sm" variant={status.tone} appearance={status.outline ? "outline" : "soft"}>
              {status.tag}
            </Tag>
          ) : (
            <Tag size="sm" variant="neutral" appearance="soft">
              Coming soon
            </Tag>
          )}
        </span>
      </div>
      <p className="int-tile-text">
        {status ? status.text : "Talk with your agents in Crewlet itself."}
      </p>
      <div className="int-tile-foot">
        <span className="int-tile-meta">{status?.facts ?? ""}</span>
        {status?.open && (
          <ButtonLink variant="secondary" size="small" href={href(status.open.path)}>
            {status.open.label}
          </ButtonLink>
        )}
      </div>
    </article>
  );
}
