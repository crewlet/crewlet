/**
 * Company › Atlas: everything the company runs on, as one page. A band says
 * how many entries there are and how they are, the toolbar holds the search
 * and one Filter control, the map draws the entries the way a request
 * travels, and the catalogue under it lists the same entries grouped by
 * health. The map and the catalogue are one answer: one search, one set
 * of filters and one selection, read from the address by both.
 *
 * The coverage line says how many entries the atlas holds and where they come
 * from, and what a read could not read in full. It never says the atlas is
 * complete, because it never is.
 *
 * An entry that open tasks name says so on both: a notch on its card where
 * its health leaves the notch free, and a line in its catalogue row. That is
 * Worklet's answer, asked once for the page and only of a reader who uses it.
 */

import { useMemo, useState } from "react";
import { Button, PageNote, Skeleton } from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import { usePeek, usePeekControls } from "~/app/frame/peekAddress.ts";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { QueryState } from "~/components/common.tsx";
import type { AtlasFacetsAnswer } from "~/contract/atlas.ts";
import { WORKLET, holds } from "~/lib/access.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import { AddEntry } from "./AddEntry.tsx";
import { AtlasMap } from "./AtlasMap.tsx";
import { ATTENTION_SHOWN, Band } from "./Band.tsx";
import { Catalogue } from "./Catalogue.tsx";
import {
  ATLAS_POLL_MS,
  narrowed as isNarrowed,
  queryParams,
  useAtlasFilters,
  useSelection,
  useTrace,
  type AtlasFilters,
} from "./filters.ts";
import { ActiveFilters, FilterControl, SearchBox } from "./Filters.tsx";
import { coverageLine, coverageNotes } from "./parts.tsx";

/** What fills the atlas in this build, said where it is empty. */
export const FILLS =
  "It fills from what the company settings declare, from Gitlet, from what the engine already records about the integrations, servers, tool servers and models it uses, and from what people add here with Add entry.";

const NO_PARAMS = {};
const ONE = { limit: 1 };

export function Atlas() {
  const filters = useAtlasFilters();
  const params = useMemo(() => queryParams(filters), [filters]);
  const narrowed = isNarrowed(filters);
  const held = useHeldGrants() ?? [];
  const { selectedId, select } = useSelection();
  const { open } = usePeekControls();
  // A SHEET BESIDE THE PAGE takes a third of it, so the band keeps to one row.
  const sheet = usePeek() !== null;
  // THE ENTRY EITHER SURFACE POINTS AT, lit on the other: a pointer's rest,
  // not a place, so it lives here and never in the address.
  const [pointedId, setPointedId] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const add = useWriteAccess("atlas_add_entry");
  // WHAT WAITS FOR A PERSON, counted by the engine on the read: one item is
  // asked for, since the band says the count and Review lists the rest.
  const waiting = useQuery("atlas_review", ONE, { pollMs: ATLAS_POLL_MS });

  // THE BAND IS ABOUT THE COMPANY, so it reads the atlas whole (with the
  // unconfirmed entries when they are shown); the menus and the coverage line
  // read what the search and the filters leave. One question while nothing narrows.
  const whole = useMemo(
    () => (filters.unconfirmed ? { unconfirmed: true } : NO_PARAMS),
    [filters.unconfirmed],
  );
  const beyond = Object.keys(params).length > Object.keys(whole).length;
  const company = useQuery("atlas_facets", whole, { pollMs: ATLAS_POLL_MS });
  const left = useQuery("atlas_facets", params, { enabled: beyond, pollMs: ATLAS_POLL_MS });
  const facets = beyond ? left : company;
  const activity = useQuery("atlas_activity", NO_PARAMS, { pollMs: ATLAS_POLL_MS });
  const urgent = useQuery(
    "atlas_entries",
    useMemo(() => ({ ...whole, sort: "health", limit: ATTENTION_SHOWN }), [whole]),
    { pollMs: ATLAS_POLL_MS },
  );
  const attention = useMemo(
    () => (urgent.data?.rows ?? []).filter((row) => row.health !== "quiet"),
    [urgent.data],
  );
  const empty = company.data !== null && company.data.total === 0;
  // THE TRACKER IS WORKLET'S, so the open work is asked only of a reader
  // holding Worklet, once for the map and the catalogue alike.
  const marks = useQuery("atlas_work", NO_PARAMS, {
    enabled: holds(held, WORKLET),
    pollMs: ATLAS_POLL_MS,
  });
  const work = useMemo(
    () =>
      marks.data && !marks.data.unread ? new Map(Object.entries(marks.data.open ?? {})) : null,
    [marks.data],
  );

  return (
    <div className="atlas-screen">
      {company.data && !empty && (
        <Band
          facets={company.data}
          attention={attention}
          activity={activity}
          waiting={waiting.data ? waiting.data.counts : null}
          onShow={select}
          beside={sheet}
        />
      )}
      <PageToolbar>
        <SearchBox q={filters.q} />
        <FilterControl filters={filters} facets={facets.data} />
        <span className="atlas-toolbar__end">
          <Button
            variant="primary"
            size="small"
            leadingIcon={<PlusGlyph />}
            disabledReason={add.can ? undefined : add.reason}
            title={add.can ? undefined : add.reason}
            onClick={() => setAdding(true)}
          >
            Add entry
          </Button>
        </span>
      </PageToolbar>
      {adding && <AddEntry onClose={() => setAdding(false)} />}
      <ActiveFilters filters={filters} />
      <Coverage
        facets={facets.data}
        narrowed={narrowed}
        workCapped={work !== null && marks.data?.capped === true}
      />
      <MapPart
        filters={filters}
        params={params}
        held={held}
        work={work}
        selectedId={selectedId}
        onSelect={select}
        onDetails={(id) => open({ kind: "atlas", id })}
        pointedId={pointedId}
        onPoint={setPointedId}
      />
      <Catalogue
        params={params}
        facets={facets.data}
        activity={activity}
        work={work}
        narrowed={narrowed}
        emptyHint={FILLS}
        selectedId={selectedId}
        pointedId={pointedId}
        onPoint={setPointedId}
      />
    </div>
  );
}

/** Said on the coverage line where the engine stopped counting the entries with open work. */
const WORK_CAPPED =
  "More entries have open tasks than one read counts, so some are not marked as being worked on";

/**
 * The coverage line, drawn once the first count arrives (a line of zeros is a
 * claim nobody made): how many entries and where from, then what the read
 * could not read in full. A note of the page, so it wraps on a phone.
 */
function Coverage({
  facets,
  narrowed,
  workCapped,
}: {
  facets: AtlasFacetsAnswer | null;
  narrowed: boolean;
  /** The marks of open work are a part of them, which the line says. */
  workCapped: boolean;
}) {
  if (!facets) return null;
  const notes = [...coverageNotes(facets), ...(workCapped ? [WORK_CAPPED] : [])];
  const line =
    facets.total === 0 && !narrowed
      ? ["Nothing in the atlas yet", FILLS, ...notes]
      : [coverageLine(facets.total, facets.vendors), ...notes];
  return <PageNote>{line.join(" · ")}</PageNote>;
}

/**
 * The map. The filters narrow it as they narrow the catalogue; the search
 * does not: the map keeps its shape and pushes back what the search leaves
 * out, so a reader finds an entry where it stands. What matches is the
 * engine's own answer to the same question the catalogue asks.
 */
function MapPart({
  filters,
  params,
  held,
  work,
  selectedId,
  onSelect,
  onDetails,
  pointedId,
  onPoint,
}: {
  filters: AtlasFilters;
  params: Record<string, string | boolean>;
  held: readonly string[];
  work: ReadonlyMap<string, number> | null;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
  onDetails: (id: string) => void;
  pointedId: string | null;
  onPoint: (id: string | null) => void;
}) {
  const trace = useTrace();
  const drawn = useMemo(() => {
    const { q: _q, ...rest } = params;
    return { ...rest, relations: "all" };
  }, [params]);
  const map = useQuery("atlas_map", drawn, { pollMs: ATLAS_POLL_MS });
  const searching = filters.q !== "";
  const found = useQuery("atlas_map", params, { enabled: searching, pollMs: ATLAS_POLL_MS });
  const matched = useMemo(
    () => (searching && found.data ? new Set(found.data.nodes.map((node) => node.id)) : null),
    [searching, found.data],
  );
  const fitKey = useMemo(() => JSON.stringify(drawn), [drawn]);

  if (map.loading && !map.data) {
    return (
      <div className="atlas-map">
        <Skeleton variant="box" label="Loading the map" />
      </div>
    );
  }
  if (!map.data) {
    return (
      <QueryState error={map.error} refusal={map.refusal} detail={map.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return (
    <AtlasMap
      answer={map.data}
      work={work}
      matched={matched}
      selectedId={selectedId}
      onSelect={onSelect}
      onDetails={onDetails}
      pointedId={pointedId}
      onPoint={onPoint}
      trace={trace}
      narrowed={isNarrowed({ ...filters, q: "" })}
      held={held}
      fitKey={fitKey}
    />
  );
}
