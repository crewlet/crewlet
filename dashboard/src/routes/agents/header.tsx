/**
 * What every Agents section carries in the page bar: a way to find a seat, the
 * way into the builder, and the one write the org chart invites — adding a
 * seat — plus the section tabs' figures.
 *
 * # One header for every section
 *
 * The chart, the roster, the teams and the schedules are four ways of looking
 * at one company, and a reader moving between them expects the same controls
 * in the same place. Written per screen, the chart had "Edit org", the roster
 * had a pair of count tags where the controls should be, and Schedules had
 * none at all, so the bar changed shape with every tab.
 *
 * # Find a seat goes to the seat — or, on the roster, narrows to it
 *
 * On the chart a found seat is FOCUSED on the canvas and opened in the peek;
 * on Teams and Schedules it is opened in the peek. The list is the applied chart's seats —
 * no query — matched on name, handle and unit, because those are the three
 * things a reader remembers somebody by.
 *
 * ON THE ROSTER THE SAME FIELD IS THE LIST'S FILTER. The roster is a list of
 * every seat, and finding a seat in a list is narrowing the list to it — so
 * the field in the bar filters the rows under it (`q=`) rather than floating a
 * second list of seats over the first. It is one field, in one place, on every
 * section: the roster used to leave the bar's field out and draw a box of its
 * own in its toolbar, so the bar changed shape as a reader moved between the
 * tabs, and the roster alone had no "Find a seat" where every other section
 * put it.
 */

import { useMemo, useRef, useState } from "react";
import { Button, ButtonLink, Combobox, Input, type ComboboxOption } from "@crewlethq/ui";
import { PencilGlyph, PlusGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePeekControls, usePeek } from "~/app/frame/DetailRail.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { usePageMenu, useSectionCounts } from "~/app/Shell.tsx";
import { useSchedules } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { handleLabel, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { unitPath } from "~/lib/orgchart.ts";

/** How many matches the finder lists: a screenful, never the company. */
const FIND_LIMIT = 8;

/** The reason Add seat cannot be pressed by somebody who may not change the org. */
export const ADD_SEAT_REASON =
  "Changing the org needs an operator token — the company configuration is guarded";

/**
 * The seats a finder term matches, best first: a name or handle that STARTS
 * with the term before one that merely contains it, then the chart's order.
 */
export function findSeats(index: OrgIndex, term: string, limit = FIND_LIMIT): Seat[] {
  const needle = term.trim().toLowerCase();
  if (!needle) return [];
  const scored: { seat: Seat; score: number; at: number }[] = [];
  index.seats.forEach((seat, at) => {
    const name = seat.name.toLowerCase();
    const handle = seat.handle.toLowerCase();
    const place = unitPath(seat.unit).toLowerCase();
    const score =
      name.startsWith(needle) || handle.startsWith(needle)
        ? 0
        : name.includes(needle) || handle.includes(needle)
          ? 1
          : place.includes(needle)
            ? 2
            : -1;
    if (score >= 0) scored.push({ seat, score, at });
  });
  scored.sort((a, b) => a.score - b.score || a.at - b.at);
  return scored.slice(0, limit).map((s) => s.seat);
}

/**
 * The section figures every Agents section publishes: seats, units and
 * schedules — each from what this client already holds (the org projection
 * and the schedules push), so a figure costs no read.
 */
export function useAgentsCounts(index: OrgIndex): void {
  const schedules = useSchedules();
  useSectionCounts({
    roster: String(index.seats.length),
    ...(index.units.length ? { teams: String(index.units.length) } : {}),
    ...(schedules.length ? { schedules: String(schedules.length) } : {}),
  });
}

/** The roster's filter, as the bar's field drives it: see the file head. */
export interface SeatFilter {
  value: string;
  onChange: (next: string) => void;
}

/**
 * The page bar's controls for an Agents section.
 *
 * `onFound` is what a found seat does besides opening its peek — the chart
 * focuses its card. `filter` makes the bar's field the section's own filter
 * instead (the roster), so a list of every seat is narrowed where it is drawn.
 */
export function AgentsHeader({
  index,
  onFound,
  filter,
}: {
  index: OrgIndex;
  onFound?: (seat: Seat) => void;
  filter?: SeatFilter;
}) {
  const viewer = useViewer();
  const nav = useNavigator();
  const editable = viewer.operator;
  // ON A PHONE, Edit org FOLDS INTO "More": the bar keeps the finder and the
  // one action the chart invites in view, where the three side by side put
  // Add seat past the window's edge.
  usePageMenu([
    {
      key: "edit-org",
      label: "Edit org",
      icon: <PencilGlyph size="sm" />,
      onSelect: () => nav.to(["agents", "edit"]),
    },
  ]);
  return (
    <PageActions>
      {filter ? <FilterSeats {...filter} /> : <FindSeat index={index} onFound={onFound} />}
      <span className="page-action-folds">
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["agents", "edit"])}
          leadingIcon={<PencilGlyph size="sm" />}
        >
          Edit org
        </ButtonLink>
      </span>
      {/* NEVER HIDDEN: a reader without the credential sees the control and
          the reason, rather than a product with no way to grow its company. */}
      {editable ? (
        <ButtonLink
          size="small"
          variant="primary"
          href={href(["agents", "edit"], { add: "agent" })}
          leadingIcon={<PlusGlyph size="sm" />}
        >
          Add seat
        </ButtonLink>
      ) : (
        <Button
          size="small"
          variant="primary"
          leadingIcon={<PlusGlyph size="sm" />}
          disabledReason={viewer.loading ? "Checking your access" : ADD_SEAT_REASON}
          title={viewer.loading ? "Checking your access" : ADD_SEAT_REASON}
        >
          Add seat
        </Button>
      )}
    </PageActions>
  );
}

/**
 * The page bar's field on the roster: "Find a seat", narrowing the list under
 * it — see the file head. `/` focuses it, as it focuses the finder elsewhere.
 */
function FilterSeats({ value, onChange }: SeatFilter) {
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box);
  return (
    <span className="agents-find">
      <Input
        ref={box}
        type="search"
        inputSize="sm"
        width="full"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onClear={() => onChange("")}
        clearLabel="Clear the seat filter"
        leading={<SearchGlyph size="sm" />}
        aria-label="Find a seat"
        placeholder="Find a seat"
      />
    </span>
  );
}

/** The page bar's seat finder. */
function FindSeat({ index, onFound }: { index: OrgIndex; onFound?: (seat: Seat) => void }) {
  const [term, setTerm] = useState("");
  const [open, setOpen] = useState(false);
  const box = useRef<HTMLInputElement>(null);
  // `/` IS "SEARCH WHAT I AM LOOKING AT", and on an Agents section that is
  // the company's seats.
  useSearchTarget(box);
  const peek = usePeek();
  const { open: openPeek, move } = usePeekControls();
  const found = useMemo(() => findSeats(index, term), [index, term]);
  const options = useMemo<ComboboxOption[]>(
    () =>
      found.map((seat) => ({
        value: seat.key,
        label: seat.name,
        hint: [handleLabel(seat.handle), unitPath(seat.unit)].filter(Boolean).join(" · "),
      })),
    [found],
  );
  return (
    <span className="agents-find">
      <Combobox
        ref={box}
        inputSize="sm"
        label="Find a seat"
        placeholder="Find a seat"
        leading={<SearchGlyph size="sm" />}
        value={term}
        onValueChange={(next) => {
          setTerm(next);
          setOpen(next.trim() !== "");
        }}
        open={open && term.trim() !== ""}
        onOpenChange={setOpen}
        options={options}
        emptyMessage="No seat by that name, handle or unit"
        onCommit={(option) => {
          const seat = found.find((s) => s.key === option.value);
          if (!seat) return;
          const ref = { kind: "seat" as const, id: seat.handle || seat.name };
          // A PEEK ALREADY OPEN IS MOVED, not stacked: Back from the found
          // seat leaves the list, as `[` and `]` do.
          if (peek) move(ref);
          else openPeek(ref);
          onFound?.(seat);
          setTerm("");
          setOpen(false);
        }}
      />
    </span>
  );
}
