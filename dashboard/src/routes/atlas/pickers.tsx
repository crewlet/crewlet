/**
 * The three things an atlas write names beside its entry: another entry, an
 * owner of the chart, and the Doclet page that documents it. Each is picked
 * from what its own read answers, never typed as free text.
 */

import { useId, useMemo, useState } from "react";
import { Button, FormField, Input, List, ListItem, Select, type SelectOption } from "@crewlethq/ui";
import type { AtlasKind, AtlasSummary } from "~/contract/atlas.ts";
import { useOrgIndex } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { KINDS } from "./words.ts";

/** The most entries a picker lists at once: the engine's largest page. */
const PICKER_ROWS = 500;

/**
 * One entry of the atlas, picked by its name or its key. The list is the
 * engine's own page by name; where the atlas holds more than one page, a
 * field narrows it by the engine's search, so no entry is out of reach.
 */
export function EntryPicker({
  label,
  helper,
  kinds,
  exclude,
  value,
  onChange,
  optional,
}: {
  label: string;
  helper?: string;
  /** The kinds that may be picked; every kind where absent. */
  kinds?: readonly AtlasKind[];
  /** An entry left out: the one the write is about. */
  exclude?: string;
  value: string;
  onChange: (id: string, entry: AtlasSummary | null) => void;
  optional?: boolean;
}) {
  const [q, setQ] = useState("");
  const narrowID = useId();
  const one = kinds?.length === 1 ? kinds[0] : undefined;
  const asked = useMemo(
    () => ({
      sort: "name",
      limit: PICKER_ROWS,
      unconfirmed: true,
      ...(one ? { kind: one } : {}),
      ...(q.trim() ? { q: q.trim() } : {}),
    }),
    [one, q],
  );
  const found = useQuery("atlas_entries", asked);
  const rows = useMemo(
    () =>
      (found.data?.rows ?? []).filter(
        (row) => row.id !== exclude && (!kinds || kinds.includes(row.kind)),
      ),
    [found.data, exclude, kinds],
  );
  const options: SelectOption[] = rows.map((row) => ({
    value: row.id,
    label: row.name,
    description: `${KINDS[row.kind].word} · ${row.key}`,
    text: `${row.name} ${row.key}`,
  }));
  const more = (found.data?.total ?? 0) > (found.data?.rows.length ?? 0) || q.trim() !== "";
  const none = found.loading
    ? "Reading the atlas"
    : (found.detail ?? "No entry of the atlas can be picked here.");
  return (
    <>
      {more && (
        <FormField
          label={`Narrow ${label.toLowerCase()}`}
          htmlFor={narrowID}
          helper="The atlas holds more entries than one list shows. Type part of a name or a key."
        >
          <Input id={narrowID} value={q} onChange={(event) => setQ(event.target.value)} />
        </FormField>
      )}
      <FormField label={label} helper={helper} optional={optional} required={!optional}>
        {(field) => (
          <Select
            id={field.id}
            value={value}
            options={options}
            searchable
            searchPlaceholder="Search by name or key"
            placeholder={optional ? "None" : "Pick an entry"}
            emptyMessage={none}
            onChange={(id) => onChange(String(id), rows.find((row) => row.id === id) ?? null)}
          />
        )}
      </FormField>
      {optional && value !== "" && (
        <div>
          <Button variant="ghost" size="small" onClick={() => onChange("", null)}>
            Clear {label.toLowerCase()}
          </Button>
        </div>
      )}
    </>
  );
}

/** Who answers for an entry: a seat by its handle or a unit by its key, or nobody. */
export type OwnerChoice = { kind: "seat" | "unit"; name: string } | null;

const NOBODY = "";

/** The owner as one list of the chart: its seats, then its units. */
export function OwnerField({
  value,
  onChange,
  nobody,
}: {
  value: OwnerChoice;
  onChange: (owner: OwnerChoice) => void;
  /** What picking nobody is called: "No owner yet", "Clear the owner". */
  nobody: string;
}) {
  const index = useOrgIndex();
  const options: SelectOption[] = [
    { value: NOBODY, label: nobody },
    ...index.seats.map((seat) => ({
      value: `seat:${seat.handle}`,
      label: seat.name || seat.handle,
      description: seat.handle,
      group: "Seats",
      text: `${seat.name} ${seat.handle}`,
    })),
    ...index.units.map((unit) => ({
      value: `unit:${unit.id}`,
      label: unit.name || unit.id,
      description: unit.id,
      group: "Units",
      text: `${unit.name} ${unit.id}`,
    })),
  ];
  return (
    <FormField
      label="Owner"
      helper="A seat or a unit of the chart answers for it. Never free text."
    >
      {(field) => (
        <Select
          id={field.id}
          value={value ? `${value.kind}:${value.name}` : NOBODY}
          options={options}
          searchable
          searchPlaceholder="Search the chart"
          onChange={(picked) => {
            const [kind, ...rest] = String(picked).split(":");
            onChange(kind === "seat" || kind === "unit" ? { kind, name: rest.join(":") } : null);
          }}
        />
      )}
    </FormField>
  );
}

/** At most this many pages a search of titles lists. */
const PAGE_HITS = 6;

/**
 * The Doclet page that documents an entry, found by words from its title.
 * Doclet answers the search as the reader, so a reader is offered only pages
 * they can open, and one who does not use Doclet is told so by its refusal.
 */
export function PageField({
  value,
  onChange,
  optional,
}: {
  value: { id: string; title: string } | null;
  onChange: (page: { id: string; title: string } | null) => void;
  optional?: boolean;
}) {
  const [q, setQ] = useState("");
  const inputID = useId();
  const words = q.trim();
  const found = useQuery(
    "pages",
    useMemo(() => ({ title: words, limit: PAGE_HITS }), [words]),
    { enabled: words.length >= 2 && value === null },
  );
  if (value) {
    return (
      <FormField label="Documented in" optional={optional}>
        <List variant="divided">
          <ListItem
            trailing={
              <Button variant="ghost" size="small" onClick={() => onChange(null)}>
                Remove
              </Button>
            }
          >
            <span className="truncate">{value.title}</span>
          </ListItem>
        </List>
      </FormField>
    );
  }
  const hits = found.data?.pages ?? [];
  return (
    <>
      <FormField
        label="Documented in"
        htmlFor={inputID}
        optional={optional}
        required={!optional}
        helper="A Doclet page, for anything longer than one line. Type words from its title."
      >
        <Input id={inputID} value={q} onChange={(event) => setQ(event.target.value)} />
      </FormField>
      {words.length >= 2 &&
        (found.loading && !found.data ? (
          <p className="muted">Searching Doclet</p>
        ) : found.error || found.refusal ? (
          <p className="muted">{found.detail ?? "Doclet could not be searched."}</p>
        ) : hits.length === 0 ? (
          <p className="muted">No page has that in its title.</p>
        ) : (
          <List variant="divided">
            {hits.map((page) => (
              <ListItem
                key={page.id}
                onClick={() => {
                  onChange({ id: page.id, title: page.title });
                  setQ("");
                }}
              >
                <span className="truncate">{page.title}</span>
              </ListItem>
            ))}
          </List>
        ))}
    </>
  );
}
