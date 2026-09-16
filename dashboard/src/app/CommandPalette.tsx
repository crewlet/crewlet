/**
 * One search for the whole product.
 *
 * Per-screen search boxes would mean one ranking rule per screen and as many
 * places to keep them agreeing; this reaches screens, seats, units, tools, and
 * any event / trace / turn id pasted out of a log, which is the actual way an
 * operator arrives at a detail page.
 *
 * It is a LAUNCHER, not a settings panel: it closes on every action, including
 * the ones that only change a filter. The surface does that closing itself,
 * so a result only has to say where it goes.
 *
 * The SURFACE is the design system's: a modal on the layer stack, so Escape,
 * the veil, the Tab trap and the return of focus to whatever opened it behave
 * exactly as they do for every dialog and drawer, and a combobox whose input
 * keeps focus while the arrows move a highlight the input names with
 * `aria-activedescendant`, so a screen reader hears each result as it is
 * reached and nothing walks through forty tab stops to get back to the box.
 *
 * WHAT THIS FILE OWNS IS THE SEARCH: what is offered, and in what order.
 *
 * THE SHORTCUT THAT OPENED IT CLOSES IT, but only from inside it. The shell
 * listens on the window and only ever opens search, and only while no modal
 * is open; a chord pressed while another surface raised over search holds the
 * keyboard (a token dialog a refused request opened) belongs to that surface,
 * and closing search from beneath it would change a page the reader cannot
 * see.
 */

import { type ComponentType, useMemo, useState } from "react";
import { ALL_NAV } from "./nav.ts";
import { useNavigator } from "./router.tsx";
import { useAgents, useOrg, useTools } from "~/lib/store-hooks.ts";
import { indexOrg, seatPath } from "~/lib/seats.ts";
import {
  type GlyphProps,
  AccountTreeGlyph,
  Book2Glyph,
  BuildGlyph,
  DescriptionGlyph,
  ForkRightGlyph,
  LayersGlyph,
  PersonGlyph,
  SmartToyGlyph,
  TimelineGlyph,
} from "@crewlethq/icons/glyphs";
import { CommandPalette as Palette, Kbd, type CommandPaletteGroup } from "@crewlethq/ui";

interface Hit {
  id: string;
  group: string;
  icon: ComponentType<GlyphProps>;
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
          icon: DescriptionGlyph,
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
          icon: ForkRightGlyph,
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
          icon: LayersGlyph,
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
          icon: seat.kind === "human" ? PersonGlyph : SmartToyGlyph,
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
          icon: AccountTreeGlyph,
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
            icon: BuildGlyph,
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
          icon: TimelineGlyph,
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
          icon: Book2Glyph,
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

  /*
   * The groups, in the order the ranking put them. A group's items keep that
   * order too, which is what makes the first row the best match rather than
   * whichever group happened to be built first.
   */
  const groups = useMemo<CommandPaletteGroup[]>(() => {
    const byGroup = new Map<string, CommandPaletteGroup>();
    for (const hit of hits) {
      let group = byGroup.get(hit.group);
      if (!group) {
        group = { id: hit.group, label: hit.group, items: [] };
        byGroup.set(hit.group, group);
      }
      group.items.push({
        id: hit.id,
        icon: <hit.icon size="sm" />,
        label: hit.label,
        hint: hit.hint,
        // Going somewhere is all this has to do: the surface closes itself
        // after a selection, and closing it here as well raised the shell's
        // close twice for one press.
        onSelect: hit.go,
      });
    }
    return [...byGroup.values()];
  }, [hits]);

  return (
    <Palette
      open
      label="Search"
      query={q}
      onQueryChange={setQ}
      groups={groups}
      resultsLabel="Results"
      placeholder="Search screens, seats, units and tools, or paste an event, trace or turn id"
      // Never empty: with no query every screen is listed, and any query
      // offers the event log and the knowledge base as the last two results.
      emptyMessage="Nothing matches, which on this surface means the query is not an id and matches no screen, seat, unit or tool."
      onClose={onClose}
      onKeyDown={(e) => {
        // Handled, so the shell's window listener leaves the press alone
        // rather than opening search again on the same key.
        if (!isSearchShortcut(e)) return;
        e.preventDefault();
        onClose();
      }}
      footer={
        <>
          <span>
            <Kbd keys={["ArrowUp"]} /> <Kbd keys={["ArrowDown"]} /> move
          </span>
          <span>
            <Kbd keys={["Enter"]} /> open
          </span>
          <span>
            <Kbd keys={["Escape"]} /> close
          </span>
        </>
      }
    />
  );
}
