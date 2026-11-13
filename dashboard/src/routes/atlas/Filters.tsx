/**
 * What narrows the atlas: the search, and one Filter control that holds the
 * five filters and the unconfirmed switch. The toolbar row is the search and
 * that control, nothing else; the filters in force are said under it as
 * chips, each removed on its own, and only while one is set.
 *
 * Every menu lists the values with entries and counts each over every other
 * filter, as the engine counts them (`atlas_facets`).
 */

import { useEffect, useRef, type ReactNode } from "react";
import { ListFilterGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  Count,
  FilterPill,
  Input,
  Popover,
  Select,
  Switch,
  Text,
  type SelectOption,
} from "@crewlethq/ui";
import { useDraft } from "~/app/router.tsx";
import type {
  AtlasFacetsAnswer,
  AtlasHealth,
  AtlasKind,
  AtlasLane,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";
import { plural } from "~/lib/format.ts";
import { useQuiet } from "~/lib/useQuiet.ts";
import { FILTER_KEYS, useSetFilter, type AtlasFilters } from "./filters.ts";
import { VendorLead } from "./parts.tsx";
import { HEALTH, KINDS, KIND_ORDER, LANES, VENDORS, WORDS, entries } from "./words.ts";

type FilterKey = (typeof FILTER_KEYS)[number];

/** The filters the control holds: every narrowing but the search, which has its own box. */
const HELD = FILTER_KEYS.filter((key) => key !== "q");

/** The filters in force, as a chip says each: its field and the value chosen. */
export function chipsOf(
  filters: AtlasFilters,
): { key: FilterKey; field: string; value?: string }[] {
  const out: { key: FilterKey; field: string; value?: string }[] = [];
  if (filters.kind !== "")
    out.push({ key: "kind", field: "Kind", value: KINDS[filters.kind].word });
  if (filters.lane !== "")
    out.push({ key: "lane", field: "Lane", value: LANES[filters.lane].label });
  if (filters.vendor !== "") {
    out.push({ key: "vendor", field: "Vendor", value: VENDORS[filters.vendor].name });
  }
  if (filters.health !== "") {
    out.push({ key: "health", field: "Health", value: HEALTH[filters.health].label });
  }
  if (filters.confidence !== "") {
    out.push({ key: "confidence", field: "Confidence", value: WORDS[filters.confidence] });
  }
  if (filters.unconfirmed) out.push({ key: "unconfirmed", field: "Unconfirmed shown" });
  return out;
}

/** The search: a name or a key, asked once the typing settles. */
export function SearchBox({ q }: { q: string }) {
  const { set } = useSetFilter();
  const [draft, setDraft] = useDraft(q);
  const settled = useQuiet(draft.trim());
  // ONLY A NEWLY SETTLED PHRASE MOVES THE ADDRESS: Back moving the address
  // reseeds the draft, and the phrase still settling must not undo it.
  const last = useRef(settled);
  useEffect(() => {
    if (last.current === settled) return;
    last.current = settled;
    if (settled !== q) set("q", settled);
  }, [settled, q, set]);
  return (
    <div className="atlas-search">
      <Input
        inputSize="sm"
        type="search"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onClear={() => setDraft("")}
        clearLabel="Clear the search"
        leading={<SearchGlyph size="sm" />}
        placeholder="Find an entry by its name or its key"
        aria-label="Search the atlas: the map and the catalogue narrow together"
      />
    </div>
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

/**
 * The one Filter control beside the search: a button that says how many
 * filters are in force, opening the five menus, the unconfirmed switch and
 * the way to clear them all.
 */
export function FilterControl({
  filters,
  facets,
}: {
  filters: AtlasFilters;
  facets: AtlasFacetsAnswer | null;
}) {
  const { set, clearKeys } = useSetFilter();
  const active = chipsOf(filters).length;
  const hidden = facets?.words.unconfirmed ?? 0;
  return (
    <Popover
      role="dialog"
      label="Filter the atlas"
      align="start"
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="secondary"
          leadingIcon={<ListFilterGlyph size="sm" />}
          aria-expanded={open}
          aria-haspopup="dialog"
          onClick={toggle}
        >
          Filter
          {active > 0 && (
            <span className="atlas-filter-count">
              <Count
                value={active}
                tone="attention"
                label={`${plural(active, "filter")} in force`}
              />
            </span>
          )}
        </Button>
      )}
    >
      <div className="atlas-filters">
        <FilterRow label="Kind">
          <Select
            size="sm"
            width="full"
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
        </FilterRow>
        <FilterRow label="Lane">
          <Select
            size="sm"
            width="full"
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
        </FilterRow>
        <FilterRow label="Vendor">
          <Select
            size="sm"
            width="full"
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
        </FilterRow>
        <FilterRow label="Health">
          <Select
            size="sm"
            width="full"
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
        </FilterRow>
        <FilterRow label="Confidence">
          <Select
            size="sm"
            width="full"
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
        </FilterRow>
        <div className="atlas-filters__foot">
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
          {active > 0 && (
            <Button variant="ghost" size="small" onClick={() => clearKeys(HELD)}>
              Clear all filters
            </Button>
          )}
        </div>
      </div>
    </Popover>
  );
}

/** One filter in the panel: its name, then its menu. */
function FilterRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="atlas-filters__row">
      <Text variant="label">{label}</Text>
      {children}
    </div>
  );
}

/**
 * The filters in force, each a chip removed on its own, with the way to
 * clear them all. Nothing is drawn while none is set.
 */
export function ActiveFilters({ filters }: { filters: AtlasFilters }) {
  const { set, clearKeys } = useSetFilter();
  const chips = chipsOf(filters);
  if (chips.length === 0) return null;
  return (
    <div className="atlas-chosen" role="group" aria-label="Filters in force">
      {chips.map((chip) => (
        <FilterPill
          key={chip.key}
          field={chip.field}
          operator={chip.value === undefined ? undefined : "is"}
          value={chip.value}
          onRemove={() => set(chip.key, "")}
        />
      ))}
      <Button variant="ghost" size="small" onClick={() => clearKeys(HELD)}>
        Clear all
      </Button>
    </div>
  );
}
