/**
 * The catalogue under the map: the same entries, one rich row each, grouped
 * by health with the most urgent first. The engine orders the whole atlas
 * that way (`sort=health`), so a page never splits a group, and each group's
 * heading counts all of it, loaded or not.
 *
 * A row is selected with the map: pressing one opens its sheet and lights its
 * path, and the row under the pointer is lit on the map. A row whose entry
 * open tasks name says how many under its health, as a link to them.
 */

import { useMemo } from "react";
import {
  EmptyValue,
  Link,
  Skeleton,
  Sparkline,
  StatusDot,
  TableDateCell,
  TableTextCell,
  TableTwoLineCell,
  Text,
  VisuallyHidden,
  peekRowActivation,
  type DataGridBand,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { QueryState } from "~/components/common.tsx";
import type {
  AtlasActivityAnswer,
  AtlasEntriesAnswer,
  AtlasFacetsAnswer,
  AtlasSummary,
} from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useQuery, withFloor, type QueryResult } from "~/lib/useQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { ATLAS_POLL_MS } from "./filters.ts";
import { EntryDisc, HealthWord, OwnerChip, RelationChips, VendorLead, WordTag } from "./parts.tsx";
import { HEALTH, HEALTH_ORDER, KINDS, VENDORS, entries, openTasks } from "./words.ts";
import { workHref } from "./Work.tsx";

const rowsOf = (answer: AtlasEntriesAnswer) => answer.rows;
const idOf = (row: AtlasSummary) => row.id;
const nextOf = (answer: AtlasEntriesAnswer) => answer.next_cursor ?? "";

type Activity = AtlasActivityAnswer["entries"][number];

/** The rows under a heading per health, the most urgent first, each counting all of its group. */
export function bandsOf(
  rows: readonly AtlasSummary[],
  counts: AtlasFacetsAnswer["health"] | undefined,
): DataGridBand<AtlasSummary>[] {
  return HEALTH_ORDER.map((health) => ({
    key: health,
    label: HEALTH[health].label,
    mark: <StatusDot tone={HEALTH[health].tone} />,
    total: counts?.[health],
    rows: rows.filter((row) => row.health === health),
  })).filter((band) => band.rows.length > 0);
}

export function Catalogue({
  params,
  facets,
  activity,
  work,
  narrowed,
  emptyHint,
  selectedId,
  pointedId,
  onPoint,
}: {
  /** The search and the filters as the engine takes them. */
  params: Record<string, string | boolean>;
  /** The counts each group's heading says, once they arrive. */
  facets: AtlasFacetsAnswer | null;
  /** The atlas log counted by day and entry, for each row's line and last change. */
  activity: QueryResult<AtlasActivityAnswer>;
  /** How many open tasks name each entry, by its id, or null where no work is read for the reader. */
  work: ReadonlyMap<string, number> | null;
  narrowed: boolean;
  /** What fills the atlas, said under an empty one. */
  emptyHint: string;
  selectedId: string | null;
  /** The entry the map points at, lit as the pointer lights a row. */
  pointedId: string | null;
  onPoint: (id: string | null) => void;
}) {
  const now = useNow();
  const { socket } = useClient();
  const { open } = usePeekControls();
  const asked = useMemo(() => ({ ...params, sort: "health" }), [params]);
  const first = useQuery("atlas_entries", asked, { pollMs: ATLAS_POLL_MS });
  const paged = usePaged(
    first,
    asked,
    // AT THIS TAB'S READ FLOOR, as the first page is asked.
    (cursor) =>
      socket.query(
        "atlas_entries",
        withFloor("atlas_entries", JSON.stringify({ ...asked, cursor })),
      ),
    rowsOf,
    idOf,
    nextOf,
    { restartWhenRefused: true },
  );
  const rows = paged.items;
  const total = paged.answers[0]?.total ?? 0;
  const logged = useMemo(
    () => new Map((activity.data?.entries ?? []).map((one): [string, Activity] => [one.id, one])),
    [activity.data],
  );
  const span = activity.data?.days.length ?? 0;
  const bands = useMemo(() => bandsOf(rows, facets?.health), [rows, facets]);

  return (
    <section className="atlas-catalogue" aria-labelledby="atlas-catalogue-title">
      <div className="atlas-catalogue__head">
        <Text as="h2" variant="heading" id="atlas-catalogue-title">
          Catalogue
        </Text>
        {paged.answers.length > 0 && (
          <Text variant="caption" tone="secondary">
            {entries(total)}
          </Text>
        )}
      </div>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={6} label="Loading entries" />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : narrowed
              ? {
                  title: "No entry matches",
                  hint: "Clear the search or a filter to see more of the atlas.",
                }
              : { title: "Nothing in the atlas yet", hint: emptyHint }
        }
      >
        <DataGrid<AtlasSummary>
          bands={bands}
          rowKey={idOf}
          serverSorted
          rowHref={(row) => peekHref({ kind: "atlas", id: row.id })}
          onRowActivate={peekRowActivation<AtlasSummary>((row) =>
            open({ kind: "atlas", id: row.id }),
          )}
          isSelected={(row) => row.id === selectedId}
          isPointed={(row) => row.id === pointedId}
          onRowPoint={(row) => onPoint(row?.id ?? null)}
          rowTone={(row) => HEALTH[row.health].dot}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={
            paged.more
              ? `${entries(rows.length)} of ${total.toLocaleString()} loaded`
              : `That is all of it: ${entries(total)}`
          }
          aria-label="Catalogue of everything the company runs on"
          columns={[
            {
              key: "name",
              header: "Entry",
              width: "minmax(13rem, 2fr)",
              phoneLead: true,
              cell: (row) => (
                <TableTwoLineCell
                  icon={<EntryDisc entry={row} size="md" />}
                  value={row.name}
                  detail={KINDS[row.kind].word}
                  titleWhenCut
                />
              ),
            },
            {
              key: "vendor",
              header: "Vendor",
              width: "minmax(8rem, 1fr)",
              drop: 4,
              cell: (row) =>
                row.vendor ? (
                  <TableTextCell mark={<VendorLead vendor={row.vendor} />}>
                    {VENDORS[row.vendor].name}
                  </TableTextCell>
                ) : (
                  <EmptyValue />
                ),
            },
            {
              key: "owner",
              header: "Owner",
              width: "minmax(9rem, 1fr)",
              drop: 6,
              cell: (row) =>
                row.owner ? (
                  <OwnerChip owner={row.owner} />
                ) : (
                  <Text variant="cell" tone="tertiary">
                    No owner yet
                  </Text>
                ),
            },
            {
              key: "health",
              header: "Health",
              width: "minmax(8rem, 1fr)",
              title: (row) => row.reason,
              cell: (row) => <HealthCell row={row} open={work?.get(row.id) ?? 0} />,
            },
            {
              key: "relations",
              header: "Relations",
              width: "minmax(7rem, 0.8fr)",
              drop: 1,
              cell: (row) => <RelationChips relations={row.relations} />,
            },
            {
              key: "activity",
              header: "Activity",
              width: "minmax(8rem, 1fr)",
              drop: 2,
              cell: (row) => <ActivityCell days={logged.get(row.id)?.days} span={span} />,
            },
            {
              key: "changed",
              header: "Last change",
              width: "minmax(10rem, 1.4fr)",
              drop: 5,
              title: (row) => {
                const last = logged.get(row.id)?.last;
                return last ? `${last.text} · ${fmtDateTime(last.at)}` : undefined;
              },
              cell: (row) => {
                const last = logged.get(row.id)?.last;
                return last ? (
                  <TableTwoLineCell
                    value={<TableDateCell value={last.at} now={now} />}
                    detail={last.text}
                  />
                ) : (
                  <EmptyValue />
                );
              },
            },
            {
              key: "confidence",
              header: "Confidence",
              width: "minmax(7rem, 0.8fr)",
              drop: 3,
              cell: (row) => <WordTag word={row.word} />,
            },
          ]}
        />
      </QueryState>
    </section>
  );
}

/**
 * An entry's health, and under it how many open tasks name it, as the way to
 * them. A second line of a cell every row already has, so the mark costs the
 * grid no column and stays on a phone's card.
 */
function HealthCell({ row, open }: { row: AtlasSummary; open: number }) {
  if (open === 0) return <HealthWord health={row.health} />;
  return (
    <TableTwoLineCell
      value={<HealthWord health={row.health} />}
      detail={
        <Link href={workHref(row.id)} variant="subtle" size="inherit">
          <StatusDot tone="info">{openTasks(open)}</StatusDot>
        </Link>
      }
    />
  );
}

/**
 * An entry's part of the atlas log over the window: its lines a day as a fine
 * line, and their count. A count of recorded lines, never a rate; an entry
 * the log holds no line of in the window draws nothing.
 */
function ActivityCell({ days, span }: { days: readonly number[] | undefined; span: number }) {
  if (!days) return <EmptyValue />;
  const total = days.reduce((sum, day) => sum + day, 0);
  return (
    <span className="atlas-activity">
      <span className="atlas-activity__line">
        <Sparkline
          values={[...days]}
          weight="fine"
          color="var(--color-brand-accent)"
          height="1.375rem"
        />
      </span>
      <Text variant="cell" tone="secondary" numeric>
        {total.toLocaleString()}
        <VisuallyHidden>
          {" "}
          {total === 1 ? "line" : "lines"} recorded in {span} days
        </VisuallyHidden>
      </Text>
    </span>
  );
}
