/**
 * Company › Structure: the org on one page, in two tabs over the same
 * builder, the Org chart and the Table.
 *
 * BOTH TABS SHOW THE RUNNING COMPANY. Each chart card and table row says its
 * seat's live state, one status filters both (the Table's chips, the chart's
 * legend in the canvas), and the Table carries every run-time control (pause,
 * pause and stop, resume, steer, and the remedy a state asks for). So there is
 * one place to watch the company and one to change it, and they are the same
 * page.
 *
 * BOTH TABS ARE THE BUILDER. A draft and its selection carry from one tab to
 * the other, and a reader who may not change the org reads the same page with
 * every edit held and its reason said (`builder/Builder.tsx`).
 *
 * A CHUNK OF ITS OWN. The builder is about a fifth of the dashboard's source,
 * more than any whole workspace, so a glance at a seat's page does not pay for
 * it (`app/lazyScreen.ts` names it as the `org` chunk).
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { Combobox, type ComboboxOption } from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePeek, usePeekControls } from "~/app/frame/peekAddress.ts";
import { useNavigator, useRoute, useTrailOrigin, type Route } from "~/app/router.tsx";
import {
  EDITOR_OPEN,
  resolve,
  STRUCTURE_PATH,
  STRUCTURE_TABLE_PATH,
  type MovedOrgPage,
  type StructureTab,
} from "~/app/routes.ts";
import { screenScroller } from "~/lib/scroller.ts";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { useSectionCounts } from "~/app/Shell.tsx";
import { unitPath } from "~/lib/orgchart.ts";
import { useLiveOrg } from "~/lib/seatLive.ts";
import { handleLabel, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { Builder } from "./builder/Builder.tsx";
import { builderSurfaces } from "./builder/surfaces.ts";
import { StructureLiveProvider } from "./structureLive.tsx";

/** How long a scroll rests before the address records it: the end of a scroll, not each frame. */
const SCROLL_SETTLE_MS = 150;

export function Structure({ tab }: { tab: StructureTab }) {
  const live = useLiveOrg();
  const nav = useNavigator();
  // A SEAT OPENED FROM HERE, whichever way, names this place as where it was
  // opened from, and its trail and crumbs come back to exactly it.
  useTrailOrigin(isSeatPage);
  // AND HOW FAR DOWN THE READER IS, kept in the address (`at`), which a crumb
  // back here opens at; a Back and a reload find it in the entry's own state.
  useEffect(() => {
    const el = screenScroller();
    if (!el) return;
    let timer = 0;
    const note = () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(() => {
        nav.note({ at: el.scrollTop > 0 ? String(Math.round(el.scrollTop)) : null });
      }, SCROLL_SETTLE_MS);
    };
    el.addEventListener("scroll", note, { passive: true });
    return () => {
      el.removeEventListener("scroll", note);
      window.clearTimeout(timer);
    };
  }, [nav]);
  // THE TABLE COUNTS THE SEATS, once the org has arrived: a 0 before it is a claim.
  useSectionCounts(live.loaded ? { table: String(live.index.seats.length) } : {});
  return (
    <StructureLiveProvider value={live}>
      <PageActions>
        <FindSeat index={live.index} />
      </PageActions>
      <Builder surfaces={builderSurfaces} view={tab} />
    </StructureLiveProvider>
  );
}

/** Whether a move goes to a seat's page, which carries Structure as where it was opened from. */
function isSeatPage(to: Route): boolean {
  const where = resolve(to.path);
  return where.resolved && where.screen === "seat";
}

/** How many matches the finder lists: a screenful, never the company. */
const FIND_LIMIT = 8;

/**
 * The seats a finder term matches, best first: a name or handle that STARTS
 * with the term before one that merely contains it, then one whose unit does,
 * then the chart's order.
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
 * The page bar's seat finder: a found seat opens in the seat panel over the
 * tab, so the place the reader found it from stays under it. `/` focuses it.
 */
function FindSeat({ index }: { index: OrgIndex }) {
  const [term, setTerm] = useState("");
  const [open, setOpen] = useState(false);
  const box = useRef<HTMLInputElement>(null);
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
          const ref = { kind: "seat" as const, id: seat.handle };
          // A PANEL ALREADY OPEN IS MOVED, not stacked: Back from the found
          // seat leaves the page, as `[` and `]` do.
          if (peek) move(ref);
          else openPeek(ref);
          setTerm("");
          setOpen(false);
        }}
      />
    </span>
  );
}

/**
 * Where an earlier org page's address is now, with what its query asked for
 * said in Structure's terms: Edit org is the Org chart (its `view=table` the
 * Table, and a node it named opens its editor, as the link meant), and the
 * Roster, the Teams list and a team's page are the Table, a team's with its
 * panel open. Whatever panel was open stays open.
 */
export function movedTo(
  was: MovedOrgPage,
  unit: string | undefined,
  query: URLSearchParams,
): { path: readonly string[]; query: Record<string, string> } {
  if (was === "edit") {
    const kept = Object.fromEntries([...query].filter(([key]) => key !== "view"));
    const names = (kept.seat ?? "") !== "" || (kept.unit ?? "") !== "";
    return {
      path: query.get("view") === "table" ? STRUCTURE_TABLE_PATH : STRUCTURE_PATH,
      query: names && !kept.add ? { ...kept, ...EDITOR_OPEN } : kept,
    };
  }
  const peek = query.get("peek");
  return {
    path: STRUCTURE_TABLE_PATH,
    query: unit ? { peek: `unit:${unit}` } : peek ? { peek } : {},
  };
}

/** An earlier org page's address, said again by replace as the Structure tab holding it. */
export function StructureMoved({ was, unit }: { was: MovedOrgPage; unit?: string | undefined }) {
  const route = useRoute();
  const nav = useNavigator();
  useEffect(() => {
    const to = movedTo(was, unit, route.query);
    nav.replace(to.path, to.query);
  }, [was, unit, route.query, nav]);
  return null;
}
