/**
 * Company › Integrations with nothing chosen: what the company has in use.
 *
 * # Only what is active
 *
 * The list beside the page is the catalogue, every integration by category;
 * this home is the company's own: the connected apps (Jira and Confluence each
 * as its own product of the one Atlassian connection) and the Crewlet services
 * it runs, each a compact card in one header line with its state, its figure
 * and its one action. Services not in use and apps nobody connected are the list's,
 * and appear here only as the way to them: a home with nothing active says so
 * and points at the list. The heading's sentence and the count beside In use
 * are `home.ts`'s, which says why the one is fixed and what the other counts.
 *
 * EVERY FIGURE IS ONE THE ENGINE BACKS, or it is not drawn. An app is read by
 * its agents' accounts (x of y ready) where it gives each agent one, and
 * otherwise by its newest delivery, drawn only where this node could read its
 * event log. The activity is the webhook deliveries the event log holds and
 * each app's last reconcile pass.
 *
 * WHAT A READER IS SHOWN FOLLOWS WHAT THEY MAY READ. An Admin gets every app's
 * state, what needs a look, the activity and the address apps deliver to. A
 * reader without Admin gets the Crewlet services the company runs, which
 * every reader holds, and the note saying who manages the apps: nothing here
 * asks a question the engine keeps for Admins.
 */

import { useMemo, type ReactNode } from "react";
import {
  ButtonLink,
  CatalogTile,
  Count,
  EmptyState,
  Section,
  StatusDot,
  TileGrid,
} from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { PlugGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { IntegrationRow } from "~/contract/integrations.ts";
import { CATALOG, deliveryEvent, type Entry } from "./catalogue.tsx";
import { DIRECTORY, entryOf, type Line } from "./categories.ts";
import { BUCKET_WORDS, type BuiltInService, type Intent, type Model, type Tool } from "./model.ts";
import {
  actionName,
  EngineAddress,
  FeedRow,
  IssueRow,
  ServiceMark,
  serviceStatus,
} from "./parts.tsx";
import { DESCRIPTION, homeFacts, type InUse } from "./home.ts";

/** How many lines the activity shows: a glance, with each app one press away. */
const ACTIVITY_LINES = 5;

/** The size a mark is drawn at in a compact card's well. */
const MARK = 20;

/** Where one item's page is, opening its settings form for `open` when one is named. */
function pageOf(key: string, open?: Intent): string {
  return href(["company", "integrations", key], open ? { open } : undefined);
}

/** The app a delivery's source surface belongs to. */
function entryOfSource(source: string): Entry | undefined {
  return CATALOG.find((e) => e.surfaces.some((s) => s.key === source));
}

/** The newest instant one app's rows carry for its deliveries. */
function lastDelivery(tool: Tool, rows: Map<string, IntegrationRow>): string | null {
  let best: string | null = null;
  for (const s of tool.entry.surfaces) {
    const at = rows.get(s.key)?.last_at;
    if (typeof at === "string" && at !== "" && (best === null || at > best)) best = at;
  }
  return best;
}

/** Whether a line is in use: a service the company runs, or a connected app or product. */
function active(line: Line, model: Model | null, services: BuiltInService[]): boolean {
  if (line.kind === "service") return services.some((s) => s.key === line.key && s.known && s.on);
  const tool = model?.byKey.get(entryOf(line)!.key);
  if (!tool) return false;
  return line.kind === "product"
    ? tool.sections.some((s) => s.tool.key === line.key && s.tool.configured)
    : tool.configured;
}

export function Landing({
  model,
  services,
  note,
}: {
  model: Model | null;
  services: BuiltInService[];
  /** What a reader without Admin is told about the apps. */
  note: ReactNode;
}) {
  const now = useNow();
  // IN THE LIST'S OWN ORDER, so a card stands where its line does.
  const lines = DIRECTORY.flatMap((g) => g.lines).filter((l) => active(l, model, services));
  const facts = homeFacts(
    lines.map((l): InUse =>
      l.kind === "service" ? "service" : l.kind === "product" ? "product" : "app",
    ),
    model !== null,
  );
  return (
    <div className="int-landing">
      <ObjectHeader kind="Company" icon="plug" title="Integrations" description={DESCRIPTION} />
      {model ? <Attention model={model} /> : note}
      {!facts.empty ? (
        <Section
          spacing="tight"
          title={
            <>
              In use <Count value={facts.inUse} />
            </>
          }
        >
          <TileGrid as="ul" columns={2}>
            {lines.map((line) => (
              <Card key={line.key} line={line} model={model} services={services} now={now} />
            ))}
          </TileGrid>
          {facts.hint && <p className="t-caption">{facts.hint}</p>}
        </Section>
      ) : (
        <EmptyState
          size="compact"
          icon={<PlugGlyph size={32} />}
          title="Nothing is in use yet"
          description="Choose an app or a Crewlet service from the list to see what it does for your agents."
        />
      )}
      {model && model.tools.some((t) => t.configured) && <Activity model={model} now={now} />}
      {model?.base && (
        <footer className="int-landing-foot">
          <EngineAddress value={model.base} />
        </footer>
      )}
    </div>
  );
}

/**
 * What needs a look, each with its one fix, or one line saying nothing does.
 * WHAT A PERSON OWES and nothing else: an issue the engine or the app is
 * already working through is said on the app's page.
 */
function Attention({ model }: { model: Model }) {
  const owed = model.tools.flatMap((t) =>
    t.issues.filter((i) => i.owner === "you").map((issue) => ({ tool: t, issue })),
  );
  if (owed.length === 0) {
    return model.tools.some((t) => t.configured) ? (
      <StatusDot tone="success">
        All good. Every connected app is working, and nothing is waiting on you.
      </StatusDot>
    ) : null;
  }
  return (
    <Section spacing="tight" title="Needs a look">
      <div className="int-issues">
        {owed.map(({ tool, issue }) => (
          <IssueRow
            key={`${tool.entry.key}:${issue.key}`}
            tool={tool}
            issue={issue}
            go={pageOf(tool.entry.key, issue.fix?.intent)}
            showTool
          />
        ))}
      </div>
    </Section>
  );
}

/** One active item, compact: its mark, name, state and figure on one line, and its action. */
function Card({
  line,
  model,
  services,
  now,
}: {
  line: Line;
  model: Model | null;
  services: BuiltInService[];
  now: number;
}) {
  if (line.kind === "service") {
    const s = services.find((x) => x.key === line.key)!;
    return (
      <CatalogTile
        as="li"
        density="compact"
        mark={<ServiceMark service={s} size={MARK} />}
        title={
          <a className="int-tile-link" href={pageOf(s.key)}>
            {s.name}
          </a>
        }
        status={serviceStatus(s)}
        description={s.sentence}
        action={
          <ButtonLink
            size="small"
            variant="secondary"
            href={pageOf(s.key)}
            aria-label={actionName("Open", s.name)}
          >
            Open
          </ButtonLink>
        }
      />
    );
  }
  const entry = entryOf(line)!;
  const tool = model!.byKey.get(entry.key)!;
  const words = BUCKET_WORDS[tool.bucket];
  const figure = figureOf(tool, model!, now);
  const fix = tool.issues.find((i) => i.owner === "you" && i.fix)?.fix;
  const name = line.kind === "product" ? line.name : entry.name;
  return (
    <CatalogTile
      as="li"
      density="compact"
      mark={<VendorMark vendor={entry.vendor} size={MARK} />}
      title={
        <a className="int-tile-link" href={pageOf(line.key)}>
          {name}
        </a>
      }
      status={words}
      description={entry.description}
      {...(figure ? { meta: figure } : {})}
      action={
        <ButtonLink
          size="small"
          variant="secondary"
          href={pageOf(line.key, fix?.intent)}
          aria-label={actionName(fix ? fix.label : "Open", name)}
        >
          {fix ? fix.label : "Open"}
        </ButtonLink>
      }
    />
  );
}

/** The figure an app is read by: its agents, or its newest delivery. */
function figureOf(tool: Tool, model: Model, now: number): string {
  if (tool.seats.length > 0) {
    const ready = tool.seats.filter((s) => s.status === "ready").length;
    return `${ready} of ${plural(tool.seats.length, "agent")} ready`;
  }
  if (!model.trafficKnown) return "";
  const last = lastDelivery(tool, model.rows);
  return last ? `Last delivery ${relTime(last, now)}` : "Nothing delivered yet";
}

/** One line of the activity: a delivery, or an app's latest pass. */
interface Happening {
  key: string;
  at: string;
  entry: Entry;
  what: string;
}

/**
 * What happened across the connected apps, newest first: the deliveries the
 * event log holds and each app's latest reconcile pass, each opening its app.
 */
function Activity({ model, now }: { model: Model; now: number }) {
  const deliveries = useQuery(
    "events",
    { category: "webhook", limit: ACTIVITY_LINES },
    { pollMs: 60_000, refetchOnFocus: true },
  );
  const lines = useMemo<Happening[]>(() => {
    const out: Happening[] = [];
    for (const e of deliveries.data?.events ?? []) {
      const entry = entryOfSource(e.source);
      if (!entry) continue;
      out.push({
        key: `delivery:${e.id}`,
        at: e.timestamp,
        entry,
        what: `sent ${deliveryEvent(e.type)}${e.summary ? `: ${e.summary}` : ""}`,
      });
    }
    for (const t of model.tools.filter((x) => x.configured)) {
      for (const s of t.entry.surfaces) {
        const r = model.rows.get(s.key)?.reconcile;
        if (!r?.last_attempt_at) continue;
        const label = r.phase_label || r.phase.replace(/_/g, " ");
        out.push({
          key: `pass:${s.key}`,
          at: r.last_attempt_at,
          entry: t.entry,
          what: `checked by Crewlet${t.entry.surfaces.length > 1 ? ` (${s.name})` : ""}: ${label.toLowerCase()}`,
        });
      }
    }
    return out.sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : 0)).slice(0, ACTIVITY_LINES);
  }, [deliveries.data, model.tools, model.rows]);

  return (
    <Section spacing="tight" title="Recent activity">
      {lines.length > 0 ? (
        <ul className="int-feed">
          {lines.map((l) => (
            <FeedRow
              key={l.key}
              mark={<VendorMark vendor={l.entry.vendor} size="sm" />}
              href={pageOf(l.entry.key)}
              who={l.entry.name}
              what={l.what}
              at={l.at}
              now={now}
            />
          ))}
        </ul>
      ) : (
        <p className="t-caption">
          {deliveries.loading
            ? "Reading the activity"
            : "Nothing has arrived from a connected app yet."}
        </p>
      )}
    </Section>
  );
}
