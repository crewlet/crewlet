/**
 * An entry's page: what it is, what is wrong with it, the entry among its
 * neighbours, its facts and its recent changes (Overview), what it depends on
 * and what depends on it (Relations), and what each source says about it
 * (Sources), where a disagreement between sources is shown with both values
 * and never settled on the reader's behalf.
 */

import { useId, useMemo } from "react";
import { Card, Link, Section, Skeleton, Stack, Tabs, Tag } from "@crewlethq/ui";
import { SetByLine } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { RelatedRow } from "~/app/frame/RelatedRow.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { href } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import type {
  AtlasClaimed,
  AtlasEdge,
  AtlasEntry,
  AtlasEntryAnswer,
  AtlasExternal,
  AtlasFactView,
  AtlasSource,
  AtlasStanding,
} from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { Changes } from "./Changes.tsx";
import { ATLAS_POLL_MS, SELECT_KEY, TRACE_KEY } from "./filters.ts";
import {
  EntryHead,
  Facts,
  RelatedList,
  Signals,
  Touches,
  dependsOn,
  entryPath,
  selfOf,
  useEntryReads,
  usedBy,
  type EntryReads,
} from "./entry.tsx";
import { FactValue, sourceName, sourceSetBy } from "./parts.tsx";
import { FACTS, KINDS, RELATIONS, SOURCE_KINDS, USES, WORDS, entries } from "./words.ts";

const TABS = ["overview", "relations", "sources"] as const;
type Tab = (typeof TABS)[number];
const TAB_LABELS: Readonly<Record<Tab, string>> = {
  overview: "Overview",
  relations: "Relations",
  sources: "Sources",
};

export function EntryPage({ id }: { id: string }) {
  const reads = useEntryReads(id);
  const { entry } = reads;
  const answer = entry.data;
  usePageLabels(answer ? { [id]: answer.entry.name } : {});
  if (entry.loading && !answer)
    return <Skeleton variant="text" rows={8} label="Loading the entry" />;
  if (!answer) {
    return (
      <QueryState error={entry.error} refusal={entry.refusal} detail={entry.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return <EntryReading answer={answer} reads={reads} />;
}

/** The facts and relations the sources disagree on. */
function disagreements(entry: AtlasEntry): { facts: AtlasFactView[]; relations: AtlasEdge[] } {
  return {
    facts: entry.facts.filter((fact) => fact.standing !== "settled"),
    relations: entry.relations.filter((edge) => edge.standing !== "settled"),
  };
}

function EntryReading({ answer, reads }: { answer: AtlasEntryAnswer; reads: EntryReads }) {
  const now = useNow();
  const panelId = useId();
  const [tab, setTab] = useTab<Tab>("tab", TABS);
  const { entry } = answer;
  const below = dependsOn(entry, reads.next.data);
  const above = usedBy(answer);
  const disputed = disagreements(entry);
  const conflicts = disputed.facts.length + disputed.relations.length;
  return (
    <div className="atlas-entry">
      <EntryHead answer={answer} self={selfOf(reads)} size="page" now={now} />
      <div className="tab-detail">
        <Tabs
          variant="underline"
          overflow="menu"
          ariaLabel={`${entry.name} sections`}
          value={tab}
          onValueChange={(value) => setTab(value as Tab)}
          panelId={panelId}
          items={TABS.map((value) => ({
            value,
            label: TAB_LABELS[value],
            ...(value === "relations" ? { count: below.length + above.length || null } : {}),
            ...(value === "sources"
              ? {
                  count: entry.sources.length,
                  ...(conflicts > 0
                    ? {
                        marker: {
                          tone: "warning" as const,
                          label: `${plural(conflicts, "disagreement")} between sources`,
                        },
                      }
                    : {}),
                }
              : {}),
          }))}
        />
        <div className="tabpanel" role="tabpanel" id={panelId} tabIndex={0}>
          {tab === "overview" && <Overview entry={entry} reads={reads} now={now} />}
          {tab === "relations" && <Relations answer={answer} below={below} above={above} />}
          {tab === "sources" && <Sources entry={entry} now={now} />}
        </div>
      </div>
    </div>
  );
}

function Overview({ entry, reads, now }: { entry: AtlasEntry; reads: EntryReads; now: number }) {
  return (
    <Stack gap={5}>
      <Signals entry={entry} now={now} />
      <Touches self={selfOf(reads)} reads={reads} stacked={false} headingLevel={2} />
      <Section spacing="tight" headingLevel={2} title="Facts">
        <Facts entry={entry} now={now} />
      </Section>
      {(entry.use?.length ?? 0) > 0 && <Use entry={entry} now={now} />}
      {(entry.holders?.length ?? 0) > 0 && <Holders entry={entry} now={now} />}
      {entry.members.length > 1 && (
        <Section spacing="tight" headingLevel={2} title="Also known as">
          <p className="muted">
            Sources name this one thing by {entry.members.length.toLocaleString()} exact keys, so
            the atlas holds it as one entry.
          </p>
          <ul className="atlas-list">
            {entry.members.map((member) => (
              <li key={`${member.kind}:${member.key}`}>
                {KINDS[member.kind].word} <span className="mono">{member.key}</span>
              </li>
            ))}
          </ul>
        </Section>
      )}
      <Section spacing="tight" headingLevel={2} title="Recent changes">
        <Changes id={entry.id} />
      </Section>
    </Stack>
  );
}

/** How much the company relies on the entry, channel by channel. */
function Use({ entry, now }: { entry: AtlasEntry; now: number }) {
  const seatBadge = useSeatBadgeOf();
  return (
    <Section spacing="tight" headingLevel={2} title="Use">
      {entry.not_used_recently && (
        <p className="muted">Nothing recorded a use of it in the last 30 days.</p>
      )}
      <ul className="atlas-list">
        {(entry.use ?? []).map((use) => {
          const [one, many] = USES[use.channel].unit;
          const counted = use.counted
            ? `${plural(use.week, one, many)} this week, ${plural(use.month, one, many)} this month`
            : null;
          const last = use.last ? `last ${relTime(use.last, now)}` : "never recorded";
          const by = use.seats?.length
            ? `by ${use.seats.map((handle) => seatBadge(handle).name).join(", ")}`
            : null;
          return (
            <li key={use.channel}>
              <strong>{USES[use.channel].label}</strong>:{" "}
              {[counted, last, by].filter(Boolean).join(", ")}
            </li>
          );
        })}
      </ul>
    </Section>
  );
}

/** The agents holding an account at the vendor the entry is. */
function Holders({ entry, now }: { entry: AtlasEntry; now: number }) {
  const seatBadge = useSeatBadgeOf();
  return (
    <Section spacing="tight" headingLevel={2} title="Agents with an account here">
      <ul className="atlas-list">
        {(entry.holders ?? []).map((holder) => (
          <li key={`${holder.seat}:${holder.lane}`}>
            <Link href={href(["company", "org", "seats", holder.seat])}>
              {seatBadge(holder.seat).name}
            </Link>{" "}
            <span className="muted">
              on {holder.lane}
              {holder.level ? `, ${holder.level}` : ""}
              {holder.enabled ? "" : ", turned off"}
              {holder.observed_at ? `, seen ${relTime(holder.observed_at, now)}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </Section>
  );
}

/** The glyph of each outside identity a relation names. */
const EXTERNAL_GLYPHS: Readonly<Record<AtlasExternal, GlyphName>> = {
  seat: "bot",
  unit: "network",
  page: "file-text",
};

/** Where an outside identity a relation names is read. */
function externalPath(edge: AtlasEdge): string[] | null {
  if (!edge.name) return null;
  switch (edge.external) {
    case "seat":
      return ["company", "org", "seats", edge.name];
    case "unit":
      return ["company", "org", "table"];
    case "page":
      return ["doclet", "pages", edge.name];
    default:
      return null;
  }
}

function Relations({
  answer,
  below,
  above,
}: {
  answer: AtlasEntryAnswer;
  below: ReturnType<typeof dependsOn>;
  above: ReturnType<typeof usedBy>;
}) {
  const { entry } = answer;
  const outside = entry.relations.filter((edge) => edge.external && edge.name);
  return (
    <Stack gap={5}>
      <Section
        spacing="tight"
        headingLevel={2}
        title={`Depends on (${below.length.toLocaleString()})`}
      >
        <RelatedList related={below} none="It depends on no other entry." />
      </Section>
      <Section
        spacing="tight"
        headingLevel={2}
        title={`Used by (${above.length.toLocaleString()})`}
      >
        <RelatedList related={above} none="No other entry depends on it." />
      </Section>
      {outside.length > 0 && (
        <Section spacing="tight" headingLevel={2} title="Owners and pages">
          <div className="atlas-related">
            {outside.map((edge) => {
              const path = externalPath(edge);
              return path ? (
                <RelatedRow
                  key={`${edge.relation}:${edge.external}:${edge.name}`}
                  path={path}
                  icon={<NamedGlyph name={EXTERNAL_GLYPHS[edge.external ?? "page"]} size="sm" />}
                  title={edge.name ?? ""}
                  description={RELATIONS[edge.relation]}
                />
              ) : null;
            })}
          </div>
        </Section>
      )}
      <Upstream id={entry.id} name={entry.name} />
    </Stack>
  );
}

/** Everything that depends on the entry, hop by hop, through the relations the map draws. */
function Upstream({ id, name }: { id: string; name: string }) {
  const trace = useQuery(
    "atlas_trace",
    useMemo(() => ({ id, direction: "up" }), [id]),
    { pollMs: ATLAS_POLL_MS },
  );
  const answer = trace.data;
  const reached = answer?.hops.reduce((sum, hop) => sum + hop.entries.length, 0) ?? 0;
  return (
    <Section spacing="tight" headingLevel={2} title="What depends on it, hop by hop">
      {!answer ? (
        trace.loading ? (
          <Skeleton variant="text" rows={2} label="Tracing" />
        ) : (
          <QueryState
            error={trace.error}
            refusal={trace.refusal}
            detail={trace.detail}
            loading={false}
          >
            {null}
          </QueryState>
        )
      ) : reached === 0 ? (
        <p className="muted">Nothing on the map depends on {name}.</p>
      ) : (
        <>
          <p className="muted">
            {entries(reached)} {reached === 1 ? "depends" : "depend"} on {name}
            {answer.truncated
              ? ", and the trace stopped at its bound before walking every hop"
              : ""}
            .{" "}
            <Link href={href(["company", "atlas"], { [SELECT_KEY]: id, [TRACE_KEY]: "up" })}>
              Show it on the map
            </Link>
          </p>
          <ol className="atlas-hops">
            {answer.hops.map((hop) => (
              <li key={hop.distance}>
                <span className="t-label">{plural(hop.distance, "hop")}</span>
                <span>
                  {hop.entries.map((summary, i) => (
                    <span key={summary.id}>
                      {i > 0 && ", "}
                      <Link href={href(entryPath(summary.id))}>{summary.name}</Link>
                      {summary.word === "unconfirmed" && (
                        <span className="muted"> ({WORDS.unconfirmed.toLowerCase()})</span>
                      )}
                    </span>
                  ))}
                </span>
              </li>
            ))}
          </ol>
        </>
      )}
    </Section>
  );
}

/** Why the atlas shows the value it does where the sources disagree, in words. */
const STANDING_WORDS: Readonly<Record<Exclude<AtlasStanding, "settled">, string>> = {
  contested:
    "A newer report contradicts what a person set. The person's value is shown, the other beside it, and the entry waits for a person to decide.",
  disputed:
    "Two sources the engine trusts alike disagree, with no person to settle it. The newest is shown, the other beside it, and the entry waits for a person to decide.",
};

/** Two sources are one where every part of their identity agrees. */
function sameSource(a: AtlasSource, b: AtlasSource): boolean {
  return a.kind === b.kind && a.name === b.name && a.turn === b.turn && a.revision === b.revision;
}

function Sources({ entry, now }: { entry: AtlasEntry; now: number }) {
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const disputed = disagreements(entry);
  const says = (source: AtlasSource) =>
    entry.facts.flatMap((fact) => {
      if (sameSource(fact.source, source)) return [{ fact: fact.fact, value: fact.value }];
      const other = fact.others?.find((claim) => sameSource(claim.source, source));
      return other?.value ? [{ fact: fact.fact, value: other.value }] : [];
    });
  return (
    <Stack gap={5}>
      {disputed.facts.length + disputed.relations.length > 0 && (
        <Section spacing="tight" headingLevel={2} title="Where the sources disagree">
          <Stack gap={3}>
            {disputed.facts.map((fact) => (
              <Disagreement
                key={fact.fact}
                label={FACTS[fact.fact]}
                standing={fact.standing}
                shown={{
                  value: fact.value,
                  source: fact.source,
                  word: fact.word,
                  observed_at: fact.observed_at,
                }}
                others={fact.others ?? []}
                seatName={seatName}
                now={now}
              />
            ))}
            {disputed.relations.map((edge) => (
              <Disagreement
                key={`${edge.relation}:${edge.to ?? edge.name}`}
                label={RELATIONS[edge.relation]}
                standing={edge.standing}
                shown={{ source: edge.source, word: edge.word, observed_at: edge.observed_at }}
                others={edge.others ?? []}
                seatName={seatName}
                now={now}
              />
            ))}
          </Stack>
        </Section>
      )}
      <Section
        spacing="tight"
        headingLevel={2}
        title={`Every source (${entry.sources.length.toLocaleString()})`}
      >
        <Stack gap={3}>
          {entry.sources.map((source) => {
            const values = says(source);
            const named = source.kind === "settings" || source.kind === "records";
            const name = sourceName(source, seatName);
            return (
              <Card
                key={`${source.kind}:${source.name ?? ""}:${source.turn ?? ""}:${source.revision ?? ""}`}
                variant="outlined"
                padding="md"
              >
                <Card.Header
                  subtitle={
                    source.kind === "settings"
                      ? source.revision && `Revision ${source.revision}`
                      : SOURCE_KINDS[source.kind]
                  }
                  subtitlePlacement="beside"
                >
                  <Card.Title as="h3">
                    {named ? name.charAt(0).toUpperCase() + name.slice(1) : name}
                  </Card.Title>
                </Card.Header>
                <Card.Body>
                  {values.length > 0 ? (
                    <PropertiesRail
                      groups={[
                        {
                          properties: values.map((one) => ({
                            label: FACTS[one.fact],
                            value: <FactValue value={one.value} />,
                          })),
                        },
                      ]}
                    />
                  ) : (
                    <p className="muted">
                      It names the entry and its relations, and no fact of its own.
                    </p>
                  )}
                </Card.Body>
              </Card>
            );
          })}
        </Stack>
      </Section>
    </Stack>
  );
}

/** One fact or relation the sources disagree on: the value shown, every other beside it. */
function Disagreement({
  label,
  standing,
  shown,
  others,
  seatName,
  now,
}: {
  label: string;
  standing: AtlasStanding;
  shown: Pick<AtlasFactView, "source" | "word" | "observed_at"> & {
    value?: AtlasFactView["value"];
  };
  others: readonly AtlasClaimed[];
  seatName: (handle: string) => string;
  now: number;
}) {
  return (
    <Card variant="outlined" padding="md" rail="warning">
      <Card.Header
        subtitle={standing === "settled" ? undefined : STANDING_WORDS[standing]}
        subtitlePlacement="below"
      >
        <Card.Title as="h3">{label}</Card.Title>
      </Card.Header>
      <Card.Body>
        <ul className="atlas-claims">
          <li>
            <span className="atlas-claims__value">
              {shown.value ? <FactValue value={shown.value} /> : "Holds"}
              <Tag variant="neutral" size="sm">
                Shown
              </Tag>
            </span>
            <SetByLine setBy={sourceSetBy(shown, seatName, now)} className="t-caption muted" />
          </li>
          {others.map((claim) => (
            <li key={`${claim.source.kind}:${claim.source.name ?? ""}:${claim.observed_at}`}>
              <span className="atlas-claims__value">
                {claim.denied ? (
                  "Says it does not hold"
                ) : claim.value ? (
                  <FactValue value={claim.value} />
                ) : (
                  "Holds"
                )}
              </span>
              <SetByLine setBy={sourceSetBy(claim, seatName, now)} className="t-caption muted" />
            </li>
          ))}
        </ul>
      </Card.Body>
    </Card>
  );
}
