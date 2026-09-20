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
import { createPortal } from "react-dom";
import { Input, Kbd, useBodyScrollLock, useLayerContainer, useModalLayer } from "@crewlethq/ui";
import { DESTINATIONS } from "./nav.ts";
import { useNavigator, useRoute, type Navigator, type Route } from "./router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useRecentsByVisit, forgetAll } from "~/lib/recents.ts";
import { DENSITIES, THEMES, useViewerPrefs, type ViewerPrefs } from "~/lib/prefs.ts";
import { requestToken } from "~/protocol/index.ts";
import { useAgents, useOrg, useTools } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
// PURE VALUES, no React and no DOM — so no cycle, and `Hit.icon` is already the
// `MarkName` `typeIcon` returns.
import { statusLabel, typeIcon, typeName } from "~/lib/work.ts";
import { QueryState } from "~/components/common.tsx";
import { markByName, type MarkName } from "~/ui/glyph.tsx";

interface Hit {
  id: string;
  group: string;
  icon: MarkName;
  label: string;
  hint: string;
  go: () => void;
}

/**
 * The palette's scopes, as the sigil that opens one.
 *
 * A LAUNCHER WITH ONE INDEX answers "where do I go", and three questions do
 * not fit that shape: searching the company's WORK is a server query and
 * cannot be ranked against a list of screens; finding a PERSON wants the
 * roster and nothing else, so a colleague is not buried under four tools
 * whose names happen to match; and running a COMMAND has no name to search
 * for at all.
 *
 * A SIGIL rather than a mode switch, because it is typed in the same box in
 * the same keystroke — and because the reader can see which scope they are
 * in from the character they just typed, which a mode indicator elsewhere on
 * the screen cannot claim.
 */
const SCOPES = [
  { sigil: "#", label: "work items", hint: "search the company's work" },
  { sigil: "@", label: "people", hint: "seats and units only" },
  { sigil: ">", label: "commands", hint: "theme, density, this page" },
] as const;

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

/**
 * The commands the `>` scope offers.
 *
 * WHAT HAS NO NAME TO SEARCH FOR. Every one of these is either a preference
 * with no page of its own, or an action on the page the reader is already on
 * — neither of which an index of screens and seats can hold, which is why the
 * scope exists rather than these being ranked beside a seat called "dark".
 *
 * Built per call rather than held as a constant, because each closes over the
 * current preference and the current route: a command that says "switch to
 * dark" while the page is already dark is a control that does not know what it
 * is looking at.
 */
function commands(prefs: ViewerPrefs, nav: Navigator, route: Route): Hit[] {
  const out: Hit[] = [];
  const add = (id: string, icon: MarkName, label: string, hint: string, go: () => void) =>
    out.push({ id: `cmd-${id}`, group: "Commands", icon, label, hint, go });

  for (const choice of THEMES) {
    if (choice === prefs.theme) continue;
    add(
      `theme-${choice}`,
      choice === "dark" ? "dark_mode" : choice === "light" ? "light_mode" : "computer",
      `Theme: ${choice}`,
      choice === "system" ? "follow this machine's setting" : `always ${choice}`,
      () => prefs.setTheme(choice),
    );
  }
  for (const choice of DENSITIES) {
    if (choice === prefs.density) continue;
    add(`density-${choice}`, "layers", `Density: ${choice}`, "how much air every list has", () =>
      prefs.setDensity(choice),
    );
  }
  add("copy-link", "link", "Copy link to this page", route.hash, () => {
    // THE WHOLE URL, not the hash: a link is pasted into a message and the
    // fragment alone resolves against whatever the reader has open.
    void navigator.clipboard?.writeText(window.location.href);
  });
  add("token", "key", "Set the API token", "for the operator-only screens", requestToken);
  add("clear-recents", "schedule", "Clear recents", "this browser only", forgetAll);
  return out;
}

export function CommandPalette({ onClose }: { onClose: () => void }) {
  const nav = useNavigator();
  const agents = useAgents();
  const org = useOrg();
  const tools = useTools();
  // VISIT ORDER HERE, ARRIVAL ORDER IN THE RAIL. The palette is opened fresh
  // and closes, so its re-sort is invisible and "where you just were" is what
  // an empty query should rank first; the sidebar's Recent section is drawn
  // and navigated by position, so it may not re-sort under the pointer. See
  // `lib/recents.ts`.
  const recents = useRecentsByVisit();
  const prefs = useViewerPrefs();
  const route = useRoute();
  const [q, setQ] = useState("");
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // THE LAYER, NOT A SHELL. uilet publishes the two halves of a modal
  // separately — [Modal] is the frame, [useModalLayer] is what a frame does —
  // and this surface wants only the second: Escape going to the TOPMOST layer
  // rather than to whichever handler bound `document` first, Tab trapped
  // inside the panel, focus moved in on open and handed back on close to the
  // row that opened it, one z-index band, and a press that closes on the
  // veil's own `click` rather than on its `pointerdown`.
  //
  // WHY NOT [Modal] ITSELF, which is what `ui/Dialog.tsx` ports onto. Its body
  // is one scrolling block, so the field would ride up out of view as the
  // arrows walked the list. uilet's own CommandPalette fixes that with a
  // stylesheet rule on `.crewlet-palette .crewlet-modal__body` — a band layout
  // a consumer cannot ask for through the component's props.
  const container = useLayerContainer();
  const layer = useModalLayer({ onClose, initialFocus: () => inputRef.current });
  useBodyScrollLock(true);

  const index = useMemo(() => indexOrg(org), [org]);

  // WHICH SCOPE, AND WHAT IS LEFT OF THE QUERY. Read off the first character
  // so the reader sees the scope they are in as the thing they typed.
  const sigil = SCOPES.find((s) => q.startsWith(s.sigil))?.sigil ?? "";
  const term = sigil ? q.slice(1).trim() : q.trim();

  // THE WORK SCOPE IS A SERVER QUERY, which is why it is a scope at all: a
  // ranked search over the company's items cannot be folded into an index of
  // screens and seats, and running it on every keystroke of an unscoped query
  // would put a search on the wire for somebody typing "settings".
  const asked = sigil === "#" && term.length >= 2;
  const work = useQuery("work_search", { q: term, limit: 8 }, { enabled: asked });

  // WHICH TERM THE ANSWER ON HAND IS FOR.
  //
  // `useQuery` keeps the previous answer across a change of parameters and
  // across a failure, which is right for a polled table — replacing a rendered
  // table with a skeleton every tick is how a polled screen becomes unreadable
  // — and wrong for a search box, where the question changes on every
  // keystroke. Without this the palette listed `auth`'s items as hits for
  // `authz` with nothing saying a query was in flight, and a refusal raised by
  // one term was read as a refusal of the next.
  //
  // Recorded against the ANSWER'S OWN IDENTITY rather than against a timer:
  // `socket.query` builds a fresh object per answer and `useQuery` drops the
  // answer of a superseded generation, so a change of identity is this term's
  // answer arriving and nothing else. The answer itself carries no echo of the
  // query, which is why the term has to be remembered here.
  const settled = useRef<{ data: unknown; error: string | null; term: string }>({
    data: null,
    error: null,
    term: "",
  });
  if (settled.current.data !== work.data || settled.current.error !== work.error) {
    settled.current = { data: work.data, error: work.error, term };
  }
  const answers = asked && settled.current.term === term;
  // THE THREE STATES OF A SERVER QUERY, kept apart. A read that failed is not
  // a company with no such work — `internal/api/queries` splits them itself
  // (an unregistered `work_search` is `unknown_query`, a building index is
  // `available:false`, a store fault is an error) and a palette that collapses
  // them tells the reader to file a duplicate for work that already exists.
  const refusal = answers ? work.error : null;
  const searching = asked && !answers;

  const hits = useMemo<Hit[]>(() => {
    const query = term.toLowerCase();
    const out: { hit: Hit; rank: number }[] = [];
    const push = (hit: Hit, rank: number) => out.push({ hit, rank });

    // RECENTS WHEN THERE IS NOTHING TO SEARCH. An empty palette is a blank box
    // asking a question the reader answered by opening it: they want to go
    // somewhere, and the likeliest somewhere is where they just were.
    if (!sigil && query === "") {
      recents.forEach((r, i) =>
        push(
          {
            id: `recent-${r.path.join("/")}`,
            group: "Recent",
            icon: "schedule",
            label: r.label,
            hint: r.workspace || r.path.join(" / "),
            go: () => nav.to(r.path),
          },
          -100 + i,
        ),
      );
    }

    if (sigil === "#") {
      // Only the answer to THIS term is a hit for it; a refusal has none.
      for (const [i, item] of (answers && !refusal ? (work.data?.hits ?? []) : []).entries()) {
        push(
          {
            id: `work-${item.key}`,
            group: "Work",
            // THE ITEM'S OWN TYPE, from the one table that says which drawing a
            // type wears. A hardcoded tick made a bug, an epic and a milestone
            // one mark in a list that also holds the DESTINATION rows from
            // `nav.ts` — where a tick legitimately names the Work workspace.
            // Eight rows, one glyph, and no way to tell an item from a screen.
            icon: typeIcon(item.type),
            label: `${item.key} — ${item.title}`,
            // AND THE WORD, because `HitGlyph` draws every hit's mark with no
            // `title`: that is right where the LABEL is the word, which it is
            // for a destination and a seat, and wrong here — the label is a key
            // and a title, so the type appears nowhere else on the row. The
            // status goes through `statusLabel` for the reason that function
            // exists: this line printed the raw slug, so the palette said
            // `in_progress` where every other surface says "In progress". No
            // project defs, for the same reason the search screen gives — the
            // answer spans projects.
            hint: [
              typeName(item.type),
              statusLabel(item.status),
              item.assignee && `@${item.assignee}`,
            ]
              .filter(Boolean)
              .join(" · "),
            go: () => nav.to(["work", item.key]),
          },
          i,
        );
      }
      // "NOTHING MATCHED" IS AN ANSWER, so it is claimed only when this term
      // has one: not while a query is in flight, and never over a refusal —
      // the banner beside the list says what went wrong instead.
      if (answers && !refusal && (work.data?.hits ?? []).length === 0) {
        push(
          {
            id: "work-none",
            group: "Work",
            icon: "search",
            label: `Nothing matches “${term}”`,
            // THE INDEX IS ITS OWN STATE. A search that answers nothing
            // because the index is still building is not a company with no
            // such work, and the reader has to be able to tell them apart.
            hint:
              work.data?.available === false
                ? "the search index is still building"
                : "open the full search",
            go: () => nav.to(["work", "search"], { q: term }),
          },
          500,
        );
      }
      return out.sort((a, b) => a.rank - b.rank).map((r) => r.hit);
    }

    if (sigil === ">") {
      for (const command of commands(prefs, nav, route)) {
        const s = query ? score(command.label, query) : 0;
        if (s < 0) continue;
        push(command, s);
      }
      return out
        .sort((a, b) => a.rank - b.rank)
        .slice(0, 40)
        .map((r) => r.hit);
    }

    // A pasted id is a destination, not a search term — offer it first and
    // exactly, rather than making the reader guess which screen takes it.
    if (!sigil && (UUIDISH.test(query) || HEXISH.test(query))) {
      push(
        {
          id: `event-${query}`,
          group: "Open by id",
          icon: "description",
          label: query,
          hint: "as an event",
          go: () => nav.to(["activity", "events", query]),
        },
        -3,
      );
      push(
        {
          id: `trace-${query}`,
          group: "Open by id",
          icon: "fork_right",
          label: query,
          hint: "as a trace — every event that carries it",
          // THE TRACE PAGE, which is the only screen that assembles one: it
          // reads every event carrying the id and draws the span tree they
          // form. This pointed at the event log with `?trace=` instead, a
          // parameter `routes/activity/Activity.tsx` reads nowhere — so the
          // reader landed on the UNFILTERED log, which reads as a trace that
          // touched everything.
          go: () => nav.to(["activity", "traces", query]),
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
          go: () => nav.to(["activity", "turns", query]),
        },
        -1,
      );
    }

    for (const item of sigil === "@" ? [] : DESTINATIONS) {
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
          icon: seat.kind === "human" ? "person" : "group",
          label: seat.name,
          hint:
            seat.kind === "human"
              ? "human teammate"
              : `@${seat.handle}${live?.state ? ` · ${live.state}` : ""}`,
          go: () => nav.to(["company", "people", seat.handle]),
        },
        s + 1,
      );
    }

    for (const unit of index.units) {
      // AN EMPTY QUERY LISTS EVERY UNIT, exactly as it lists every seat and
      // every destination. `Infinity` is the seat loop's per-field no-match
      // sentinel, which `Math.min` folds there and nothing folds here: copied
      // in, it made the finiteness guard drop every unit whenever the box was
      // empty, so typing `@` showed the whole roster and no units at all under
      // a footer promising "seats and units only".
      const s = query ? score(unit.name, query) : 0;
      if (s < 0) continue;
      push(
        {
          id: `unit-${unit.name}`,
          group: "Units",
          icon: "account_tree",
          label: unit.name,
          hint: `${unit.type || "unit"}${unit.lead ? ` · lead ${unit.lead}` : ""}`,
          // BY NAME. A unit's stable `id:` is part of the guarded
          // configuration rather than the anonymous org projection, so no
          // link this palette can build carries one.
          go: () => nav.to(["company", "units", unit.name]),
        },
        s + 2,
      );
    }

    if (query && !sigil) {
      for (const tool of tools) {
        const s = score(tool.name, query);
        if (s < 0) continue;
        push(
          {
            id: `tool-${tool.name}`,
            group: "Tools",
            icon: "build",
            label: tool.name,
            hint: tool.source,
            go: () => nav.to(["admin", "tools"], { q: tool.name }),
          },
          s + 3,
        );
      }
      push(
        {
          id: "search-events",
          group: "Search",
          icon: "timeline",
          label: `Events mentioning “${q.trim()}”`,
          hint: "the event log, filtered",
          go: () => nav.to(["activity", "events"], { q: q.trim() }),
        },
        900,
      );
      push(
        {
          id: "search-knowledge",
          group: "Search",
          icon: "book_2",
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
  }, [term, sigil, index, agents, tools, nav, recents, work.data, answers, refusal, prefs, route]);

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
    }
    // ESCAPE IS THE LAYER STACK'S, and it goes to the TOPMOST surface rather
    // than to whichever handler bound `document` first — so it closes from
    // the result list and the footer too, and a dialog raised over this one
    // closes itself and leaves the palette standing.
  }

  let flat = -1;
  if (!container) return null;
  return createPortal(
    <div className="veil" ref={layer.veilRef} style={{ zIndex: layer.zIndex }}>
      <div
        className="palette"
        ref={layer.panelRef}
        role="dialog"
        aria-modal
        aria-label="Search"
        tabIndex={-1}
      >
        {/* THE FIELD IS THE SURFACE, which is exactly what uilet's `command`
            appearance is for: no box of its own, set at the reading size, and
            a placeholder on an ink measured to 4.5:1 rather than on the
            decoration step a hint is usually drawn in. The band geometry
            around it stays `.palette-input`, which is ours. */}
        <Input
          ref={inputRef}
          appearance="command"
          containerClassName="palette-input"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Search — or # work, @ people, > commands"
          aria-label="Search"
          autoComplete="off"
          spellCheck={false}
        />
        <div className="palette-results" ref={listRef} role="listbox">
          {/* A SCOPE THAT IS WAITING IS NOT A SCOPE THAT FOUND NOTHING, and
              the work scope is the only one that can be either: it is a
              server query, so "no hits yet" covers FOUR states — a term too
              short to run one, a query in flight, a read the engine refused
              or could not complete, and a company with no such work. Each
              sends the reader somewhere different, so each says so. */}
          {!hits.length && sigil === "#" && term.length < 2 && (
            <div className="palette-item" style={{ color: "var(--text-muted)" }}>
              Type at least two characters to search the company&rsquo;s work.
            </div>
          )}
          {!hits.length && searching && (
            <div className="palette-item" style={{ color: "var(--text-muted)" }}>
              Searching…
            </div>
          )}
          {/* THE REFUSAL ITSELF, through the one table that says what each
              code means — `components/common.tsx`. A second wording here is
              how "the engine does not serve this answer" and "the socket went
              away" ended up both reading as "nothing matched". */}
          {refusal && <QueryState error={refusal} loading={false} />}
          {!hits.length && !(sigil === "#" && (term.length < 2 || searching || !!refusal)) && (
            <div className="palette-item" style={{ color: "var(--text-muted)" }}>
              Nothing matches “{term || q}”.
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
                    <HitGlyph name={hit.icon} />
                    <span className="truncate">{hit.label}</span>
                    <span className="palette-hint truncate">{hit.hint}</span>
                  </button>
                );
              })}
            </div>
          ))}
        </div>
        <div className="palette-foot">
          {/* THE SCOPES ARE IN THE FOOTER, because a sigil nobody is told
              about is a feature that does not exist. The one in use is
              marked, so the reader can see which scope they typed into. */}
          {SCOPES.map((s) => (
            <span key={s.sigil} className={sigil === s.sigil ? "is-on" : undefined}>
              <Kbd>{s.sigil}</Kbd> {s.label}
            </span>
          ))}
          <span className="spacer" />
          <span>
            <Kbd>↑</Kbd> <Kbd>↓</Kbd> move
          </span>
          <span>
            <Kbd>↵</Kbd> open
          </span>
          <span>
            <Kbd>esc</Kbd> close
          </span>
        </div>
      </div>
    </div>,
    container,
  );
}

/**
 * One hit's glyph, looked up from the name the hit carries.
 *
 * BY NAME, because a hit's icon IS data here: it comes from `nav.ts`'s
 * destination table, from a seat's kind, or from which of three ways a pasted
 * id could be read. `~/ui/glyph.tsx` is the one lookup over a name, and the
 * registry it wraps is published for exactly this case and says so; every
 * other call site in this tree imports the one glyph it draws and never
 * reaches a lookup at all.
 */
function HitGlyph({ name }: { name: MarkName }) {
  const Glyph = markByName(name);
  return <Glyph size="sm" />;
}
