/**
 * One search for the whole product.
 *
 * Per-screen search boxes would mean one ranking rule per screen and as many
 * places to keep them agreeing; this reaches screens, seats, units, tools, and
 * any event / trace / turn id pasted out of a log — which is the actual way an
 * operator arrives at a detail page.
 *
 * It is a LAUNCHER, not a settings panel: it closes on every action, including
 * the ones that only change a filter.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { ALL_NAV } from "./nav.ts";
import { useNavigator } from "./router.tsx";
import { useAgents, useOrg, useTools } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { Icon, type IconName } from "~/ui/Icon.tsx";

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

function score(text: string, q: string): number {
  const t = text.toLowerCase();
  const i = t.indexOf(q);
  if (i < 0) return -1;
  // A prefix match beats a match in the middle, and a short field beats a long
  // one — so typing "pm" finds the seat called PM rather than every seat whose
  // backstory mentions a PM.
  return (i === 0 ? 0 : 100 + i) + t.length / 100;
}

export function CommandPalette({ onClose }: { onClose: () => void }) {
  const nav = useNavigator();
  const agents = useAgents();
  const org = useOrg();
  const tools = useTools();
  const [q, setQ] = useState("");
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const index = useMemo(() => indexOrg(org), [org]);

  const hits = useMemo<Hit[]>(() => {
    const query = q.trim().toLowerCase();
    const out: { hit: Hit; rank: number }[] = [];
    const push = (hit: Hit, rank: number) => out.push({ hit, rank });

    // A pasted id is a destination, not a search term — offer it first and
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
          id: `seat-${seat.handle}`,
          group: "Seats",
          icon: seat.kind === "human" ? "user" : "users",
          label: seat.name,
          hint:
            seat.kind === "human"
              ? "human teammate"
              : `@${seat.handle}${live?.state ? ` · ${live.state}` : ""}`,
          go: () => nav.to(["seats", seat.handle]),
        },
        s + 1,
      );
    }

    for (const unit of index.units) {
      const s = query ? score(unit.name, query) : Infinity;
      if (s < 0 || !Number.isFinite(s)) continue;
      push(
        {
          id: `unit-${unit.name}`,
          group: "Units",
          icon: "sitemap",
          label: unit.name,
          hint: `${unit.type ?? "unit"}${unit.lead ? ` · lead ${unit.lead}` : ""}`,
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

  useEffect(() => setCursor(0), [q]);
  useEffect(() => {
    listRef.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: "nearest" });
  }, [cursor]);

  const groups = useMemo(() => {
    const map = new Map<string, Hit[]>();
    for (const hit of hits) map.set(hit.group, [...(map.get(hit.group) ?? []), hit]);
    return [...map.entries()];
  }, [hits]);

  function onKeyDown(e: React.KeyboardEvent): void {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor((c) => Math.min(hits.length - 1, c + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor((c) => Math.max(0, c - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      hits[cursor]?.go();
      onClose();
    } else if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    }
  }

  let flat = -1;
  return (
    <div className="veil" onMouseDown={onClose} role="presentation">
      <div
        className="palette"
        role="dialog"
        aria-modal="true"
        aria-label="Search"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <input
          ref={inputRef}
          className="palette-input"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search screens, seats, units, tools — or paste an event, trace or turn id"
          aria-label="Search"
          autoComplete="off"
          spellCheck={false}
        />
        <div className="palette-results" ref={listRef} role="listbox">
          {!hits.length && (
            <div className="palette-item" style={{ color: "var(--text-muted)" }}>
              Nothing matches “{q}”.
            </div>
          )}
          {groups.map(([group, items]) => (
            <div key={group}>
              <div className="palette-group">{group}</div>
              {items.map((hit) => {
                flat++;
                const mine = flat;
                return (
                  <button
                    key={hit.id}
                    className="palette-item"
                    role="option"
                    aria-selected={mine === cursor}
                    onMouseEnter={() => setCursor(mine)}
                    onClick={() => {
                      hit.go();
                      onClose();
                    }}
                  >
                    <Icon name={hit.icon} size="sm" />
                    <span className="truncate">{hit.label}</span>
                    <span className="palette-hint truncate">{hit.hint}</span>
                  </button>
                );
              })}
            </div>
          ))}
        </div>
        <div className="palette-foot">
          <span>
            <kbd>↑</kbd> <kbd>↓</kbd> move
          </span>
          <span>
            <kbd>↵</kbd> open
          </span>
          <span>
            <kbd>esc</kbd> close
          </span>
        </div>
      </div>
    </div>
  );
}
