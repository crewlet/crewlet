/**
 * One entry, as its peek and its page read it: its own answer, the entries
 * its relations name, and the pieces both draw (its signals, its facts with
 * where each came from, and what it depends on and what depends on it).
 */

import { useMemo } from "react";
import { AttentionList, Section, Skeleton, type AttentionItem } from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail, type PropertyGroup } from "~/app/frame/PropertiesRail.tsx";
import { RelatedRow } from "~/app/frame/RelatedRow.tsx";
import type {
  AtlasEntry,
  AtlasEntryAnswer,
  AtlasFact,
  AtlasFactView,
  AtlasSummary,
  AtlasTraceAnswer,
} from "~/contract/atlas.ts";
import { relTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { ATLAS_POLL_MS } from "./filters.ts";
import { Neighbourhood } from "./Neighbourhood.tsx";
import {
  EntryDisc,
  FactValue,
  HealthWord,
  KindGlyph,
  OwnerChip,
  VendorName,
  WordTag,
  sourceSetBy,
} from "./parts.tsx";
import {
  factLabel,
  KINDS,
  LANES,
  MEANING,
  RELATIONS,
  SIGNALS,
  STATES,
  WORDS,
  entries,
} from "./words.ts";

/** The address of an entry's page. */
export function entryPath(id: string): string[] {
  return ["company", "atlas", id];
}

/** An entry's answer and the summaries of the entries one step from it, either way. */
export interface EntryReads {
  entry: QueryResult<AtlasEntryAnswer>;
  /** One hop along every relation: the entries this one depends on, by name. */
  next: QueryResult<AtlasTraceAnswer>;
  /** One hop against every relation: the entries that rely on this one. */
  prev: QueryResult<AtlasTraceAnswer>;
}

/** Ask for an entry and for the entries it relates to, while `enabled`. */
export function useEntryReads(id: string, enabled = true): EntryReads {
  const params = useMemo(() => ({ id }), [id]);
  const down = useMemo(() => ({ id, direction: "down", depth: 1, relations: "all" }), [id]);
  const up = useMemo(() => ({ id, direction: "up", depth: 1, relations: "all" }), [id]);
  return {
    entry: useQuery("atlas_entry", params, { enabled, pollMs: ATLAS_POLL_MS }),
    next: useQuery("atlas_trace", down, { enabled, pollMs: ATLAS_POLL_MS }),
    prev: useQuery("atlas_trace", up, { enabled, pollMs: ATLAS_POLL_MS }),
  };
}

/** The entry as a row shows it (its owner, its purpose, its counts), once either trace answers. */
export function selfOf(reads: Pick<EntryReads, "next" | "prev">): AtlasSummary | null {
  return reads.next.data?.from ?? reads.prev.data?.from ?? null;
}

/**
 * An entry's header, on its sheet and on its page: its disc beside the
 * frame's header (its name and key, its health word, who answers for it and
 * how far it is trusted, the one line a person gave it, and where it stands).
 * The disc stands beside the header, never on its plate: a plate is a mark's.
 */
export function EntryHead({
  answer,
  self,
  size,
  now,
}: {
  answer: AtlasEntryAnswer;
  self: AtlasSummary | null;
  size: "page" | "peek";
  now: number;
}) {
  const { entry } = answer;
  const facts: Fact[] = [
    ...(answer.vendor ? [{ label: "Vendor", value: <VendorName vendor={answer.vendor} /> }] : []),
    ...(size === "page" ? [{ label: "Lane", value: LANES[entry.lane].label }] : []),
    { label: "Last seen", value: relTime(entry.last_seen, now) },
    { label: "Sources", value: entry.sources.length.toLocaleString() },
  ];
  return (
    <div className="atlas-head">
      <EntryDisc
        entry={{
          kind: entry.ref.kind,
          vendor: answer.vendor,
          health: answer.health,
          word: entry.word,
        }}
        size="lg"
      />
      <ObjectHeader
        size={size}
        kind={KINDS[entry.ref.kind].word}
        icon={KINDS[entry.ref.kind].glyph}
        identifier={entry.ref.key}
        title={entry.name}
        description={self?.purpose}
        status={
          <>
            <HealthWord health={answer.health} />
            {self?.owner && <OwnerChip owner={self.owner} link />}
            <WordTag word={entry.word} />
            {(entry.state === "stale" || entry.state === "retired") && (
              <span className="muted">{STATES[entry.state]}</span>
            )}
          </>
        }
        facts={facts}
      />
    </div>
  );
}

/**
 * What an entry touches: how many entries rely on it and how many it relies
 * on, in the engine's own counts, over the entry among its neighbours.
 */
export function Touches({
  self,
  reads,
  stacked,
  headingLevel,
}: {
  self: AtlasSummary | null;
  reads: Pick<EntryReads, "next" | "prev">;
  stacked: boolean;
  headingLevel: 2 | 3;
}) {
  const settled = !reads.next.loading && !reads.prev.loading;
  return (
    <Section spacing="tight" headingLevel={headingLevel} title="What it touches">
      {!self ? (
        settled ? (
          <p className="muted">Its relations could not be read.</p>
        ) : (
          <Skeleton variant="box" label="Loading what it touches" />
        )
      ) : (
        <>
          <p>
            {entries(self.relations.used_by)} {self.relations.used_by === 1 ? "relies" : "rely"} on
            it, and it relies on {self.relations.depends_on.toLocaleString()}.
          </p>
          <Neighbourhood
            self={self}
            down={reads.next.data}
            up={reads.prev.data}
            stacked={stacked}
          />
        </>
      )}
    </Section>
  );
}

/** One related entry and how it relates, from either side. */
export interface Related {
  relation: string;
  entry: AtlasSummary;
}

/** What an entry depends on, read off its relations and the summaries one hop along them. */
export function dependsOn(entry: AtlasEntry, next: AtlasTraceAnswer | null): Related[] {
  const named = new Map((next?.hops[0]?.entries ?? []).map((summary) => [summary.id, summary]));
  return entry.relations.flatMap((edge) => {
    const summary = edge.to ? named.get(edge.to) : undefined;
    return summary ? [{ relation: RELATIONS[edge.relation], entry: summary }] : [];
  });
}

/** What depends on an entry, each saying how it relies on this one: "Runs on it". */
export function usedBy(answer: AtlasEntryAnswer): Related[] {
  return answer.used_by.map((link) => ({
    relation: `${RELATIONS[link.relation]} it`,
    entry: link.entry,
  }));
}

/** The facts that place an entry, then what people decided it means, then the rest. */
const IDENTITY: ReadonlySet<AtlasFact> = new Set([
  "vendor",
  "account_ref",
  "site",
  "region",
  "integration",
  "address",
  "url",
  "inbound_url",
  "port",
]);

/** The entry's signals as rows: what is wrong, and since when. */
export function Signals({ entry, now }: { entry: AtlasEntry; now: number }) {
  const items: AttentionItem[] = (entry.signals ?? []).map((signal) => ({
    key: `${signal.kind}:${signal.alarm ?? ""}`,
    tone: signal.severity,
    title: signal.alarm ? `${SIGNALS[signal.kind]}: ${signal.alarm}` : SIGNALS[signal.kind],
    meta: signal.since ? `Since ${relTime(signal.since, now)}` : undefined,
  }));
  if (items.length === 0) {
    return (
      <p className="muted">
        {entry.review
          ? "Nothing is broken. It waits for a person, because its sources disagree: see Sources."
          : "Nothing open on this entry."}
      </p>
    );
  }
  return <AttentionList items={items} labels={{ title: "Signals" }} headingLevel={3} />;
}

/** A fact's row: its value, where it came from, when, and how far it is trusted. */
function factProperty(
  fact: AtlasFactView,
  seatName: (handle: string) => string,
  now: number,
): PropertyGroup["properties"][number] {
  return {
    label: factLabel(fact),
    value: <FactValue value={fact.value} />,
    setBy: sourceSetBy(fact, seatName, now),
    title:
      fact.standing === "settled"
        ? fact.suggested
          ? "Only suggested by an integration or an agent: a person decides what an entry means"
          : undefined
        : "The sources disagree on this fact: see Sources",
  };
}

/** The entry's facts, grouped, each with its source, when it was seen and its confidence word. */
export function Facts({ entry, now }: { entry: AtlasEntry; now: number }) {
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const facts = entry.facts.filter((fact) => fact.fact !== "name");
  const group = (name: string, pick: (fact: AtlasFactView) => boolean): PropertyGroup => ({
    name,
    properties: facts.filter(pick).map((fact) => factProperty(fact, seatName, now)),
  });
  const groups = [
    group("Identity", (fact) => IDENTITY.has(fact.fact)),
    group("Meaning", (fact) => MEANING.has(fact.fact)),
    group("Details", (fact) => !IDENTITY.has(fact.fact) && !MEANING.has(fact.fact)),
  ].filter((one) => one.properties.length > 0);
  if (groups.length === 0)
    return <p className="muted">No source says anything more about it yet.</p>;
  return <PropertiesRail groups={groups} headings="sentence" />;
}

/** At most this many related entries a peek lists of each side before pointing at the page. */
export const PEEK_RELATED = 6;

/** Related entries as rows, each to its own page. */
export function RelatedList({
  related,
  limit,
  none,
}: {
  related: readonly Related[];
  limit?: number;
  /** What an empty side says. */
  none: string;
}) {
  if (related.length === 0) return <p className="muted">{none}</p>;
  const shown = limit === undefined ? related : related.slice(0, limit);
  return (
    <div className="atlas-related">
      {shown.map((one) => (
        <RelatedRow
          key={`${one.relation}:${one.entry.id}`}
          path={entryPath(one.entry.id)}
          icon={<KindGlyph kind={one.entry.kind} />}
          title={one.entry.name}
          description={[
            one.relation,
            KINDS[one.entry.kind].word,
            ...(one.entry.word === "unconfirmed" ? [WORDS.unconfirmed] : []),
          ].join(" · ")}
        />
      ))}
      {shown.length < related.length && (
        <p className="muted">
          And {(related.length - shown.length).toLocaleString()} more on the entry&rsquo;s page.
        </p>
      )}
    </div>
  );
}
