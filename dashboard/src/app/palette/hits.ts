/**
 * What the palette offers from what this browser already holds — the pure
 * half, over values, so every rule in it is exercised without a socket.
 *
 * THE SERVER'S ANSWERS ARE NOT HERE. Tasks, pages and the colleague tiers are
 * questions (`work_search`, `knowledge`, `colleague`) and are turned into rows
 * where they are asked, in `Palette.tsx`; what is here is the index of
 * screens, seats, units and tools the frame already has, the ids a person
 * pastes out of a log, the per-browser commands, and the rules that decide
 * which scope a keystroke is in and when a question is worth asking.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import { DESTINATIONS } from "../nav.ts";
import type { Navigator } from "../router.tsx";
import { resolve } from "../routes.ts";
import type { Recent } from "~/lib/recents.ts";
import { DATE_FORMATS, DENSITIES, THEMES, type DateFormat, type ViewerPrefs } from "~/lib/prefs.ts";
import type { OrgIndex, Seat, Unit } from "~/lib/seats.ts";
import { COLLEAGUE_QUERY_MAX } from "~/contract/wire.ts";
import { utf8Bytes } from "~/lib/format.ts";

/**
 * The scopes, in the order the tab row draws them and Tab walks them.
 *
 * FIVE, because five questions do not fit one ranked list: the company's WORK
 * and its PAGES are server searches ranked by the engine and cannot be ranked
 * against a list of screens; finding a PERSON wants the roster and the
 * engine's own name tiers and nothing else, so a colleague is not buried
 * under four tools whose names happen to match; and running a COMMAND has no
 * name to search for at all. ALL is the launcher over the lot.
 *
 * A SIGIL still opens three of them, typed in the same box in the same
 * keystroke as the query: `#` tasks, `@` agents, `>` actions. The sigil
 * selects the tab and leaves the box, so the tab row — not a character the
 * reader has to remember is special — says which scope they are in.
 */
export const SCOPES = [
  { id: "all", label: "All" },
  { id: "tasks", label: "Tasks", sigil: "#" },
  { id: "pages", label: "Pages" },
  { id: "agents", label: "Agents", sigil: "@" },
  { id: "actions", label: "Actions", sigil: ">" },
] as const;

export type ScopeId = (typeof SCOPES)[number]["id"];

/** The scope a typed value opens, and what is left of it once the sigil is read. */
export function readSigil(value: string): { scope: ScopeId | null; rest: string } {
  for (const scope of SCOPES) {
    if ("sigil" in scope && value.startsWith(scope.sigil)) {
      return { scope: scope.id, rest: value.slice(scope.sigil.length) };
    }
  }
  return { scope: null, rest: value };
}

/**
 * The shortest term a server search is asked for.
 *
 * TWO CHARACTERS: one ranks half the company's work against a letter, and a
 * search per first keystroke is a search nobody reads. Two is the shortest
 * key fragment worth ranking ("db", "ci").
 */
export const SEARCH_MIN = 2;

/**
 * How many task hits the Tasks scope lists, and how many All shows of them.
 *
 * EIGHT in the scope: a ranked answer is opened, not scrolled — the person's
 * next move is one or two of the top rows, and past eight the full search
 * screen (`#/work/search`, one row away) is where the rest belong. THREE in
 * All, where tasks share the list with pages, agents and actions and the
 * approved palette draws two or three of each.
 */
export const TASK_HITS = 8;
export const ALL_ROWS = 3;

/**
 * How many task and page hits All shows UNDER AN ANSWER — two of each.
 *
 * THE ACTIONS STAY IN THE FIRST SCREEN. An answer card with a three-line
 * paragraph and its source chips stands about 160px; under it three tasks and
 * three pages put the three actions — the rows a question is typed to reach —
 * below the fold of a 900px window (the kit's frame there is 666px tall). Two
 * of each is what the approved palette draws under its answer, and the answer
 * itself is written from those same top hits, so the third of each is the row
 * the paragraph has already read out. Taken from the moment the term reads
 * as a question, not when the answer lands, so the list does not shrink
 * under the highlight 800 ms after the person stopped typing.
 */
export const ALL_ROWS_UNDER_ANSWER = 2;

/**
 * How many of an answer's sources are drawn as chips, on ONE line; the rest
 * are "+N", and every source — these four included — is a row under
 * "Sources of the answer", which is how a keyboard reaches any of them. Four
 * chips of a title each fill the 720px card's line; a second line of chips
 * is what pushed the actions off the first screen.
 */
export const SOURCE_CHIPS = 4;

/**
 * How many projects the palette reads to say where a create lands and to
 * offer when it cannot tell: the engine's own cap on the listing (200), so
 * no company's project is missing from the step that picks one.
 */
export const PROJECT_PAGE = 200;

/**
 * The longest text the engine resolves to a colleague, in UTF-8 bytes:
 * `queries.ColleagueQueryMax`, held by the contract (`contract/wire.ts`). A
 * pasted log line is refused `bad_params` there, and that refusal would only
 * drop the engine's name tiers from the Agents list with nothing saying why
 * — so a term past it is not sent, and the chart's own matching answers it.
 */
export function colleagueSendable(term: string): boolean {
  return term !== "" && utf8Bytes(term) <= COLLEAGUE_QUERY_MAX;
}

/** How many rows one group may put in All before it gives way to the next. */
export const GO_TO_ROWS = 5;

/** Is this the shape of an id somebody pasted out of a log? */
const UUIDISH = /^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$/i;
const HEXISH = /^[0-9a-f]{16,64}$/i;

export function pastedId(term: string): boolean {
  return UUIDISH.test(term) || HEXISH.test(term);
}

/**
 * How well a field matches a query, lower is better, or -1 for no match.
 *
 * A prefix beats a match in the middle, and a short field beats a long one —
 * so typing "pm" finds the seat called PM rather than every seat whose goal
 * mentions a PM.
 */
export function score(text: string, query: string): number {
  const t = text.toLowerCase();
  const i = t.indexOf(query);
  if (i < 0) return -1;
  return (i === 0 ? 0 : 100 + i) + t.length / 100;
}

/** One row the palette offers, before it is drawn. */
export interface Hit {
  id: string;
  icon: GlyphName;
  label: string;
  hint: string;
  go: () => void;
}

/** The ranked matches of a list, best first, capped. */
function ranked<T>(
  items: readonly T[],
  query: string,
  fields: (item: T) => string[],
  cap: number,
): T[] {
  const scored: { item: T; s: number }[] = [];
  for (const item of items) {
    const s = query
      ? Math.min(
          ...fields(item).map((f) => {
            const v = score(f, query);
            return v < 0 ? Infinity : v;
          }),
        )
      : 0;
    if (Number.isFinite(s)) scored.push({ item, s });
  }
  return scored
    .sort((a, b) => a.s - b.s)
    .slice(0, cap)
    .map((x) => x.item);
}

/**
 * Where the reader just was, most recent first — what an empty palette should
 * offer, because the likeliest place to go is the one they just left.
 */
export function recentHits(recents: readonly Recent[], nav: Navigator): Hit[] {
  return recents.map((r) => ({
    id: `recent-${r.path.join("/")}`,
    icon: "clock",
    label: r.label,
    hint: r.workspace || r.path.join(" / "),
    go: () => nav.to(r.path),
  }));
}

/** The screens, matched by name. */
export function destinationHits(query: string, nav: Navigator, cap: number): Hit[] {
  return ranked(DESTINATIONS, query, (d) => [d.label], cap).map((d) => ({
    id: `nav-${d.key}`,
    icon: d.icon,
    label: d.label,
    hint: d.guarded ? `${d.hint} · needs a token` : d.hint,
    go: () => nav.to(d.path),
  }));
}

/**
 * A pasted id is a destination, not a search term — offered first and
 * exactly, as each of the three things it could be, rather than making the
 * reader guess which screen takes it.
 */
export function idHits(term: string, nav: Navigator): Hit[] {
  if (!pastedId(term)) return [];
  const id = term.toLowerCase();
  return [
    {
      id: `event-${id}`,
      icon: "file-text",
      label: id,
      hint: "as an event",
      go: () => nav.to(["live", "events", id]),
    },
    {
      id: `trace-${id}`,
      icon: "split",
      label: id,
      hint: "as a trace — every event that carries it",
      // THE TRACE PAGE, which is the only screen that assembles one. This
      // pointed at the event log with `?trace=` once, a parameter that screen
      // did not read, and the reader landed on the whole log.
      go: () => nav.to(["live", "traces", id]),
    },
    {
      id: `turn-${id}`,
      icon: "layers",
      label: id,
      hint: "as a turn",
      go: () => nav.to(["live", "turns", id]),
    },
  ];
}

/** The seats matching a query, in the local index's order of fit. */
export function matchingSeats(index: OrgIndex, query: string, cap: number): Seat[] {
  return ranked(
    index.seats.filter((s) => s.handle !== ""),
    query,
    (s) => [s.name, s.handle, s.goal],
    cap,
  );
}

/**
 * The units matching a query — EVERY unit on an empty one, exactly as every
 * seat is listed, because a scope that promises teams and draws none until
 * something is typed is a scope that lies about what it holds.
 */
export function matchingUnits(index: OrgIndex, query: string, cap: number): Unit[] {
  return ranked(index.units, query, (u) => [u.name], cap);
}

/** The tools matching a query; nothing on an empty one, since a tool is found by name. */
export function toolHits(
  tools: readonly { name: string; source: string }[],
  query: string,
  nav: Navigator,
  cap: number,
): Hit[] {
  if (!query) return [];
  return ranked(tools, query, (t) => [t.name], cap).map((tool) => ({
    id: `tool-${tool.name}`,
    icon: "wrench",
    label: tool.name,
    hint: tool.source,
    go: () => nav.to(["settings", "tools"], { q: tool.name }),
  }));
}

/** The searches a term can be handed to whole, as the screens that run them. */
export function searchHits(term: string, nav: Navigator): Hit[] {
  if (!term) return [];
  return [
    {
      id: "search-events",
      icon: "chart-no-axes-gantt",
      label: `Events mentioning “${term}”`,
      hint: "the event log, filtered",
      go: () => nav.to(["live", "events"], { q: term }),
    },
    {
      id: "search-knowledge",
      icon: "book-open",
      label: `Knowledge search for “${term}”`,
      hint: "every mode, every container",
      go: () => nav.to(["knowledge"], { q: term }),
    },
  ];
}

const DATE_WORDS: Record<DateFormat, string> = {
  auto: "this browser's own",
  iso: "2026-09-22",
  long: "September 22, 2026",
};

/**
 * The per-browser commands.
 *
 * WHAT HAS NO NAME TO SEARCH FOR: a preference with no page of its own, or an
 * action on the page the reader is already on. Built per call, because each
 * closes over the current preference and route: a command that says "switch
 * to dark" while the page is already dark is a control that does not know
 * what it is looking at, so the choice in effect is never offered.
 */
export function commandHits(
  prefs: ViewerPrefs,
  query: string,
  actions: {
    copyLink: () => void;
    setToken: () => void;
    clearRecents: () => void;
    showKeys: () => void;
    hash: string;
  },
): Hit[] {
  const out: Hit[] = [];
  for (const choice of THEMES) {
    if (choice === prefs.theme) continue;
    out.push({
      id: `cmd-theme-${choice}`,
      icon: choice === "dark" ? "moon" : choice === "light" ? "sun" : "monitor",
      label: `Theme: ${choice === "system" ? "match system" : choice}`,
      hint: choice === "system" ? "follow this machine's setting" : `always ${choice}`,
      go: () => prefs.setTheme(choice),
    });
  }
  for (const choice of DENSITIES) {
    if (choice === prefs.density) continue;
    out.push({
      id: `cmd-density-${choice}`,
      icon: "layers",
      label: `Density: ${choice}`,
      hint: "how much air every list has",
      go: () => prefs.setDensity(choice),
    });
  }
  for (const choice of DATE_FORMATS) {
    if (choice === prefs.dateFormat) continue;
    out.push({
      id: `cmd-dates-${choice}`,
      icon: "calendar",
      label: `Dates: ${DATE_WORDS[choice]}`,
      hint: "how every date is written, in this browser",
      go: () => prefs.setDateFormat(choice),
    });
  }
  // THE ZONE HAS FOUR HUNDRED VALUES, so the palette offers only the one
  // choice that is a command rather than a search: going back to following
  // the browser. Picking a zone is the preferences panel's searchable list.
  if (prefs.timezoneChosen) {
    out.push({
      id: "cmd-zone-browser",
      icon: "globe",
      label: "Time zone: this browser's",
      hint: `instead of ${prefs.timezone.replace(/_/g, " ")}`,
      go: () => prefs.setTimezone(""),
    });
  }
  out.push(
    {
      id: "cmd-copy-link",
      icon: "link",
      label: "Copy link to this page",
      hint: actions.hash,
      go: actions.copyLink,
    },
    {
      id: "cmd-token",
      icon: "key",
      label: "Set the API token",
      hint: "who this browser acts as",
      go: actions.setToken,
    },
    {
      id: "cmd-clear-recents",
      icon: "clock",
      label: "Clear recents",
      hint: "this browser only",
      go: actions.clearRecents,
    },
    // THE LEGEND HAS A ROW, because `?` is a key and the reader looking for
    // the keys is the reader who does not know it yet.
    {
      id: "cmd-keys",
      icon: "command",
      label: "Keyboard shortcuts",
      hint: "every key, and where it works",
      go: actions.showKeys,
    },
  );
  return query ? ranked(out, query, (h) => [h.label], out.length) : out;
}

/**
 * The task a person is looking at, from the route — what "Assign" acts on
 * before any search has answered, and what a create files next to.
 */
export function routeTask(path: string[]): string {
  const at = resolve(path);
  return at.resolved && at.screen === "item" ? at.id : "";
}

/**
 * The project a person is looking at, from the route: the project page, or
 * the project a task key names. "" elsewhere, and for a task addressed by
 * its uuid, whose project the route does not say.
 */
export function routeProject(path: string[]): string {
  const at = resolve(path);
  if (!at.resolved) return "";
  if (at.screen === "project") return at.key;
  if (at.screen === "item") {
    const dash = at.id.lastIndexOf("-");
    const head = dash > 0 ? at.id.slice(0, dash) : "";
    return /^[A-Z][A-Z0-9_]*$/.test(head) ? head : "";
  }
  return "";
}

/** A question in the form two askings of it compare equal in. */
export function normalizeQuestion(term: string): string {
  return term.trim().replace(/\s+/g, " ").toLowerCase();
}

/**
 * Whether a term reads as a QUESTION worth answering from the company's
 * knowledge — and so worth tokens.
 *
 * THREE WORDS AND TWELVE CHARACTERS. Every answer spends the company's
 * tokens, and a palette is mostly used to jump somewhere: "ENG-42", "spend",
 * "@ana" and "turns" are destinations, and a phrase of three words that is at
 * least twelve characters long is the shortest thing that reads as asking
 * ("why is checkout slow", "who owns billing"). Below it the rows are the
 * answer.
 */
export function answerable(term: string): boolean {
  const t = term.trim();
  return t.length >= ANSWER_MIN_CHARS && t.split(/\s+/).length >= ANSWER_MIN_WORDS;
}

export const ANSWER_MIN_WORDS = 3;
export const ANSWER_MIN_CHARS = 12;

/**
 * How long typing must pause before a question is answered, in ms.
 *
 * 800: a typist's gap between keystrokes is 150–250 ms and a thinking pause
 * mid-sentence rarely passes half a second, so 800 is a question finished
 * rather than one being typed — and every trigger spends tokens, so a pause
 * that fired mid-word would pay for questions nobody asked.
 */
export const ANSWER_IDLE_MS = 800;
