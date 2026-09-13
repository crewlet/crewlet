/**
 * One search for the whole product.
 *
 * Per-screen search boxes would mean one ranking rule per screen and as many
 * places to keep them agreeing; this reaches screens, seats, units, tools, and
 * any event / trace / turn id pasted out of a log, which is the actual way an
 * operator arrives at a detail page.
 *
 * It is a LAUNCHER, not a settings panel: it closes on every action, including
 * the ones that only change a filter.
 *
 * It is a modal on the layer stack (`useModal`), so Escape, the veil, the Tab
 * trap and the return of focus to whatever opened it behave exactly as they
 * do for every dialog and drawer. It used to close on its own Escape and on a
 * second, window-level one in the shell, and returned focus to nothing.
 *
 * IT IS A COMBOBOX. Focus stays in the input and the arrows move a highlight
 * through the results, which the input names with `aria-activedescendant`, so
 * a screen reader hears each result as it is reached. The results used to be
 * a listbox of buttons nothing pointed at: the highlight moved in silence, and
 * Tab walked through up to forty of them before it came back to the input.
 * The keys are the shared listbox's (`ui/useListbox.ts`), so the highlight
 * wraps and survives a shrinking list here exactly as it does in every field
 * that offers one; the list is the modal's whole body, so it is not a popup
 * of its own (`popup: false`).
 *
 * THE SHORTCUT THAT OPENED IT CLOSES IT, but only from inside it. The shell
 * listens on the window and only ever opens search, and only while no modal
 * is open; a chord pressed while another surface raised over search holds the
 * keyboard (a token dialog a refused request opened) belongs to that surface,
 * and closing search from beneath it would change a page the reader cannot
 * see.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";
import { ALL_NAV } from "./nav.ts";
import { useNavigator } from "./router.tsx";
import { useAgents, useOrg, useTools } from "~/lib/store-hooks.ts";
import { indexOrg, seatPath } from "~/lib/seats.ts";
import { ModalPanel } from "~/ui/Dialog.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { Kbd } from "~/ui/Kbd.tsx";
import { useListbox } from "~/ui/useListbox.ts";
import { useModal } from "~/ui/useModal.ts";

interface Hit {
  id: string;
  group: string;
  icon: IconName;
  label: string;
  hint: string;
  go: () => void;
}

/** Is this the shape of an id somebody pasted out of a log? */
const UUIDISH = /^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$/i;
const HEXISH = /^[0-9a-f]{16,64}$/i;

/**
 * The chord that opens search from anywhere and closes it from inside.
 *
 * Command on Apple platforms and Control elsewhere, and either is accepted on
 * both, as the hint (`ui/Kbd.tsx`'s `Mod`) promises. One definition because
 * the shell opens with it and the palette closes with it.
 */
export const SEARCH_SHORTCUT = ["Mod", "k"] as const;

/**
 * Every key that opens search, as `aria-keyshortcuts` spells them: the chord
 * under either modifier, and the bare "/" the shell also accepts.
 */
export const SEARCH_ARIA_KEYSHORTCUTS = "Control+K Meta+K /";

export function isSearchShortcut(e: { key: string; metaKey: boolean; ctrlKey: boolean }): boolean {
  return (e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k";
}

function score(text: string, q: string): number {
  const t = text.toLowerCase();
  const i = t.indexOf(q);
  if (i < 0) return -1;
  // A prefix match beats a match in the middle, and a short field beats a long
  // one, so typing "pm" finds the seat called PM rather than every seat whose
  // backstory mentions a PM.
  return (i === 0 ? 0 : 100 + i) + t.length / 100;
}

export function CommandPalette({ onClose }: { onClose: () => void }) {
  const nav = useNavigator();
  const agents = useAgents();
  const org = useOrg();
  const tools = useTools();
  const [q, setQ] = useState("");
  const listRef = useRef<HTMLDivElement>(null);
  const id = useId();
  const modal = useModal({ onClose });

  const index = useMemo(() => indexOrg(org), [org]);

  const hits = useMemo<Hit[]>(() => {
    const query = q.trim().toLowerCase();
    const out: { hit: Hit; rank: number }[] = [];
    const push = (hit: Hit, rank: number) => out.push({ hit, rank });

    // A pasted id is a destination, not a search term: offer it first and
    // exactly, rather than making the reader guess which screen takes it.
    if (UUIDISH.test(query) || HEXISH.test(query)) {
      push(
        {
          id: `event-${query}`,
          group: "Open by id",
          icon: "file",
          label: query,
          hint: "as an event",
          go: () => nav.to(["events", query]),
        },
        -3,
      );
      push(
        {
          id: `trace-${query}`,
          group: "Open by id",
          icon: "gitBranch",
          label: query,
          hint: "as a trace",
          go: () => nav.to(["traces", query]),
        },
        -2,
      );
      push(
        {
          id: `turn-${query}`,
          group: "Open by id",
          icon: "layers",
          label: query,
          hint: "as a turn",
          go: () => nav.to(["turns", query]),
        },
        -1,
      );
    }

    for (const item of ALL_NAV) {
      const s = query ? score(item.label, query) : 0;
      if (s < 0) continue;
      push(
        {
          id: `nav-${item.key}`,
          group: "Go to",
          icon: item.icon,
          label: item.label,
          hint: item.guarded ? `${item.hint} · needs a token` : item.hint,
          go: () => nav.to(item.path),
        },
        s,
      );
    }

    for (const seat of index.seats) {
      const s = query
        ? Math.min(
            ...[seat.name, seat.handle, seat.goal].map((f) => {
              const v = score(f, query);
              return v < 0 ? Infinity : v;
            }),
          )
        : 0;
      if (!Number.isFinite(s)) continue;
      const live = agents.find((a) => a.role === seat.name);
      push(
        {
          id: `seat-${seat.key}`,
          group: "Seats",
          icon: seat.kind === "human" ? "user" : "users",
          label: seat.name,
          hint:
            seat.kind === "human"
              ? "human teammate"
              : [seat.handle && `@${seat.handle}`, live?.state].filter(Boolean).join(" · "),
          go: () => nav.to(seatPath(seat)),
        },
        s + 1,
      );
    }

    for (const unit of index.units) {
      const s = query ? score(unit.name, query) : Infinity;
      if (s < 0 || !Number.isFinite(s)) continue;
      push(
        {
          id: `unit-${unit.key}`,
          group: "Units",
          icon: "sitemap",
          label: unit.name,
          hint: `${unit.type || "unit"}${unit.lead ? ` · lead ${unit.lead.name}` : ""}`,
          go: () => nav.to(["org"], { unit: unit.name }),
        },
        s + 2,
      );
    }

    if (query) {
      for (const tool of tools) {
        const s = score(tool.name, query);
        if (s < 0) continue;
        push(
          {
            id: `tool-${tool.name}`,
            group: "Tools",
            icon: "wrench",
            label: tool.name,
            hint: tool.source,
            go: () => nav.to(["tools"], { q: tool.name }),
          },
          s + 3,
        );
      }
      push(
        {
          id: "search-events",
          group: "Search",
          icon: "activity",
          label: `Events mentioning “${q.trim()}”`,
          hint: "the event log, filtered",
          go: () => nav.to(["activity"], { q: q.trim() }),
        },
        900,
      );
      push(
        {
          id: "search-knowledge",
          group: "Search",
          icon: "book",
          label: `Knowledge base for “${q.trim()}”`,
          hint: "live search, run as the company",
          go: () => nav.to(["knowledge"], { q: q.trim() }),
        },
        901,
      );
    }

    return out
      .sort((a, b) => a.rank - b.rank)
      .slice(0, 40)
      .map((r) => r.hit);
  }, [q, index, agents, tools, nav]);

  function open(hit: Hit | undefined): void {
    hit?.go();
    onClose();
  }

  const listbox = useListbox({
    id,
    open: true,
    count: hits.length,
    popup: false,
    onCommit: (at) => open(hits[at]),
    onClose,
  });

  // THE HIGHLIGHTED ROW STAYS IN VIEW, by scrolling the result list and
  // nothing else. `scrollIntoView` scrolls every scrollable ancestor it finds,
  // which is why the router scrolls `#screen-scroll` directly too, and it does
  // not exist in the suite's DOM, so the palette threw the moment it opened
  // there. Re-run when the results change as well as the cursor: typing over a
  // list scrolled by the wheel leaves the cursor at 0 and the row out of view.
  useEffect(() => {
    const list = listRef.current;
    const row = list?.querySelector<HTMLElement>('[aria-selected="true"]');
    if (!list || !row) return;
    const box = list.getBoundingClientRect();
    const at = row.getBoundingClientRect();
    if (at.top < box.top) list.scrollTop -= box.top - at.top;
    else if (at.bottom > box.bottom) list.scrollTop += at.bottom - box.bottom;
  }, [listbox.active, hits]);

  const groups = useMemo(() => {
    const map = new Map<string, Hit[]>();
    for (const hit of hits) map.set(hit.group, [...(map.get(hit.group) ?? []), hit]);
    return [...map.entries()];
  }, [hits]);

  let flat = -1;
  return (
    <div
      className="veil"
      ref={modal.veilRef}
      role="presentation"
      onKeyDown={(e) => {
        // Handled, so the shell's window listener leaves the press alone
        // rather than opening search again on the same key.
        if (!isSearchShortcut(e)) return;
        e.preventDefault();
        onClose();
      }}
    >
      <ModalPanel className="palette" title="Search" panelRef={modal.panelRef}>
        <input
          className="palette-input"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            // A new query is a new list: the best match leads it.
            listbox.setActive(0);
          }}
          onKeyDown={listbox.onKeyDown}
          placeholder="Search screens, seats, units and tools, or paste an event, trace or turn id"
          role="combobox"
          aria-label="Search"
          aria-expanded={true}
          aria-controls={listbox.listId}
          aria-autocomplete="list"
          aria-activedescendant={listbox.active >= 0 ? listbox.optionId(listbox.active) : undefined}
          autoComplete="off"
          spellCheck={false}
        />
        {/* Never empty, so there is no "nothing matches" state: with no query
            every screen is listed, and any query offers the event log and the
            knowledge base as the last two results. */}
        <div
          className="palette-results"
          ref={listRef}
          id={listbox.listId}
          role="listbox"
          aria-label="Results"
        >
          {groups.map(([group, items], g) => (
            // By position, not by name: a group's name has spaces in it
            // ("Open by id"), and `aria-labelledby` splits on them.
            <div key={group} role="group" aria-labelledby={`${id}-group-${g}`}>
              <div className="palette-group" id={`${id}-group-${g}`} role="presentation">
                {group}
              </div>
              {items.map((hit) => {
                flat++;
                const mine = flat;
                return (
                  // NOT A BUTTON: focus never leaves the input, so a result is
                  // reached by the arrows and the pointer, and a tab stop per
                  // result is forty stops between the input and itself.
                  // Opened on CLICK rather than the shared list's mousedown:
                  // nothing here closes on blur, and search gone on the press
                  // would leave its release to land on the screen beneath.
                  // The press itself is prevented all the same, so it does
                  // not move focus to the dialog around the list: a combobox
                  // that loses its input to a press dragged off a row names
                  // no highlight, and typing reaches nothing.
                  <div
                    key={hit.id}
                    id={listbox.optionId(mine)}
                    className="palette-item"
                    role="option"
                    aria-selected={mine === listbox.active}
                    onMouseEnter={listbox.optionHandlers(mine).onMouseEnter}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => open(hit)}
                  >
                    <Icon name={hit.icon} size="sm" />
                    <span className="truncate">{hit.label}</span>
                    <span className="palette-hint truncate">{hit.hint}</span>
                  </div>
                );
              })}
            </div>
          ))}
        </div>
        <div className="palette-foot">
          <span>
            <Kbd keys={["ArrowUp"]} /> <Kbd keys={["ArrowDown"]} /> move
          </span>
          <span>
            <Kbd keys={["Enter"]} /> open
          </span>
          <span>
            <Kbd keys={["Escape"]} /> close
          </span>
        </div>
      </ModalPanel>
    </div>
  );
}
