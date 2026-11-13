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
  Skeleton,
  StackedBar,
  StackedRing,
  Text,
} from "@crewlethq/ui";
import type {
  AtlasActivityAnswer,
  AtlasFacetsAnswer,
  AtlasSummary,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";
import { fmtCountShort, fmtDate } from "~/lib/format.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import { EntryDisc } from "./parts.tsx";
import { HEALTH, HEALTH_ORDER, VENDORS, WORDS, WORD_COLORS, entries } from "./words.ts";

/** How many entries the attention tile names before it only counts the rest. */
export const ATTENTION_SHOWN = 2;

const DAY_MS = 86_400_000;

/** The ring reads from the calm part to the urgent one, as the design draws it. */
const RING_ORDER = [...HEALTH_ORDER].reverse();
const WORD_ORDER = Object.keys(WORDS) as AtlasWord[];

export function Band({
  facets,
  attention,
  activity,
  onShow,
  beside = false,
}: {
  /** The whole atlas counted, whatever the search and the filters narrow the page to. */
  facets: AtlasFacetsAnswer;
  /** The most urgent entries, the most urgent first. */
  attention: readonly AtlasSummary[];
  activity: QueryResult<AtlasActivityAnswer>;
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
            <Text variant="label">The company runs on</Text>
            <Text variant="heading">{entries(facets.total)}</Text>
            <Legend items={health.map((one) => ({ ...one, value: one.value.toLocaleString() }))} />
          </div>
        </div>
      </Card>
      <Card padding="md" className="atlas-band__tile atlas-band__tile--attention">
        <div className="atlas-band__words">
          <div className="atlas-band__head">
            <Text variant="label">Needs attention now</Text>
            {wrong > 0 && (
              <Text variant="caption" tone="tertiary">
                {wrong.toLocaleString()} in all
              </Text>
            )}
          </div>
          {attention.length === 0 ? (
            <Text variant="body" tone="secondary">
              Nothing needs attention.
            </Text>
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
                      <Text variant="caption" tone="secondary" truncate>
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
          <Text variant="label">Where it comes from</Text>
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
        </div>
      </Card>
    </div>
  );
}

/** The atlas log's last seven days as one figure, over its lines a day across the whole window. */
function Week({ activity }: { activity: QueryResult<AtlasActivityAnswer> }) {
  const answer = activity.data;
  if (!answer) {
    return (
      <div className="atlas-band__words">
        <Text variant="label">Recorded this week</Text>
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
      <Text variant="label">Recorded this week</Text>
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
    </div>
  );
}
