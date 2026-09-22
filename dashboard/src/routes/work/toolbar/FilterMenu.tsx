/**
 * One button that adds a filter, in place of a control per field.
 *
 * # Why a menu rather than a row of pickers
 *
 * The bar carried eleven controls: a search box, five selects, a three-way
 * scope switch, two chips, a Clear button and the count — and which of them
 * appeared depended on the shape, so the scope switch sat at three different
 * places on three tabs. Eleven controls is also a ceiling: the company's OWN
 * fields could never be among them, because a build cannot know what a company
 * declared, so the one half of the grammar that is a company's own was
 * unreachable from the screen entirely.
 *
 * A menu has no ceiling. Every field the grammar filters on is a row, the
 * company's own fields are rows beside them, and what is APPLIED is drawn as
 * chips under the bar rather than as a set of controls that all look alike.
 *
 * # Two stages in one panel
 *
 * Pick a field, then pick its value. A panel per field would be a popover
 * inside a popover; a flat list of every value of every field would be four
 * hundred rows. The back control is the field name, so the reader always knows
 * which question they are answering.
 *
 * # It offers nothing the engine would refuse
 *
 * Every value written here is one the query grammar takes: a status from the
 * project's own six, a type from the catalogue, a due alias `dates.go` expands
 * itself, and for a custom field the field's own natural comparison. Where a
 * type admits more than a bare value the panel takes the grammar verbatim and
 * says so in the placeholder, rather than inventing an operator control whose
 * spelling would be a second copy of a table the engine refuses against.
 */

import { useMemo, useState } from "react";
import { Button, Input, Popover, Tag } from "@crewlethq/ui";
import { CheckGlyph, ChevronLeftGlyph, SearchGlyph, TuneGlyph } from "@crewlethq/icons/glyphs";
import {
  DUE_FILTERS,
  PRIORITIES,
  STATUSES,
  statusLabel,
  type Shape,
  type TrackerFilters,
} from "~/lib/work.ts";
import { humanize } from "~/lib/format.ts";
import type { WorkFieldDef, WorkProjectTag, WorkStatusDef, WorkTypeDef } from "~/protocol/index.ts";

/** One value a field can be filtered to. */
interface FilterOption {
  value: string;
  label: string;
}

/**
 * A field the panel can narrow on.
 *
 * `options` is what the reader picks from; a field with none takes a typed
 * value, and `placeholder` is where the grammar for it is stated.
 */
interface FilterField {
  /** The URL key, which is also what the chip removes. */
  param: string;
  label: string;
  options?: FilterOption[];
  placeholder?: string;
  /** A field whose only values are on and off — one row, no second stage. */
  toggle?: boolean;
}

export interface FilterMenuProps {
  filters: TrackerFilters;
  /** The shape on screen, because the calendar spends the `due` key itself. */
  shape: Shape;
  /** Writes one URL key. An empty value clears it. */
  onSet: (param: string, value: string) => void;
  types?: WorkTypeDef[];
  statuses?: WorkStatusDef[];
  tags?: WorkProjectTag[];
  fields?: WorkFieldDef[];
  seats: { handle: string; name: string }[];
  /**
   * The handle the HOST screen fixed, where one did.
   *
   * A locked narrowing is not a filter: it is what the screen IS, so it is not
   * offered here, not drawn as a chip and not on the address. Offering the row
   * anyway would put a second answer to one question in front of the reader —
   * and choosing it would write a key the lock overwrites on the way to the
   * wire, which reads as a control that does nothing.
   */
  lockedAssignee?: string;
}

export function FilterMenu(props: FilterMenuProps) {
  const fields = useFilterFields(props);
  return (
    <Popover
      role="dialog"
      label="Filter by"
      align="start"
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="tertiary"
          leadingIcon={<TuneGlyph size="sm" />}
          aria-expanded={open}
          aria-haspopup="dialog"
          onClick={toggle}
        >
          Filter
        </Button>
      )}
    >
      {(close) => (
        <FilterPanel fields={fields} filters={props.filters} onSet={props.onSet} close={close} />
      )}
    </Popover>
  );
}

/**
 * The fields this company can be asked about, standard ones first.
 *
 * THE COMPANY'S OWN COME LAST AND IN ITS OWN ORDER — the catalogue's — because
 * that order is a decision somebody made, and re-sorting it alphabetically
 * here would throw it away. An ARCHIVED field is not offered: its values left
 * the value table, so a filter over one matches nothing for ever.
 */
function useFilterFields({
  shape,
  types,
  statuses,
  tags,
  fields,
  seats,
  lockedAssignee,
}: FilterMenuProps): FilterField[] {
  return useMemo(() => {
    const out: FilterField[] = [
      {
        param: "status",
        label: "Status",
        options: STATUSES.map((s) => ({ value: s.value, label: statusLabel(s.value, statuses) })),
      },
    ];
    const live = (types ?? []).filter((t) => !t.archived);
    if (live.length > 0) {
      out.push({
        param: "type",
        label: "Type",
        options: live.map((t) => ({ value: t.slug, label: t.name })),
      });
    }
    out.push({
      param: "priority",
      label: "Priority",
      options: PRIORITIES.map((p) => ({ value: p, label: humanize(p) })),
    });
    // NOT WHERE THE HOST ALREADY FIXED IT — see [FilterMenuProps.lockedAssignee].
    if (!lockedAssignee) {
      out.push({
        param: "assignee",
        label: "Assignee",
        // UNASSIGNED IS A VALUE, not a missing filter — it is the one question
        // a lead actually opens a board to ask.
        options: [
          { value: "none", label: "Unassigned" },
          ...seats.map((s) => ({ value: s.handle, label: s.name })),
        ],
      });
    }
    // A TAG SET BELONGS TO A PROJECT, so this row exists only where the
    // project's own tags are known. On the company-wide list there is no set
    // to offer, and a free-text tag box would offer every misspelling.
    if (tags && tags.length > 0) {
      out.push({
        param: "tag",
        label: "Tag",
        options: tags.filter((t) => !t.archived).map((t) => ({ value: t.slug, label: t.label })),
      });
    }
    // NOT ON THE CALENDAR, whose own window is this same key — see
    // `buildItemsParams`.
    if (shape !== "calendar") {
      out.push({
        param: "due",
        label: "Due",
        options: DUE_FILTERS.map((d) => ({ value: d.value, label: d.label })),
      });
    }
    out.push({ param: "blocked", label: "Blocked", toggle: true });
    out.push({ param: "removed", label: "Removed items", toggle: true });

    for (const def of fields ?? []) {
      if (def.archived) continue;
      out.push(customField(def, seats));
    }
    return out;
  }, [shape, types, statuses, tags, fields, seats, lockedAssignee]);
}

/**
 * One of the company's own fields, as a row of this panel.
 *
 * WHICH TYPES GET A LIST is decided by whether the field itself declares one:
 * a dropdown and a labels field carry their options, a checkbox has exactly
 * two values, and a people field is the roster. Everything else takes a typed
 * value, and the placeholder is where its grammar is stated — the operators a
 * type admits are a table in `internal/tracker/coerce.go` and the docs, and a
 * control that re-spelled them here would be a second copy of it.
 *
 * `null` AND `not_null` ARE ON EVERY TYPE, because "is this set at all" is a
 * question about the ROW rather than about the value.
 */
function customField(def: WorkFieldDef, seats: { handle: string; name: string }[]): FilterField {
  const param = `f.${def.slug}`;
  const set: FilterOption[] = [
    { value: "not_null", label: "Is set" },
    { value: "null", label: "Is not set" },
  ];
  const declared = def.config?.options ?? [];
  if (declared.length > 0) {
    return {
      param,
      label: def.name,
      // AN ARCHIVED OPTION IS NOT OFFERED, for the reason an archived field is
      // not: a value nothing can hold any more is a filter that matches
      // nothing. The SLUG is what the grammar compares against and the NAME is
      // what the company calls it.
      options: [
        ...declared
          .filter((o) => !o.archived)
          .map((o) => ({ value: o.slug, label: o.name || o.slug })),
        ...set,
      ],
    };
  }
  if (def.type === "checkbox") {
    return {
      param,
      label: def.name,
      options: [{ value: "true", label: "Yes" }, { value: "false", label: "No" }, ...set],
    };
  }
  // A PEOPLE FIELD'S VALUES ARE THE ROSTER, which the panel already holds for
  // the assignee row — so it is offered as a list rather than as a box where a
  // reader guesses at a handle. Its natural comparison is `any`, so one name
  // is a value and several are written `any:a,b` on the address.
  if (def.type === "people") {
    return {
      param,
      label: def.name,
      options: [...seats.map((s) => ({ value: s.handle, label: s.name })), ...set],
    };
  }
  return { param, label: def.name, placeholder: placeholderFor(def.type) };
}

/**
 * What to type for a field of this type, in the grammar's own spelling.
 *
 * A BARE VALUE IS THE TYPE'S NATURAL COMPARISON — `any` on a set and `eq`
 * everywhere else — which is the engine's rule rather than this panel's, so
 * the simple case needs no operator at all and the placeholder shows the one
 * form a reader would otherwise have to look up.
 */
function placeholderFor(type: string): string {
  switch (type) {
    case "number":
    case "progress":
    case "rollup":
      return "12, or gte:3, or range:3..8";
    case "date":
      return "2026-03-01, or gte:today, or range:sow..eom";
    case "people":
    case "relationship":
      return "a handle, or any:a,b — or not_null";
    case "textarea":
      return "text it contains";
    default:
      return "a value, or contains:…";
  }
}

/** The panel itself: the field list, or one field's values. */
function FilterPanel({
  fields,
  filters,
  onSet,
  close,
}: {
  fields: FilterField[];
  filters: TrackerFilters;
  onSet: (param: string, value: string) => void;
  close: () => void;
}) {
  const [param, setParam] = useState("");
  const [find, setFind] = useState("");
  const [typed, setTyped] = useState("");
  const chosen = fields.find((f) => f.param === param);

  const applied = (field: FilterField): string => {
    if (field.param === "blocked") return filters.blocked ? "true" : "";
    if (field.param === "removed") return filters.removed ? "true" : "";
    if (field.param === "due") return filters.due;
    if (field.param.startsWith("f.")) return filters.fields[field.param] ?? "";
    return (
      {
        status: filters.status,
        type: filters.type,
        priority: filters.priority,
        assignee: filters.assignee,
        tag: filters.tag,
      }[field.param] ?? ""
    );
  };

  if (!chosen) {
    const needle = find.trim().toLowerCase();
    const shown = needle ? fields.filter((f) => f.label.toLowerCase().includes(needle)) : fields;
    return (
      <div className="work-menu">
        <div className="work-menu-find">
          <Input
            type="search"
            width="full"
            value={find}
            onChange={(e) => setFind(e.target.value)}
            aria-label="Find a field"
            leading={<SearchGlyph size="sm" />}
            placeholder="Find a field…"
            autoFocus
          />
        </div>
        <div className="work-menu-list">
          {shown.map((field) => {
            const on = applied(field);
            return (
              <button
                key={field.param}
                type="button"
                className="work-menu-row"
                onClick={() => {
                  // A SWITCH HAS NO SECOND STAGE: there is one thing to say
                  // about it and the row says it, so the panel closes on the
                  // answer rather than showing a list of two.
                  if (field.toggle) {
                    onSet(field.param, on ? "" : "true");
                    close();
                    return;
                  }
                  setParam(field.param);
                  setTyped(on);
                }}
              >
                <span className="truncate">{field.label}</span>
                {on ? (
                  <Tag size="xs" appearance="outline">
                    on
                  </Tag>
                ) : null}
                {/* THE KEY IT WRITES, because this product's own address bar is
                    a surface people read: a reader who has seen `assignee=` in
                    a URL can then write one. */}
                <span className="work-menu-key mono">{field.param}=</span>
              </button>
            );
          })}
          {shown.length === 0 && <div className="work-menu-empty">No field by that name.</div>}
        </div>
      </div>
    );
  }

  const current = applied(chosen);
  return (
    <div className="work-menu">
      <div className="work-menu-head">
        <Button
          size="small"
          variant="tertiary"
          leadingIcon={<ChevronLeftGlyph size="sm" />}
          onClick={() => setParam("")}
        >
          {chosen.label}
        </Button>
      </div>
      {chosen.options ? (
        <div className="work-menu-list">
          {chosen.options.map((option) => (
            <button
              key={option.value}
              type="button"
              className="work-menu-row"
              aria-pressed={current === option.value}
              onClick={() => {
                // CHOOSING WHAT IS ALREADY CHOSEN TAKES IT OFF, which is the
                // only way back to "any" without leaving the panel — and it is
                // what the pressed state above promises.
                onSet(chosen.param, current === option.value ? "" : option.value);
                close();
              }}
            >
              <span className="work-menu-check">
                {current === option.value ? <CheckGlyph size="sm" /> : null}
              </span>
              <span className="truncate">{option.label}</span>
            </button>
          ))}
        </div>
      ) : (
        <form
          className="work-menu-form"
          onSubmit={(e) => {
            e.preventDefault();
            onSet(chosen.param, typed.trim());
            close();
          }}
        >
          <Input
            width="full"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            aria-label={`${chosen.label} filter`}
            placeholder={chosen.placeholder}
            autoFocus
          />
          <div className="work-menu-actions">
            {current && (
              <Button
                size="small"
                variant="tertiary"
                onClick={() => {
                  onSet(chosen.param, "");
                  close();
                }}
              >
                Remove
              </Button>
            )}
            <Button size="small" type="submit">
              Apply
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}
