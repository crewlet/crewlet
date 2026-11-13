/**
 * The pieces every atlas screen draws an entry with: its disc, its vendor,
 * its owner, its health and confidence words, a fact's value and where a
 * claim came from. An entry looks like itself on the map, in the catalogue
 * and in its sheet because all three draw it from here.
 */

import { VendorMark } from "@crewlethq/icons";
import { ArrowUpRightGlyph, CornerDownLeftGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { EmptyValue, EntityChip, IconTile, StatusDot, Tag, VisuallyHidden } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import type {
  AtlasCoverage,
  AtlasHealth,
  AtlasKind,
  AtlasRead,
  AtlasSource,
  AtlasSummary,
  AtlasValue,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";
import type { SetBy } from "~/app/frame/ObjectHeader.tsx";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { seatPath, unitByKey, useOrgIndex } from "~/lib/seats.ts";
import { andList } from "~/lib/words.ts";
import { COVERAGE, HEALTH, KINDS, RECORDS, VENDORS, WORDS, entries, valueWord } from "./words.ts";
// THE ATLAS'S OWN SHEET, with the chunk: every atlas screen draws these parts.
import "~/styles/atlas.css";

/** The kind's glyph, decorative: the kind word is always said beside it. */
export function KindGlyph({ kind, size = "sm" }: { kind: AtlasKind; size?: "sm" | "md" }) {
  return <NamedGlyph name={KINDS[kind].glyph} size={size} />;
}

/** A vendor's mark, where the kit draws one: decorative, its name is beside it. */
export function VendorLead({ vendor }: { vendor: AtlasVendor | undefined }) {
  const mark = vendor ? VENDORS[vendor].mark : undefined;
  return mark ? <VendorMark vendor={mark} size="sm" /> : null;
}

/** A vendor's mark and its name. */
export function VendorName({ vendor }: { vendor: AtlasVendor }) {
  return (
    <span className="atlas-vendor">
      <VendorLead vendor={vendor} />
      <span>{VENDORS[vendor].name}</span>
    </span>
  );
}

/** What every surface needs of an entry to draw it as itself. */
export type EntryFace = Pick<AtlasSummary, "kind" | "vendor" | "health" | "word">;

/**
 * An entry's disc: its kind's glyph, its vendor's mark on the corner, a ring
 * and a badge when it needs a person, and a dashed edge while nobody has
 * confirmed it. Decorative: the words are always said beside it.
 */
export function EntryDisc({ entry, size = "md" }: { entry: EntryFace; size?: "sm" | "md" | "lg" }) {
  const mark = entry.vendor ? VENDORS[entry.vendor].mark : undefined;
  const tone = HEALTH[entry.health].dot;
  return (
    <IconTile
      shape="disc"
      tone="neutral"
      size={size}
      mark={mark ? <VendorMark vendor={mark} /> : undefined}
      status={tone ? { tone } : undefined}
      dashed={entry.word === "unconfirmed"}
    >
      <KindGlyph kind={entry.kind} size={size === "sm" ? "sm" : "md"} />
    </IconTile>
  );
}

/** An entry's health as its mark and its word, a quiet one included. */
export function HealthWord({ health }: { health: AtlasHealth }) {
  const { label, tone } = HEALTH[health];
  return <StatusDot tone={tone}>{label}</StatusDot>;
}

/**
 * Who answers for an entry: the seat or the unit by its name on the chart,
 * or the key as written where the chart no longer holds it. `link` leads to
 * the seat, where the chip does not already stand inside a row's own link.
 */
export function OwnerChip({
  owner,
  link = false,
}: {
  owner: NonNullable<AtlasSummary["owner"]>;
  link?: boolean;
}) {
  const index = useOrgIndex();
  if (owner.kind === "seat") {
    const seat = index.byHandle.get(owner.name);
    return (
      <EntityChip
        name={seat?.name ?? owner.name}
        kind={seat?.kind}
        size="xs"
        href={link && seat ? href(seatPath(seat)) : undefined}
      />
    );
  }
  return <EntityChip name={unitByKey(index, owner.name)?.name ?? owner.name} size="xs" />;
}

/** How an entry relates, as two counts: what relies on it, and what it relies on. */
export function RelationChips({ relations }: { relations: AtlasSummary["relations"] }) {
  if (relations.used_by === 0 && relations.depends_on === 0) {
    return <EmptyValue label="No relations" />;
  }
  return (
    <span className="atlas-chips">
      <Tag size="xs" appearance="outline" leadingIcon={<CornerDownLeftGlyph />}>
        {relations.used_by.toLocaleString()}
        <VisuallyHidden> used by</VisuallyHidden>
      </Tag>
      <Tag size="xs" appearance="outline" leadingIcon={<ArrowUpRightGlyph />}>
        {relations.depends_on.toLocaleString()}
        <VisuallyHidden> depends on</VisuallyHidden>
      </Tag>
    </span>
  );
}

/** A confidence word on the neutral pill: confidence is a word, never a colour. */
export function WordTag({ word }: { word: AtlasWord }) {
  return (
    <Tag variant="neutral" size="xs" appearance={word === "confirmed" ? "quiet" : "outline"}>
      {WORDS[word]}
    </Tag>
  );
}

/** The value types written as a machine reads them, in the code face. */
const MONO = new Set<AtlasValue["type"]>([
  "hostname",
  "ip",
  "address",
  "url",
  "port",
  "version",
  "account_ref",
  "branch",
  "image",
]);

/** A fact's value as the engine canonicalised it, with an internal address marked as one. */
export function FactValue({ value }: { value: AtlasValue }) {
  const text =
    value.type === "instant"
      ? fmtDateTime(value.text)
      : value.type === "word"
        ? valueWord(value.text)
        : value.text;
  return (
    <span className="atlas-value">
      <span className={MONO.has(value.type) ? "mono" : undefined}>{text}</span>
      {value.private && <InternalTag />}
    </span>
  );
}

/** The mark an address no public network routes carries. */
export function InternalTag() {
  return (
    <Tag
      variant="neutral"
      size="xs"
      appearance="outline"
      title="An address on a private network: the company's own map, reachable only from inside it"
    >
      Internal
    </Tag>
  );
}

/** An integration's kind as a reader knows it, where it is not a vendor of its own. */
const INTEGRATION_NAMES: Readonly<Record<string, string>> = {
  jira: "Jira",
  confluence: "Confluence",
  ssh: "SSH",
};

/** Who or what a source is, in a sentence: "Cloudflare", "the access log", "ada". */
export function sourceName(source: AtlasSource, seatName: (handle: string) => string): string {
  const name = source.name ?? "";
  switch (source.kind) {
    case "settings":
      return "the company settings";
    case "records":
      return RECORDS[name] ?? name;
    case "integration":
      return INTEGRATION_NAMES[name] ?? VENDORS[name as AtlasVendor]?.name ?? name;
    case "agent":
    case "person":
      return seatName(name);
  }
}

/**
 * Where a claim came from as a by-line: set by the settings or a person, seen
 * by an integration or the engine's records, observed by an agent in its turn,
 * with the confidence word after the actor and when it was last seen.
 */
export function sourceSetBy(
  claim: { source: AtlasSource; word?: AtlasWord; observed_at: string },
  seatName: (handle: string) => string,
  now: number,
): SetBy {
  const { source } = claim;
  const vendor = source.kind === "integration" ? (source.name as AtlasVendor) : undefined;
  const mark = vendor && VENDORS[vendor]?.mark;
  return {
    actor: sourceName(source, seatName),
    verb:
      source.kind === "agent"
        ? "observed"
        : source.kind === "integration" || source.kind === "records"
          ? "seen"
          : "set",
    qualifier: claim.word ? WORDS[claim.word].toLowerCase() : undefined,
    mark: mark ? <VendorMark vendor={mark} size="sm" /> : undefined,
    ago: relTime(claim.observed_at, now),
    at: claim.observed_at,
    actorKind: source.kind === "agent" ? "agent" : source.kind === "person" ? "human" : "system",
    turnId: source.turn,
  };
}

/** At most this many vendors are named in the coverage line before "and N more". */
const COVERAGE_NAMES = 4;

/**
 * The coverage line: how many entries, and which vendors they come from, most
 * entries first. It never says the atlas is complete, because it never is.
 */
export function coverageLine(total: number, vendors: Partial<Record<AtlasVendor, number>>): string {
  const named = (Object.entries(vendors) as [AtlasVendor, number][])
    .filter(([vendor, count]) => vendor !== "other" && count > 0)
    .sort((a, b) => b[1] - a[1] || VENDORS[a[0]].name.localeCompare(VENDORS[b[0]].name))
    .map(([vendor]) => VENDORS[vendor].name);
  if (named.length === 0) return entries(total);
  const shown = named.length > COVERAGE_NAMES + 1 ? named.slice(0, COVERAGE_NAMES) : named;
  const rest = named.length - shown.length;
  return `${entries(total)}, from ${andList(rest > 0 ? [...shown, `${rest} more`] : shown)}`;
}

/** What a read could not read in full, and what it left out, each one sentence. */
export function coverageNotes(read: Pick<AtlasRead, "coverage" | "withheld">): string[] {
  const notes = read.coverage
    .filter((one: AtlasCoverage) => !one.complete)
    .map(
      (one) => `${COVERAGE[one.source]} was not read in full${one.reason ? `: ${one.reason}` : ""}`,
    );
  if (read.withheld > 0) {
    notes.push(
      `${read.withheld === 1 ? "One value was" : `${read.withheld} values were`} left out for looking like a credential`,
    );
  }
  return notes;
}
