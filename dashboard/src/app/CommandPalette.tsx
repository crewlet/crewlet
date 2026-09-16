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
import { useModal } from "~/ui/Dialog.tsx";
import { DESTINATIONS } from "./nav.ts";
import { useNavigator, useRoute, type Navigator, type Route } from "./router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useRecents, forgetAll } from "~/lib/recents.ts";
import { DENSITIES, THEMES, useViewerPrefs, type ViewerPrefs } from "~/lib/prefs.ts";
import { requestToken } from "~/protocol/index.ts";
import { useAgents, useOrg, useTools } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { QueryState } from "~/components/common.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";

interface Hit {
  id: string;
  group: string;
  icon: IconName;
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
  const add = (id: string, icon: IconName, label: string, hint: string, go: () => void) =>
    out.push({ id: `cmd-${id}`, group: "Commands", icon, label, hint, go });

  for (const choice of THEMES) {
    if (choice === prefs.theme) continue;
    add(
      `theme-${choice}`,
      choice === "dark" ? "moon" : choice === "light" ? "sun" : "monitor",
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
  add("clear-recents", "clock", "Clear recents", "this browser only", forgetAll);
  return out;
}

export function CommandPalette({ onClose }: { onClose: () => void }) {
  const nav = useNavigator();
  const agents = useAgents();
  const org = useOrg();
  const tools = useTools();
  const recents = useRecents();
  const prefs = useViewerPrefs();
  const route = useRoute();
  const [q, setQ] = useState("");
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // THE SHELL EVERY OTHER MODAL USES. This one hand-rolled its veil because
  // `Dialog`'s chrome is a title bar and the palette's header IS its input —
  // and so it had none of the behaviour: focus was moved in but never
  // returned, so closing left a keyboard reader at the top of the page rather
  // than on the row they opened it from, and Escape was bound to the input
  // alone, which does nothing once focus is in the result list beside it.
  const { veil, shell } = useModal({ label: "Search", onClose });

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
            icon: "clock",
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
            icon: "check",
            label: `${item.key} — ${item.title}`,
            hint: [item.status, item.assignee && `@${item.assignee}`].filter(Boolean).join(" · "),
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
          icon: "file",
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
          icon: "gitBranch",
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
          icon: seat.kind === "human" ? "user" : "users",
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
          icon: "sitemap",
          label: unit.name,
          hint: `${unit.type ?? "unit"}${unit.lead ? ` · lead ${unit.lead}` : ""}`,
          go: () => nav.to(["company", "units", unit.id || unit.name]),
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
            icon: "wrench",
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
          icon: "activity",
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
    // ESCAPE IS THE SHELL'S, on `document`, so it closes from the result list
    // and from the footer too.
  }

  let flat = -1;
  return (
    <div {...veil}>
      <div {...shell} className="palette">
        <input
          ref={inputRef}
          className="palette-input"
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
          {/* THE SCOPES ARE IN THE FOOTER, because a sigil nobody is told
              about is a feature that does not exist. The one in use is
              marked, so the reader can see which scope they typed into. */}
          {SCOPES.map((s) => (
            <span key={s.sigil} className={sigil === s.sigil ? "is-on" : undefined}>
              <kbd>{s.sigil}</kbd> {s.label}
            </span>
          ))}
          <span className="spacer" />
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
