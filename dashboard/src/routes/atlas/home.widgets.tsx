/**
 * Atlas's widget: what in the atlas is alerting or needs a look, without
 * opening Company › Atlas. The two counts, the most urgent entries with the
 * engine's own reason for each, and a link to every one.
 *
 * READ ONLY, AND ONLY WHAT THE PAGE READS: the questions the page's band
 * asks, in the page's own words for an entry's health. The board offers it
 * only to a reader who holds Atlas, and the engine refuses both questions to
 * anybody else, so no entry's name reaches a reader without the permission.
 */

import { useMemo } from "react";
import { List, ListItem, StatCard, StatGroup, StatusDot } from "@crewlethq/ui";
import { MapGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type { AtlasCoverage } from "~/contract/atlas.ts";
import { plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { WidgetCard, WidgetMore, WidgetQuery, defineWidgets } from "~/routes/home/board/kit.tsx";
import { COVERAGE, HEALTH } from "./words.ts";

/** The entries the card lists before it says how many more the page holds. */
const ROWS = 4;

/** A Home is glanced at, not watched: half the page's own cadence. */
const POLL_MS = 60_000;

/** The atlas whole, as the page's band counts it. */
const WHOLE = {};

/** The most urgent entries first; the quiet ones a short atlas answers are left out below. */
const URGENT = { sort: "health", limit: ROWS };

/** The page, narrowed to what the card counts. */
const ATLAS = ["company", "atlas"];

/**
 * What the reads could not read in full, one sentence a source, each said
 * once: a source that was not read may hold an entry that is alerting.
 */
function gapsOf(reads: readonly ({ coverage: AtlasCoverage[] } | null)[]): string[] {
  const missed = new Set(
    reads.flatMap((read) => (read?.coverage ?? []).filter((c) => !c.complete).map((c) => c.source)),
  );
  return [...missed].map((source) => `${COVERAGE[source]} was not read, so this may be short.`);
}

function Attention() {
  const counts = useQuery("atlas_facets", WHOLE, { pollMs: POLL_MS });
  const urgent = useQuery("atlas_entries", URGENT, { pollMs: POLL_MS });
  const rows = useMemo(
    () => (urgent.data?.rows ?? []).filter((row) => row.health !== "quiet"),
    [urgent.data],
  );
  const alerting = counts.data?.health.alerting ?? 0;
  const look = counts.data?.health.needs_look ?? 0;
  const wrong = alerting + look;
  const total = counts.data?.total ?? 0;
  const answered = counts.data !== null && urgent.data !== null;
  return (
    <WidgetCard title="Atlas attention" from="atlas">
      <WidgetQuery
        error={counts.error ?? urgent.error}
        refusal={counts.refusal ?? urgent.refusal}
        loading={(counts.loading && !counts.data) || (urgent.loading && !urgent.data)}
        gaps={gapsOf([counts.data, urgent.data])}
        empty={
          !answered || wrong > 0 || rows.length > 0
            ? undefined
            : total === 0
              ? {
                  title: "Nothing in the atlas yet",
                  hint: "Entries appear as the company connects its tools.",
                }
              : {
                  title: "Nothing is alerting",
                  hint: `${plural(total, "entry", "entries")} in the atlas, all quiet.`,
                }
        }
      >
        <StatGroup flush columns={2} className="home-admin-stats">
          <StatCard
            label={HEALTH.alerting.label}
            value={alerting}
            sub={alerting > 0 ? "broken now" : "nothing broken"}
          />
          <StatCard
            label={HEALTH.needs_look.label}
            value={look}
            sub={look > 0 ? "waiting on a person" : "nothing waiting"}
          />
        </StatGroup>
        <List className="home-admin-list">
          {rows.map((row) => (
            <ListItem
              key={row.id}
              href={href([...ATLAS, row.id])}
              leading={
                <StatusDot tone={HEALTH[row.health].tone}>{HEALTH[row.health].label}</StatusDot>
              }
            >
              <span className="home-widget-line">
                <span className="truncate" title={row.name}>
                  {row.name}
                </span>
                {row.reason && <span className="t-caption">{row.reason}</span>}
              </span>
            </ListItem>
          ))}
        </List>
        <WidgetMore more={wrong - rows.length} to={ATLAS} />
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "atlas.attention",
    from: "atlas",
    label: "Atlas attention",
    description: "Entries that are alerting or need a look, with counts",
    icon: MapGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: Attention,
  },
]);
