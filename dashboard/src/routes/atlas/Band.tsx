/**
 * The band over the map, four tiles a reader takes in before the picture:
 * how many entries and how they are, what needs attention now and why, how
 * much the atlas recorded this week, and where the record comes from.
 *
 * The band is about the company, so the search and the filters under it do
 * not narrow it. Every figure is the engine's: the counts are `atlas_facets`, the reasons
 * are each entry's own sentence, and the bars are the atlas log's lines a
 * day. Nothing here is a rate, a latency or an estimate.
 */

import { VendorMark } from "@crewlethq/icons";
import {
  ActivityStrip,
  Card,
  Legend,
  Link,
  Skeleton,
  StackedBar,
  StackedRing,
  Text,
  VisuallyHidden,
} from "@crewlethq/ui";
import type {
  AtlasActivityAnswer,
  AtlasFacetsAnswer,
  AtlasReviewKind,
  AtlasSummary,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";
import { href } from "~/app/router.tsx";
import { fmtCountShort, fmtDate } from "~/lib/format.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import { EntryDisc } from "./parts.tsx";
import { HEALTH, HEALTH_ORDER, VENDORS, WORDS, WORD_COLORS, entries } from "./words.ts";

/** The healths that need a person, the most urgent first: what the attention tile counts. */
const WRONG = HEALTH_ORDER.filter((one) => one !== "quiet");

/**
 * How many entries need attention, each health a link to the catalogue and
 * the map narrowed to it (`health=`, the filter's own key). A bare "6 in all"
 * beside two cards left the other four nowhere a reader could go.
 */
function AttentionLinks({
  health,
  unconfirmed,
}: {
  health: AtlasFacetsAnswer["health"];
  unconfirmed: boolean;
}) {
  const counted = WRONG.filter((one) => (health[one] ?? 0) > 0);
  if (counted.length === 0) return null;
  return (
    <span className="atlas-band__counts">
      {counted.map((one) => (
        <Link
          key={one}
          size="inherit"
          href={href(["company", "atlas"], {
            health: one,
            ...(unconfirmed ? { unconfirmed: "true" } : {}),
          })}
        >
          {/* NAMED BY WHAT IT SHOWS FIRST, so a reader speaking to the page
              reaches it by the words on it ("Alerting 4"), and the rest of
              its name says where it goes. */}
          {HEALTH[one].label} {(health[one] ?? 0).toLocaleString()}
          <VisuallyHidden>: show them in the catalogue</VisuallyHidden>
        </Link>
      ))}
    </span>
  );
}

/** How many entries the attention tile names before it only counts the rest. */
export const ATTENTION_SHOWN = 2;

const DAY_MS = 86_400_000;

/** The ring reads from the calm part to the urgent one, as the design draws it. */
const RING_ORDER = [...HEALTH_ORDER].reverse();
const WORD_ORDER = Object.keys(WORDS) as AtlasWord[];

export function Band({
  facets,
  attention,
  unconfirmed = false,
  activity,
  waiting,
  onShow,
  beside = false,
}: {
  /** The whole atlas counted, whatever the search and the filters narrow the page to. */
  facets: AtlasFacetsAnswer;
  /** The most urgent entries, the most urgent first; null until their read answers. */
  attention: readonly AtlasSummary[] | null;
  /** The band counts the unconfirmed entries too, so its links keep them in. */
  unconfirmed?: boolean;
  activity: QueryResult<AtlasActivityAnswer>;
  /** How many items wait for a person, by reason; null until the engine has counted. */
  waiting: Partial<Record<AtlasReviewKind, number>> | null;
  /** Select an entry on the map. */
  onShow: (id: string) => void;
  /** A sheet stands beside the page: the band keeps to its first two tiles, on one row. */
  beside?: boolean;
}) {
  const health = RING_ORDER.map((one) => ({
    id: one,
    label: HEALTH[one].label,
    value: facets.health[one] ?? 0,
    color: HEALTH[one].color,
  }));
  const wrong = (facets.health.alerting ?? 0) + (facets.health.needs_look ?? 0);
  const sources = WORD_ORDER.map((word) => ({
    id: word,
    label: WORDS[word],
    value: facets.words[word] ?? 0,
    color: WORD_COLORS[word],
  }));
  const marks = (Object.entries(facets.vendors) as [AtlasVendor, number][])
    .filter(([vendor, count]) => count > 0 && VENDORS[vendor].mark)
    .sort((a, b) => b[1] - a[1] || VENDORS[a[0]].name.localeCompare(VENDORS[b[0]].name));
  return (
    <div className="atlas-band" data-beside={beside ? "sheet" : undefined}>
      <Card padding="md" className="atlas-band__tile atlas-band__tile--entries">
        <div className="atlas-band__ring">
          <StackedRing segments={health}>{fmtCountShort(facets.total)}</StackedRing>
          <div className="atlas-band__words">
            <Text variant="label" className="atlas-band__title">
              The company runs on
            </Text>
            <Text variant="heading">{entries(facets.total)}</Text>
            <Legend items={health.map((one) => ({ ...one, value: one.value.toLocaleString() }))} />
          </div>
        </div>
      </Card>
      <Card padding="md" className="atlas-band__tile atlas-band__tile--attention">
        <div className="atlas-band__words">
          <div className="atlas-band__head">
            <Text variant="label" className="atlas-band__title">
              Needs attention now
            </Text>
            <AttentionLinks health={facets.health} unconfirmed={unconfirmed} />
          </div>
          {attention === null ? (
            // THE LIST HAS NOT ANSWERED: the count beside the title is
            // another read's, and neither speaks for the other.
            <Skeleton variant="text" rows={2} label="Reading what needs attention" />
          ) : attention.length === 0 ? (
            // ONLY WHERE THE COUNT AGREES. Two reads a moment apart can
            // disagree; the links above then stand alone rather than under a
            // sentence that contradicts them.
            wrong === 0 && (
              <Text variant="body" tone="secondary">
                Nothing needs attention.
              </Text>
            )
          ) : (
            <div className="atlas-band__attention">
              {attention.slice(0, ATTENTION_SHOWN).map((row) => (
                // THE CARD IS THE ACTION: pressing it selects the entry on the map.
                <Card key={row.id} padding="sm" interactive asChild>
                  <button
                    type="button"
                    className="atlas-band__entry"
                    title={`Show ${row.name} on the map`}
                    onClick={() => onShow(row.id)}
                  >
                    <EntryDisc entry={row} size="md" />
                    <span className="atlas-band__entry-words">
                      <Text variant="body" truncate className="atlas-band__name">
                        {row.name}
                      </Text>
                      {/* THE REASON IS WHAT THE CARD IS FOR: two lines of it (the
                          product's one clamp, `.clamp` in base.css), and the
                          whole sentence on hover where two do not hold it. */}
                      <Text
                        variant="caption"
                        tone="secondary"
                        className="clamp"
                        title={row.reason ?? undefined}
                      >
                        {row.reason ?? HEALTH[row.health].label}
                      </Text>
                    </span>
                  </button>
                </Card>
              ))}
            </div>
          )}
        </div>
      </Card>
      <Card padding="md" className="atlas-band__tile atlas-band__tile--week">
        <Week activity={activity} />
      </Card>
      <Card padding="md" className="atlas-band__tile atlas-band__tile--sources">
        <div className="atlas-band__words">
          <Text variant="label" className="atlas-band__title">
            Where it comes from
          </Text>
          {marks.length > 0 && (
            <span className="atlas-band__marks">
              {marks.map(([vendor]) => {
                const mark = VENDORS[vendor].mark;
                return mark ? (
                  <VendorMark key={vendor} vendor={mark} size="sm" title={VENDORS[vendor].name} />
                ) : null;
              })}
            </span>
          )}
          <StackedBar segments={sources} />
          <Legend items={sources.map((one) => ({ ...one, value: one.value.toLocaleString() }))} />
          {waiting && <ReviewLink waiting={waiting} />}
          <Link
            href={href(["company", "atlas", "sources"])}
            className="atlas-band__link"
            title="Each integration the atlas reads, how its last read went, and Refresh now"
          >
            Sources
          </Link>
        </div>
      </Card>
    </div>
  );
}

/** The way to Review, saying how much waits for a person there. */
function ReviewLink({ waiting }: { waiting: Partial<Record<AtlasReviewKind, number>> }) {
  const count = Object.values(waiting).reduce((sum, one) => sum + (one ?? 0), 0);
  return (
    <Link
      href={href(["company", "atlas", "review"])}
      className="atlas-band__link"
      title="What waits for a person: disagreements, and entries nobody has confirmed"
    >
      {count === 0 ? "Review: none waiting" : `Review: ${count.toLocaleString()} waiting`}
    </Link>
  );
}

/** The atlas log's last seven days as one figure, over its lines a day across the whole window. */
function Week({ activity }: { activity: QueryResult<AtlasActivityAnswer> }) {
  const answer = activity.data;
  if (!answer) {
    return (
      <div className="atlas-band__words">
        <Text variant="label" className="atlas-band__title">
          Recorded this week
        </Text>
        {activity.loading ? (
          <Skeleton variant="text" rows={2} label="Loading the atlas log" />
        ) : (
          <Text variant="body" tone="secondary">
            {activity.detail ?? "The atlas log could not be read."}
          </Text>
        )}
      </div>
    );
  }
  const from = Date.parse(answer.from);
  const week = answer.days.slice(-7).reduce((sum, day) => sum + day, 0);
  return (
    <div className="atlas-band__words">
      <Text variant="label" className="atlas-band__title">
        Recorded this week
      </Text>
      <span className="atlas-band__figure">
        <Text variant="display" numeric>
          {week.toLocaleString()}
        </Text>
        <Text variant="caption" tone="secondary">
          {week === 1 ? "update" : "updates"}
        </Text>
      </span>
      <ActivityStrip
        label={`Lines recorded in the atlas each day, the last ${answer.days.length} days`}
        buckets={answer.days.map((v, day) => ({ t: from + day * DAY_MS, v }))}
        reading={{ formatTime: (at) => fmtDate(new Date(at).toISOString()) }}
        emptyLabel="Nothing recorded in the window."
      />
      <Link href={href(["company", "atlas", "changes"])} className="atlas-band__link">
        Every change
      </Link>
    </div>
  );
}
