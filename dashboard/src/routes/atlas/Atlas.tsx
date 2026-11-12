/**
 * Company › Atlas: everything the company runs on, drawn as a map read the
 * way a request travels, or as a list one press away. Both shapes are one
 * answer: the filters, the selection and the peek carry between them.
 *
 * The coverage line says how many entries the atlas holds and where they come
 * from, and what a read could not read in full. It never says the atlas is
 * complete, because it never is.
 */

import { useEffect, useMemo, useRef } from "react";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  Input,
  PageNote,
  SegmentedControl,
  Select,
  Skeleton,
  Switch,
  type SelectOption,
} from "@crewlethq/ui";
import { useFillScreen } from "~/app/fill.tsx";
import { usePeek, usePeekControls } from "~/app/frame/peekAddress.ts";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { useDraft } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import type {
  AtlasFacetsAnswer,
  AtlasHealth,
  AtlasKind,
  AtlasLane,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";
import { useQuiet } from "~/lib/useQuiet.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import { AtlasList, OrderMenu } from "./AtlasList.tsx";
import { AtlasMap } from "./AtlasMap.tsx";
import {
  ATLAS_POLL_MS,
  narrowed as isNarrowed,
  queryParams,
  useAtlasFilters,
  useSetFilter,
  useShape,
  useTrace,
  type AtlasFilters,
  type Shape,
} from "./filters.ts";
import { VendorLead, coverageLine, coverageNotes } from "./parts.tsx";
import { HEALTH, KINDS, KIND_ORDER, LANES, VENDORS, WORDS, entries } from "./words.ts";
import "~/styles/atlas.css";

/** What fills the atlas in this build, said where it is empty. */
export const FILLS =
  "It fills from what the company settings declare, from Gitlet, and from what the engine already records about the integrations, servers, tool servers and models it uses.";

const SHAPES: readonly { value: Shape; label: string; glyph: "map" | "list" }[] = [
  { value: "map", label: "Map", glyph: "map" },
  { value: "list", label: "List", glyph: "list" },
];

export function Atlas() {
  const [shape, setShape] = useShape();
  const filters = useAtlasFilters();
  const params = useMemo(() => queryParams(filters), [filters]);
  const narrowed = isNarrowed(filters);
  const held = useHeldGrants() ?? [];
  useFillScreen(shape === "map");

  const facets = useQuery("atlas_facets", params, { pollMs: ATLAS_POLL_MS });

  return (
    <div className={shape === "map" ? "atlas-screen atlas-screen--fill" : "atlas-screen"}>
      <PageToolbar>
        <SegmentedControl<Shape>
          semantics="radio"
          activate="manual"
          size="sm"
          label="Draw as"
          value={shape}
          onValueChange={setShape}
          options={SHAPES.map((one) => ({
            value: one.value,
            icon: <NamedGlyph name={one.glyph} size="sm" />,
            srLabel: one.label,
            title: one.label,
          }))}
        />
        <Filters filters={filters} facets={facets.data} />
        {shape === "list" && <OrderMenu />}
      </PageToolbar>
      <Coverage facets={facets.data} narrowed={narrowed} />
      {shape === "map" ? (
        <MapShape params={params} narrowed={narrowed} held={held} />
      ) : (
        <AtlasList params={params} narrowed={narrowed} emptyHint={FILLS} />
      )}
    </div>
  );
}

/**
 * The coverage line, drawn once the first count arrives (a line of zeros is a
 * claim nobody made): how many entries and where from, then what the read
 * could not read in full. A note of the page, so it wraps on a phone.
 */
function Coverage({ facets, narrowed }: { facets: AtlasFacetsAnswer | null; narrowed: boolean }) {
  if (!facets) return null;
  const line =
    facets.total === 0 && !narrowed
      ? ["Nothing in the atlas yet", FILLS, ...coverageNotes(facets)]
      : [coverageLine(facets.total, facets.vendors), ...coverageNotes(facets)];
  return <PageNote className="atlas-coverage">{line.join(" · ")}</PageNote>;
}

/** The map, its selection the peek's entry. */
function MapShape({
  params,
  narrowed,
  held,
}: {
  params: Record<string, string | boolean>;
  narrowed: boolean;
  held: readonly string[];
}) {
  const map = useQuery("atlas_map", params, { pollMs: ATLAS_POLL_MS });
  const peek = usePeek();
  const { open, move, close } = usePeekControls();
  const [trace, setTrace] = useTrace();
  const selectedId = peek?.kind === "atlas" ? peek.id : null;
  const selected = useQuery(
    "atlas_entry",
    useMemo(() => ({ id: selectedId ?? "" }), [selectedId]),
    { enabled: selectedId !== null, pollMs: ATLAS_POLL_MS },
  );
  const select = (id: string | null) => {
    if (id === null) close();
    else if (peek) move({ kind: "atlas", id });
    else open({ kind: "atlas", id });
  };
  const fitKey = useMemo(() => JSON.stringify(params), [params]);

  if (map.loading && !map.data) return <Skeleton variant="box" label="Loading the map" />;
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
      selected={
        selectedId !== null && selected.data?.entry.id === selectedId ? selected.data : null
      }
      selectedId={selectedId}
      onSelect={select}
      trace={trace}
      onTrace={setTrace}
      narrowed={narrowed}
      held={held}
      fitKey={fitKey}
    />
  );
}

/** One menu's options: every value with entries, the chosen one too, each with its count. */
function facetOptions<V extends string>(
  all: string,
  order: readonly V[],
  counts: Partial<Record<V, number>> | undefined,
  chosen: string,
  label: (value: V) => string,
  leading?: (value: V) => SelectOption["leading"],
): SelectOption[] {
  const shown = order.filter((value) => (counts?.[value] ?? 0) > 0 || value === chosen);
  return [
    { value: "", label: all },
    ...shown.map((value) => ({
      value,
      label: label(value),
      hint: counts ? (counts[value] ?? 0).toLocaleString() : undefined,
      leading: leading?.(value),
    })),
  ];
}

/** The search and the filters, each counted over every other filter by the engine. */
function Filters({ filters, facets }: { filters: AtlasFilters; facets: AtlasFacetsAnswer | null }) {
  const { set, clear } = useSetFilter();
  const [draft, setDraft] = useDraft(filters.q);
  const settled = useQuiet(draft.trim());
  // ONLY A NEWLY SETTLED PHRASE MOVES THE ADDRESS: Back moving the address
  // reseeds the draft, and the phrase still settling must not undo it.
  const last = useRef(settled);
  useEffect(() => {
    if (last.current === settled) return;
    last.current = settled;
    if (settled !== filters.q) set("q", settled);
  }, [settled, filters.q, set]);
  const hidden = facets?.words.unconfirmed ?? 0;
  return (
    <>
      <Input
        inputSize="sm"
        type="search"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onClear={() => setDraft("")}
        clearLabel="Clear the search"
        leading={<SearchGlyph size="sm" />}
        placeholder="Name or key"
        aria-label="Search the atlas"
        width="sm"
      />
      <Select
        size="sm"
        width="auto"
        value={filters.kind}
        onChange={(value) => set("kind", String(value))}
        ariaLabel="Kind"
        active={filters.kind !== ""}
        options={facetOptions<AtlasKind>(
          "Every kind",
          KIND_ORDER,
          facets?.kinds,
          filters.kind,
          (kind) => KINDS[kind].word,
        )}
      />
      <Select
        size="sm"
        width="auto"
        value={filters.lane}
        onChange={(value) => set("lane", String(value))}
        ariaLabel="Lane"
        active={filters.lane !== ""}
        options={facetOptions<AtlasLane>(
          "Every lane",
          Object.keys(LANES) as AtlasLane[],
          facets?.lanes,
          filters.lane,
          (lane) => LANES[lane].label,
        )}
      />
      <Select
        size="sm"
        width="auto"
        value={filters.vendor}
        onChange={(value) => set("vendor", String(value))}
        ariaLabel="Vendor"
        active={filters.vendor !== ""}
        options={facetOptions<AtlasVendor>(
          "Every vendor",
          Object.keys(VENDORS) as AtlasVendor[],
          facets?.vendors,
          filters.vendor,
          (vendor) => VENDORS[vendor].name,
          (vendor) => (
            <VendorLead vendor={vendor} />
          ),
        )}
      />
      <Select
        size="sm"
        width="auto"
        value={filters.health}
        onChange={(value) => set("health", String(value))}
        ariaLabel="Health"
        active={filters.health !== ""}
        options={facetOptions<AtlasHealth>(
          "Any health",
          Object.keys(HEALTH) as AtlasHealth[],
          facets?.health,
          filters.health,
          (health) => HEALTH[health].label,
        )}
      />
      <Select
        size="sm"
        width="auto"
        value={filters.confidence}
        onChange={(value) => set("confidence", String(value))}
        ariaLabel="Confidence"
        active={filters.confidence !== ""}
        options={facetOptions<AtlasWord>(
          "Any confidence",
          Object.keys(WORDS) as AtlasWord[],
          facets?.words,
          filters.confidence,
          (word) => WORDS[word],
        )}
      />
      <Switch
        size="sm"
        label={`Show unconfirmed (${hidden.toLocaleString()})`}
        checked={filters.unconfirmed || filters.confidence === "unconfirmed"}
        disabledReason={
          filters.confidence === "unconfirmed"
            ? "Shown, because the confidence filter asks for unconfirmed entries"
            : undefined
        }
        onCheckedChange={(on) => set("unconfirmed", on ? "true" : "")}
        title={`${entries(hidden)} only an agent reported, with nothing the engine could check. Hidden until a person confirms them.`}
      />
      {isNarrowed(filters) && (
        <Button variant="ghost" size="small" onClick={clear}>
          Clear filters
        </Button>
      )}
    </>
  );
}
