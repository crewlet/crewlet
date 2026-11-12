/**
 * The atlas as a list: the same entries as the map, one row each, a page at a
 * time in the engine's order. A row opens the entry in the peek beside the
 * list; a modifier press opens its page.
 */

import { useMemo } from "react";
import {
  EmptyValue,
  Select,
  Skeleton,
  TableDateCell,
  TableTextCell,
  TableTwoLineCell,
  peekRowActivation,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import type { AtlasEntriesAnswer, AtlasSummary } from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { ATLAS_POLL_MS } from "./filters.ts";
import { HealthTag, KindGlyph, VendorLead, WordTag } from "./parts.tsx";
import { KINDS, LANES, VENDORS, entries } from "./words.ts";

/** The orders the engine reads a list in, the map's first. */
export const ORDERS = [
  { value: "", label: "Map order" },
  { value: "name", label: "Name" },
  { value: "last_seen", label: "Last seen" },
] as const;

const rowsOf = (answer: AtlasEntriesAnswer) => answer.rows;
const idOf = (row: AtlasSummary) => row.id;
const nextOf = (answer: AtlasEntriesAnswer) => answer.next_cursor ?? "";

export function AtlasList({
  params,
  narrowed,
  emptyHint,
}: {
  /** The filters as the engine takes them. */
  params: Record<string, string | boolean>;
  narrowed: boolean;
  /** What fills the atlas, said under an empty one. */
  emptyHint: string;
}) {
  const now = useNow();
  const { socket } = useClient();
  const { open } = usePeekControls();
  const [order] = useParam("order", "", "filter");
  const asked = useMemo(() => (order ? { ...params, sort: order } : params), [params, order]);
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

  return (
    <>
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
                  title: "No entry matches the filters",
                  hint: "Clear a filter to see more of the atlas.",
                }
              : { title: "Nothing in the atlas yet", hint: emptyHint }
        }
      >
        <DataGrid<AtlasSummary>
          rows={rows}
          rowKey={idOf}
          serverSorted
          rowHref={(row) => peekHref({ kind: "atlas", id: row.id })}
          onRowActivate={peekRowActivation<AtlasSummary>((row) =>
            open({ kind: "atlas", id: row.id }),
          )}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={
            paged.more
              ? `${entries(rows.length)} of ${total.toLocaleString()} loaded`
              : `That is all of it: ${entries(total)}`
          }
          aria-label="Atlas entries"
          columns={[
            {
              key: "name",
              header: "Entry",
              floor: "12rem",
              phoneLead: true,
              cell: (row) => (
                <TableTwoLineCell
                  icon={<KindGlyph kind={row.kind} />}
                  value={row.name}
                  detail={row.key === row.name ? undefined : row.key}
                  mono="detail"
                  titleWhenCut
                />
              ),
            },
            {
              key: "kind",
              header: "Kind",
              shrink: true,
              cell: (row) => <TableTextCell>{KINDS[row.kind].word}</TableTextCell>,
            },
            {
              key: "vendor",
              header: "Vendor",
              shrink: true,
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
              key: "health",
              header: "Health",
              shrink: true,
              cell: (row) =>
                row.health === "quiet" ? <EmptyValue /> : <HealthTag health={row.health} />,
            },
            {
              key: "confidence",
              header: "Confidence",
              shrink: true,
              cell: (row) => <WordTag word={row.word} />,
            },
            {
              key: "lane",
              header: "Lane",
              shrink: true,
              optional: true,
              cell: (row) => <TableTextCell>{LANES[row.lane].label}</TableTextCell>,
            },
            {
              key: "last_seen",
              header: "Last seen",
              align: "right",
              shrink: true,
              title: (row) => fmtDateTime(row.last_seen),
              cell: (row) => <TableDateCell value={row.last_seen} now={now} />,
            },
          ]}
        />
      </QueryState>
    </>
  );
}

/** The list's order, beside the filters in the page's toolbar. */
export function OrderMenu() {
  const [order, setOrder] = useParam("order", "", "filter");
  return (
    <Select
      size="sm"
      width="auto"
      value={order}
      onChange={(value) => setOrder(String(value))}
      options={ORDERS.map((one) => ({ value: one.value, label: one.label }))}
      ariaLabel="Order"
      active={order !== ""}
    />
  );
}
