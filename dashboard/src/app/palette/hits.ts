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
import {
  DESTINATIONS,
  SIDEBAR_GROUPS,
  WORKSPACES,
  landingPath,
  pathShown,
  serviceShown,
  workspaceRow,
  workspaceShown,
  type ServicesRun,
  type SidebarPlace,
  type WorkspaceRow,
} from "../nav.ts";
import { accessNames, holdsAny, type Grant } from "~/lib/access.ts";
import type { Navigator } from "../router.tsx";
import { artifactPath, resolve } from "../routes.ts";
import type { Recent } from "~/lib/recents.ts";
import { DENSITIES, THEMES, type ViewerPrefs } from "~/lib/prefs.ts";
import type { OrgIndex, Seat, Unit } from "~/lib/seats.ts";
import { COLLEAGUE_QUERY_MAX } from "~/contract/wire.ts";
import { sequenceWords } from "../keymap.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { KIND_WORDS } from "~/lib/artifacts/words.ts";
import { changePath, changeRef, parseChangeRef } from "~/lib/code.ts";
import type { ArtifactKind, ArtifactListItem } from "~/protocol/index.ts";

/**
 * The scopes, in the order the tab row draws them and Tab walks them.
 *
 * SEVEN, because seven questions do not fit one ranked list: the company's
 * WORK, its PAGES and its ARTIFACTS are server searches ranked by the engine
 * and cannot be ranked against a list of screens; its CODE is a scatter over
 * the git nodes that answers lines, not records, in the repositories' order; finding a PERSON wants the
 * roster and the engine's own name tiers and nothing else, so a colleague is
 * not buried under four tools whose names happen to match; and running a
 * COMMAND has no name to search for at all. ALL is the launcher over the lot.
 *
 * ARTIFACTS AND CODE ARE ASKED IN THEIR OWN SCOPES ONLY. All already asks
 * three server questions, and a fourth would take the slot the screen under
 * the palette keeps — and a code search is a request to every git node, which
 * a launcher's every pause must not send; All offers a row that carries the
 * term to Artifacts instead, and Code › Search is one Enter away in Code's.
 *
 * A SIGIL still opens three of them, typed in the same box in the same
 * keystroke as the query: `#` tasks, `@` agents, `>` actions. The sigil
 * selects the tab and leaves the box, so the tab row — not a character the
 * reader has to remember is special — says which scope they are in.
 */
export const SCOPES = [
  { id: "all", label: "All" },
  { id: "tasks", label: "Tasks", sigil: "#", service: "tracker" },
  { id: "pages", label: "Pages", service: "knowledge" },
  { id: "artifacts", label: "Artifacts" },
  { id: "code", label: "Code", service: "code" },
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

/** How many artifacts the Artifacts scope lists: a ranked answer is opened, not scrolled. */
export const ARTIFACT_HITS = 8;

/**
 * How many matching lines the Code scope lists: a launcher's screenful, the
 * rest one Enter away on Code › Search, which groups every line the engine
 * answered by repository and file.
 */
export const CODE_HITS = 8;

/**
 * The shortest term the Code scope asks the engine to search: THREE, because
 * the code host's plan needs one clause of three bytes or more (its trigrams)
 * and refuses `bad_params` below it — a refusal per keystroke a reader would
 * read as a broken search.
 */
export const CODE_SEARCH_MIN = 3;
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

/**
 * How well a field matches a query WORD BY WORD, lower is better, or -1: the
 * whole query as [score] reads it, or else every word of it the start of some
 * word of the field — so "two step" finds "two-step verification" and "my
 * account" finds "my account" typed with a space too many. Ranked after every
 * whole match, which is the reader's own spelling of the field.
 */
export function wordScore(text: string, query: string): number {
  const whole = score(text, query);
  if (whole >= 0) return whole;
  const wanted = wordsOf(query);
  if (wanted.length < 2) return -1;
  const own = wordsOf(text);
  return wanted.every((w) => own.some((o) => o.startsWith(w))) ? 1000 + text.length / 100 : -1;
}

/** A field's words, lowercase, split at anything that is not a letter or a digit. */
function wordsOf(text: string): string[] {
  return text
    .toLowerCase()
    .split(/[^\p{L}\p{N}]+/u)
    .filter(Boolean);
}

/** One row the palette offers, before it is drawn. */
export interface Hit {
  id: string;
  icon: GlyphName;
  /** An artifact's kind, drawn with the kind's own glyph in place of `icon`, as the gallery and the sidebar draw it. */
  kind?: ArtifactKind;
  label: string;
  hint: string;
  /** A short fact at the row's end, never cut with the label as `hint` is. */
  meta?: string;
  /**
   * Taking the row. The palette closes after it unless it returns `false`,
   * which a row that leads into a step of the palette (Change theme) does.
   */
  go: () => boolean | void;
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
 * The ranked matches of a list whose fields come in TIERS, best first, capped:
 * an item matched in an earlier tier outranks every item matched only in a
 * later one, whatever the scores, and within a tier [wordScore] orders them.
 */
function rankedInTiers<T>(
  items: readonly T[],
  query: string,
  tiers: (item: T) => readonly (readonly string[])[],
  cap: number,
): T[] {
  const scored: { item: T; tier: number; s: number }[] = [];
  for (const item of items) {
    if (!query) {
      scored.push({ item, tier: 0, s: 0 });
      continue;
    }
    const fields = tiers(item);
    for (let tier = 0; tier < fields.length; tier++) {
      const s = Math.min(
        ...(fields[tier] ?? []).map((f) => {
          const v = wordScore(f, query);
          return v < 0 ? Infinity : v;
        }),
      );
      if (Number.isFinite(s)) {
        scored.push({ item, tier, s });
        break;
      }
    }
  }
  return scored
    .sort((a, b) => a.tier - b.tier || a.s - b.s)
    .slice(0, cap)
    .map((x) => x.item);
}

/**
 * Where the reader just was, most recent first: what an empty palette should
 * offer, because the likeliest place to go is the one they just left. Not a
 * place the company no longer runs the service for, whose page is a card on
 * Integrations now.
 */
export function recentHits(recents: readonly Recent[], nav: Navigator, runs: ServicesRun): Hit[] {
  return recents
    .filter((r) => pathShown(r.path, runs))
    .map((r) => ({
      id: `recent-${r.path.join("/")}`,
      icon: "clock",
      label: r.label,
      // THE WORKSPACE BY ITS NAME, as the sidebar says it: the stored value is
      // its key, and "Q4 roadmap artifacts" read as one phrase.
      hint:
        WORKSPACES.find((w) => w.key === r.workspace)?.label ?? (r.workspace || r.path.join(" / ")),
      go: () => nav.to(r.path),
    }));
}

/**
 * Who a destination's lock is read for: nobody signed in, the access of
 * somebody who is, or `null` while the viewer has not said — when nothing is
 * claimed either way, since a lock claimed on a guess is a refusal nobody made.
 */
export type Standing = { anonymous: true } | { anonymous: false; grants: readonly string[] } | null;

/**
 * The screens, matched by name — and then by the words a reader looks for one
 * by ([Destination.keywords]), so "password" or "2fa" finds Account. A label
 * match always ranks first, so a screen called what was typed is never pushed
 * under one that merely holds it; the hint's prose is never searched.
 *
 * A SCREEN THE READER HOLDS NONE OF THE ACCESS FOR SAYS WHAT WOULD OPEN IT,
 * and only then: "needs a token" named a credential a person signing in has no
 * use for, and was drawn beside every guarded row for a reader who already
 * held the access. To a reader NOBODY SIGNED IN it says "needs sign-in", the
 * step before any access — naming one would tell them to hold something before
 * they are anybody. To a reader who is, it names the access ("needs Tracker
 * access").
 */
export function destinationHits(
  query: string,
  nav: Navigator,
  cap: number,
  standing: Standing,
  runs: ServicesRun,
): Hit[] {
  // A PLACE READING A SERVICE THE COMPANY DOES NOT RUN IS NO DESTINATION, as
  // it is no row (`nav.ts`'s `serviceShown`) — and a workspace's own entry is
  // offered exactly where its row is (`workspaceShown`), because it opens
  // where the row does: My work, whose landing section is the tracker's, is a
  // row where only the code host runs, and its entry opens its Reviews.
  const offered = DESTINATIONS.filter((d) => {
    const row = d.key === d.workspace ? workspaceRow(d.workspace) : undefined;
    return row ? workspaceShown(row, runs) : serviceShown(d, runs);
  });
  const held = standing === null || standing.anonymous ? null : standing.grants;
  return rankedInTiers(offered, query, (d) => [[d.label], d.keywords ?? []], cap).map((d) => {
    // A WORKSPACE'S OWN ENTRY OPENS WHERE ITS ROW DOES ([landingPath]), and
    // is locked as its row is (`workspaceMissing`): Settings is no lock to a
    // reader holding Tracker, whose Token spend it opens on.
    const row = d.key === d.workspace ? workspaceRow(d.workspace) : undefined;
    const path = row ? landingPath(row, held, runs) : d.path;
    const needs = row ? workspaceNeeds(row, runs) : d.grants;
    return {
      id: `nav-${d.key}`,
      icon: d.icon,
      label: d.label,
      hint: lockedHint(d.hint, needs, standing),
      go: () => nav.to(path),
    };
  });
}

/** One heading of the empty palette's destinations. */
export interface DestinationGroup {
  /** The sidebar place it holds: `you`, `company`, `workspace`, `foot`, `menu`. */
  place: SidebarPlace;
  label: string;
  hits: Hit[];
}

/**
 * Every destination, under the sidebar's own headings, for the palette before
 * anything is typed: the reader's own places as "Go to", then Company,
 * Workspace, Settings and Account, each in the sidebar's order. One flat list
 * of every place hid the tree the sidebar draws, and set look-alikes side by
 * side with nothing to tell them apart (Token spend in Settings, Budgets in
 * Company). A place no group names is headed by its first row's own name.
 */
export function destinationGroups(
  nav: Navigator,
  standing: Standing,
  runs: ServicesRun,
): DestinationGroup[] {
  const hits = new Map(destinationHits("", nav, Infinity, standing, runs).map((h) => [h.id, h]));
  const groups: DestinationGroup[] = [];
  for (const d of DESTINATIONS) {
    const hit = hits.get(`nav-${d.key}`);
    const place = workspaceRow(d.workspace)?.place;
    if (!hit || !place) continue;
    let group = groups.find((g) => g.place === place);
    if (!group) {
      const named = SIDEBAR_GROUPS.find((g) => g.place === place)?.label;
      const label = named || (place === "you" ? "Go to" : (workspaceRow(d.workspace)?.label ?? ""));
      group = { place, label, hits: [] };
      groups.push(group);
    }
    group.hits.push(hit);
  }
  return groups;
}

/**
 * What opens some part of a workspace, as its sidebar row is locked
 * (`workspaceOpen`): the workspace's own grants, else nothing where any
 * section the company runs is open to every reader, else any grant that opens
 * one of those — so My work needs Tracker where its Reviews is not offered,
 * and nothing where it is.
 */
function workspaceNeeds(row: WorkspaceRow, runs: ServicesRun): readonly Grant[] | undefined {
  if (row.grants && row.grants.length > 0) return row.grants;
  const sections = row.sections.filter((s) => serviceShown(s, runs));
  if (row.personal || sections.length === 0) return undefined;
  if (sections.some((s) => s.personal || !s.grants || s.grants.length === 0)) return undefined;
  return [...new Set(sections.flatMap((s) => s.grants ?? []))];
}

/** A destination's hint, and what it needs where the reader lacks it. */
function lockedHint(
  hint: string,
  grants: readonly Grant[] | undefined,
  standing: Standing,
): string {
  if (standing === null || !grants || grants.length === 0) return hint;
  if (standing.anonymous) return `${hint} · needs sign-in`;
  return holdsAny(standing.grants, grants) ? hint : `${hint} · needs ${accessNames(grants)} access`;
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
      hint: "as a trace",
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

/**
 * A change's reference is a destination too: `web!42` — a repository's name,
 * `!`, its number — opens that change, offered first and exactly as a pasted
 * id is, wherever the company hosts its code in the engine. Parsed by the
 * engine's own grammar (`lib/code.ts`), so a term that only looks like one
 * offers nothing.
 */
export function changeHits(term: string, nav: Navigator, runs: ServicesRun): Hit[] {
  if (!runs.code) return [];
  const named = parseChangeRef(term.trim());
  if (!named) return [];
  const reference = changeRef(named.repo, named.number);
  return [
    {
      id: `change-${reference}`,
      icon: "diff",
      label: reference,
      hint: "Open the change",
      go: () => nav.to(changePath(named.repo, named.number)),
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

/** The source the engine names its own agent tools by. */
export const BUILTIN_TOOL_SOURCE = "builtin";

/**
 * The tools matching a query; nothing on an empty one, since a tool is found by
 * name.
 *
 * THE ENGINE'S OWN AGENT TOOLS ARE LEFT OUT. They are how an agent works, not a
 * place a reader goes, and "artif" listed `get_artifact` and
 * `set_artifact_schema` above the artifacts a reader was looking for. They stay
 * listed in Settings, under MCP Tools; what is offered here is a tool the
 * company connected.
 */
export function toolHits(
  tools: readonly { name: string; source: string }[],
  query: string,
  nav: Navigator,
  cap: number,
): Hit[] {
  if (!query) return [];
  const connected = tools.filter((t) => t.source !== BUILTIN_TOOL_SOURCE);
  return ranked(connected, query, (t) => [t.name], cap).map((tool) => ({
    id: `tool-${tool.name}`,
    icon: "wrench",
    label: tool.name,
    hint: tool.source,
    go: () => nav.to(["settings", "mcp-tools"], { q: tool.name }),
  }));
}

/** The artifacts a listing answered, as rows that open them. */
export function artifactHits(items: readonly ArtifactListItem[], nav: Navigator): Hit[] {
  return items.map((item) => ({
    id: `artifact-${item.id}`,
    icon: "layers",
    kind: item.kind,
    label: item.title,
    // THE KIND AT THE ROW'S END, as a page's space is: a hint is cut with the
    // label, and a long title took the kind with it.
    hint: "",
    meta: KIND_WORDS[item.kind],
    go: () => nav.to(artifactPath(item.id)),
  }));
}

/**
 * The searches a term can be handed to whole, as the screens that run them:
 * only those the company runs the service for.
 */
export function searchHits(term: string, nav: Navigator, runs: ServicesRun): Hit[] {
  if (!term) return [];
  return [
    // THE COMPANY'S WORK FIRST, ranked as an agent ranks it: the full search
    // with its modes, which the Tasks scope's own rows are the top of.
    ...(runs.tracker
      ? [
          {
            id: "search-work",
            icon: "search",
            label: `Search all work for “${term}”`,
            hint: "ranked as an agent ranks it",
            go: () => nav.to(["work", "search"], { q: term }),
          } satisfies Hit,
        ]
      : []),
    {
      id: "search-events",
      icon: "chart-no-axes-gantt",
      label: `Events mentioning “${term}”`,
      hint: "the event log, filtered",
      go: () => nav.to(["live", "events"], { q: term }),
    },
    ...(runs.knowledge
      ? [
          {
            id: "search-knowledge",
            icon: "book-open",
            label: `Knowledge search for “${term}”`,
            hint: "every mode, every container",
            go: () => nav.to(["knowledge"], { q: term }),
          } satisfies Hit,
        ]
      : []),
  ];
}

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
    signIn: () => void;
    clearRecents: () => void;
    showKeys: () => void;
    /** Step into the list of themes; the palette stays open. */
    changeTheme: () => void;
    /** The name of the theme in effect, for the row's hint. */
    themeName: string;
    /** Open the project switcher; null where the engine serves no company to switch in. */
    switchProject: (() => void) | null;
    hash: string;
  },
): Hit[] {
  const out: Hit[] = [
    {
      id: "cmd-theme",
      icon: "palette",
      label: "Change theme…",
      hint: `${actions.themeName} is on`,
      go: () => {
        actions.changeTheme();
        return false;
      },
    },
  ];
  // THE MODE IS ITS OWN CHOICE, named as the interface names it: light, dark
  // or the machine's, whatever the theme.
  for (const choice of THEMES) {
    if (choice === prefs.theme) continue;
    out.push({
      id: `cmd-theme-${choice}`,
      icon: choice === "dark" ? "moon" : choice === "light" ? "sun" : "monitor",
      label: `Mode: ${choice === "system" ? "match system" : choice}`,
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
  out.push(
    {
      id: "cmd-copy-link",
      icon: "link",
      label: "Copy link to this page",
      hint: actions.hash,
      go: actions.copyLink,
    },
    // WHO THIS BROWSER ACTS AS is a sign-in, never a token typed into a page
    // any script could read back: the session cookie is the only credential
    // the dashboard holds, and a Tier A token is exchanged for one there.
    {
      id: "cmd-sign-in",
      icon: "key",
      label: "Sign in as somebody else",
      hint: "or with an API token",
      go: actions.signIn,
    },
    {
      id: "cmd-clear-recents",
      icon: "clock",
      label: "Clear recents",
      hint: "yours, in this browser",
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
    ...(actions.switchProject ? [switchProjectHit(actions.switchProject)] : []),
  );
  return query ? ranked(out, query, (h) => [h.label], out.length) : out;
}

/**
 * The switcher, as a row: it opens the panel Work's trail opens, from
 * wherever the palette was, rather than going anywhere itself.
 */
export function switchProjectHit(open: () => void): Hit {
  return {
    id: "cmd-switch-project",
    icon: "columns-3",
    label: "Switch project…",
    hint: `or ${sequenceWords("project")}, from anywhere`,
    go: open,
  };
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
