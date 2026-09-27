/**
 * ⌘K: one search for the whole product, and the shortest way to act on what
 * it finds.
 *
 * ON THE KIT'S `CommandPalette`, which owns the surface — the combobox, the
 * scope tabs driven from the field, the lead's live region, the key legend's
 * band. What is here is what it searches and what a row does.
 *
 * # What it searches, per scope (`hits.ts` has the rules)
 *
 *  - ALL — screens, a pasted event / trace / turn id, the company's tasks and
 *    pages (three of each), seats and units, tools, and the three actions;
 *    recents when nothing is typed;
 *  - TASKS — `work_search`, hybrid, eight hits, with its four answers kept
 *    apart (too short, searching, refused, nothing matched) and a row to the
 *    full search screen;
 *  - PAGES — `knowledge`, the company's one knowledge backend;
 *  - AGENTS — the org chart with each seat's ring from the agents push, the
 *    engine's own name tiers (`colleague`) first;
 *  - ACTIONS — the per-browser commands, and the three writes.
 *
 * Three server questions at most (`work_search`, `knowledge`, `colleague`),
 * and in a picker two (`work_item`, `colleague`): the socket holds four
 * query slots and the screen under the palette keeps one. The company's
 * projects (`work_projects`) are read once, as the palette opens — before a
 * term can be typed, so they never contend with the three searches.
 *
 * # What it does, as you
 *
 * Three rows change the company, each through `useAct` as the person the
 * token is bound to (ADR-0024), each drawn for every reader and saying why
 * where it cannot act:
 *
 *  - "Assign {KEY} to an agent…" opens a picker over the agents — the one the
 *    term NAMES first, when the engine's name tiers find exactly one — and
 *    assigns on a pick, conditioned on the version the picker read;
 *  - "Ask {agent} about “…”" files the question as an ask on a new task,
 *    whose answer lands in the asker's inbox;
 *  - "Create task “…”" files it, and opens it.
 *
 * NOTHING IS PICKED FOR THE PERSON. A suggestion is the seat the term names
 * when exactly one does (`colleague`'s `match`); several is a list, and no
 * name at all is the full roster. The same holds for WHERE a create lands:
 * the project on screen, else the engine's own default for the person's seat
 * (`viewer.project`), else a step that lists the company's projects — the
 * one holding the top task hit first, as a suggestion the person still
 * presses. A person whose team owns no project can still file work; what
 * they cannot do is have it filed somewhere nobody chose.
 *
 * And the ANSWER (`answer.ts`): a paragraph from the company's own pages and
 * tasks, over the rows, when the term reads as a question — spent in tokens,
 * on the asker's behalf, and said so.
 */

import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import {
  CommandPalette as KitPalette,
  Kbd,
  type CommandPaletteGroup,
  type CommandPaletteItem,
  type CommandPaletteScopeProps,
} from "@crewlethq/ui";
import {
  BookOpenGlyph,
  CircleAlertGlyph,
  FileTextGlyph,
  FolderGlyph,
  GlobeGlyph,
  MessageSquareGlyph,
  PlusGlyph,
  SearchGlyph,
  UsersGlyph,
  ZapGlyph,
} from "@crewlethq/icons/glyphs";
import { useNavigator, useRoute } from "../router.tsx";
import { capsOf, keyRow, matchesRow } from "../keymap.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useAct } from "~/lib/useAct.ts";
import { useRecents, forgetAll } from "~/lib/recents.ts";
import { useViewerPrefs } from "~/lib/prefs.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents, useOrg, useTools } from "~/lib/store-hooks.ts";
import {
  activityOf,
  handleLabel,
  indexOrg,
  labelOf,
  nameOfIn,
  ringOf,
  type Seat,
} from "~/lib/seats.ts";
import { statusLabel } from "~/lib/work.ts";
import { fmtCount } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { requestToken } from "~/protocol/index.ts";
import type { ActResult } from "~/protocol/act.ts";
import type { SearchOutcome, WorkProjectRow } from "~/protocol/index.ts";
import { PriorityMark, StatusMark } from "~/components/work.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  ALL_ROWS,
  ALL_ROWS_UNDER_ANSWER,
  GO_TO_ROWS,
  PROJECT_PAGE,
  SCOPES,
  SOURCE_CHIPS,
  SEARCH_MIN,
  TASK_HITS,
  colleagueSendable,
  commandHits,
  destinationHits,
  idHits,
  matchingSeats,
  matchingUnits,
  readSigil,
  recentHits,
  routeProject,
  routeTask,
  searchHits,
  toolHits,
  type Hit,
  type ScopeId,
} from "./hits.ts";
import { useKnowledgeAnswer, type AnswerState, type KnowledgeAnswerReceipt } from "./answer.ts";

/** What a project step files once the person has chosen where. */
type Filing =
  { kind: "create"; title: string } | { kind: "ask"; seat: Seat; question: string; body: string };

/**
 * The project a project step offers first — the one the top task hit is
 * filed in, and the sentence saying so — held from the moment a step opens,
 * because inside a picker the search that found the hit is no longer asked.
 */
type ProjectSuggestion = { key: string; why: string } | null;

/** A picker the palette has stepped into, replacing its results. */
type Pick =
  | { kind: "assign"; item: string }
  /** Which agent to ask; `project` "" means the project step follows. */
  | { kind: "ask"; question: string; body: string; project: string; suggested: ProjectSuggestion }
  /** Which project to file into. */
  | { kind: "project"; filing: Filing; suggested: ProjectSuggestion };

/** A sentence held in the lead until the next keystroke or press. */
interface Notice {
  /** The engine refused the press, rather than the palette explaining one. */
  refused: boolean;
  text: string;
}

/** The note a notice is drawn as. */
function noticeClass(notice: Notice): string {
  return notice.refused ? "palette-answer-note is-refused" : "palette-answer-note";
}

export function CommandPalette({
  onClose,
  onShowKeys,
}: {
  onClose: () => void;
  /** Open the key legend; the palette closes on the way, as for every row. */
  onShowKeys: () => void;
}) {
  const nav = useNavigator();
  const route = useRoute();
  const viewer = useViewer();
  const prefs = useViewerPrefs();
  const recents = useRecents();
  const agents = useAgents();
  const org = useOrg();
  const tools = useTools();
  const index = useMemo(() => indexOrg(org), [org]);

  const [scope, setScope] = useState<ScopeId>("all");
  const [query, setQuery] = useState("");
  const [pick, setPick] = useState<Pick | null>(null);
  const [pickQuery, setPickQuery] = useState("");
  const [notice, setNotice] = useState<Notice | null>(null);

  // A ROW THAT STEPS INTO A PICKER, OR THAT WAITS FOR THE ENGINE, KEEPS THE
  // SURFACE OPEN. The kit closes after every row's `onSelect`, which is right
  // for a launcher and wrong for a row whose outcome the person is owed: a
  // refusal drawn on a palette that already closed is a refusal nobody reads.
  const keepOpen = useRef(false);
  const close = () => {
    if (keepOpen.current) {
      keepOpen.current = false;
      return;
    }
    onClose();
  };
  const stay = () => {
    keepOpen.current = true;
  };

  const term = query.trim();
  const lower = term.toLowerCase();
  const picking = pick !== null;
  const pickTerm = pickQuery.trim();

  // ---- the three questions ---------------------------------------------
  const wantsTasks =
    !picking && (scope === "all" || scope === "tasks") && term.length >= SEARCH_MIN;
  const tasks = useQuery(
    "work_search",
    { q: term, mode: "hybrid", limit: TASK_HITS },
    { enabled: wantsTasks },
  );
  const wantsPages =
    !picking && (scope === "all" || scope === "pages") && term.length >= SEARCH_MIN;
  const pages = useQuery("knowledge", { q: term }, { enabled: wantsPages });
  const pickingAgent = pick?.kind === "assign" || pick?.kind === "ask";
  const nameTerm = picking ? pickTerm : term;
  const wantsNames =
    (pickingAgent || (!picking && (scope === "all" || scope === "agents"))) &&
    colleagueSendable(nameTerm);
  const names = useQuery("colleague", { q: nameTerm }, { enabled: wantsNames });
  // THE COMPANY'S PROJECTS, once: to name the project a create lands in, and
  // to offer them when nothing on screen or in the chart says which.
  const projectsRead = useQuery("work_projects", { limit: PROJECT_PAGE });
  const projects: WorkProjectRow[] | null = projectsRead.data?.projects ?? null;
  const projectName = (key: string) => projects?.find((p) => p.key === key)?.name ?? "";

  // WHICH TERM EACH ANSWER ON HAND IS FOR. `useQuery` keeps the previous
  // answer across a change of parameters — right for a polled table, wrong
  // for a search box, where it listed "auth"'s items as hits for "authz" with
  // nothing saying a search was in flight. The answer carries no echo of its
  // question, so the term is remembered against the answer's own identity:
  // a new object is this term's answer arriving, and nothing else.
  const taskAnswer = useSettled(tasks, term, wantsTasks);
  const pageAnswer = useSettled(pages, term, wantsPages);
  const nameAnswer = useSettled(names, nameTerm, wantsNames);

  // ---- the answer --------------------------------------------------------
  const answer = useKnowledgeAnswer(term, !picking && (scope === "all" || scope === "pages"));
  // A QUESTION THIS READER WILL BE ANSWERED leads the list with a card, and
  // All makes room for the actions under it (`ALL_ROWS_UNDER_ANSWER`).
  const answering =
    answer.kind === "waiting" || answer.kind === "asking" || answer.kind === "answered";
  const allRows = answering ? ALL_ROWS_UNDER_ANSWER : ALL_ROWS;

  // ---- the writes --------------------------------------------------------
  const assign = useAct("update_work_item");
  const create = useAct("create_work_item");
  const pickItem = pick?.kind === "assign" ? pick.item : "";
  const itemRead = useQuery("work_item", { id: pickItem }, { enabled: pickItem !== "" });

  // The task at hand: the one on screen, else the best task hit for this term.
  const onScreen = routeTask(route.path);
  const topTask = scope === "all" && taskAnswer.data ? taskAnswer.data.hits[0] : undefined;
  const taskAtHand = onScreen || topTask?.key || "";
  // Where a create lands: the project on screen, else the person's own
  // default, else the person chooses (a project step), with the project the
  // top task hit is filed in offered first.
  const onProject = routeProject(route.path);
  const project = onProject || viewer.project;
  const projectWhy = onProject
    ? "the project you are on"
    : viewer.project
      ? "where your own work lands"
      : "";
  const hitProject: ProjectSuggestion = topTask?.project
    ? { key: topTask.project, why: `where ${topTask.key} is filed` }
    : null;
  // A COMPANY WITH NO PROJECT AT ALL is the one reason nobody can file: there
  // is nothing to choose. Only once the list has said so — an unread list is
  // a step that says it is reading.
  const noProjects = !project && projects !== null && projects.length === 0;

  // THE ONE SEAT THE TERM NAMES, if it names exactly one AGENT. Never a pick
  // among several: a suggestion is the engine's `match`, or nothing.
  const suggested = useMemo(() => {
    const match = nameAnswer.data?.match;
    const seat = match ? index.byHandle.get(match.handle) : undefined;
    return seat && seat.kind === "agent" ? { seat, why: match!.why } : null;
  }, [nameAnswer.data, index]);

  // THE SUGGESTION MADE FROM THE SEARCH TERM, held from the moment a picker
  // opens: the picker's own typing asks `colleague` about a different name,
  // and the search term's answer is gone once it is no longer asked.
  const [suggestedForPick, setSuggestedForPick] = useState<typeof suggested>(null);
  const openPick = (next: Pick) => {
    setSuggestedForPick(suggested);
    setPick(next);
    setPickQuery("");
  };
  /** The project step, for a filing that has everything but where. */
  const openProjectPick = (filing: Filing, suggestion: ProjectSuggestion) => {
    setPick({ kind: "project", filing, suggested: suggestion });
    setPickQuery("");
  };
  const leavePick = () => {
    setPick(null);
    setSuggestedForPick(null);
  };

  // A fresh keystroke clears what the last press said.
  useEffect(() => setNotice(null), [query, pickQuery, scope]);

  const liveOf = (seat: Seat) => agents.find((a) => a.role === seat.name);

  const seatItem = (seat: Seat, hint: string, onSelect: () => void): CommandPaletteItem => {
    const state = activityOf(liveOf(seat));
    return {
      id: `seat-${seat.handle}`,
      icon: (
        <SeatAvatar
          name={seat.name}
          kind={seat.kind}
          size="xs"
          ring={seat.kind === "agent" ? ringOf(state) : undefined}
          decorative
        />
      ),
      label: seat.name,
      hint,
      onSelect,
    };
  };

  const seatHint = (seat: Seat, why?: string) => {
    if (seat.kind === "human") return why ? `human teammate · ${why}` : "human teammate";
    // THE CHART'S NAMES: "Paused by Jane Founder", never the handle.
    const state = labelOf(liveOf(seat), Date.now(), nameOfIn(index));
    return [handleLabel(seat.handle), state, why].filter(Boolean).join(" · ");
  };

  // ---- a write, pressed from a row ----------------------------------------
  /**
   * What a press came to, and whether the palette may close. A refusal stays
   * in the lead — drawn on a palette that closed, nobody would read it — and
   * everything else is the toast's to report (an unknown one stays, with its
   * Retry, after the palette has gone).
   */
  const settle = (result: ActResult | null): result is Exclude<ActResult, { kind: "refused" }> => {
    if (result === null) return false;
    if (result.kind === "refused") {
      setNotice({ refused: true, text: result.sentence });
      return false;
    }
    return true;
  };

  const doAssign = async (item: string, seat: Seat) => {
    const version = itemRead.data?.task.version;
    if (version === undefined) {
      setNotice({ refused: false, text: `Still reading ${item} — a moment, then pick again.` });
      return;
    }
    const result = await assign.run(
      { item, assignee: seat.handle, if_match: version },
      { done: `Assigned ${item} to ${seat.name}` },
    );
    if (settle(result)) onClose();
  };

  const doAsk = async (seat: Seat, question: string, body: string, where: string) => {
    const result = await create.run(
      {
        title: question,
        assignee: seat.handle,
        ask: seat.handle,
        project: where,
        ...(body ? { body } : {}),
      },
      { done: `Asked ${seat.name}` },
    );
    if (settle(result)) onClose();
  };

  const doCreate = async (title: string, where: string) => {
    const result = await create.run({ title, project: where }, { done: `Created “${title}”` });
    if (!settle(result)) return;
    // THE NEW TASK IS WHERE THE PERSON GOES NEXT: they named it to work on
    // it. The route change closes the palette. An unknown outcome has no key
    // to go to, and its toast is what says so.
    const key =
      result.kind === "unknown" ? undefined : (result.receipt as { key?: string } | null)?.key;
    if (key) nav.to(["work", key]);
    else onClose();
  };

  /** File what a project step was holding, into the project chosen. */
  const file = (filing: Filing, where: string) => {
    if (filing.kind === "create") void doCreate(filing.title, where);
    else void doAsk(filing.seat, filing.question, filing.body, where);
  };

  /** Where a create lands, as a row's hint says it: "in ENG · Core platform". */
  const whereHint = (key: string, why: string): ReactNode => {
    const name = projectName(key);
    // THE WHY ON HOVER: the design names the project, and a reader wondering
    // why THAT one is answered without the row growing a clause.
    return (
      <span
        title={why ? `${key} — ${why}` : undefined}
      >{`in ${key}${name ? ` · ${name}` : ""}`}</span>
    );
  };

  // ---- the three action rows -----------------------------------------------
  const answeredMd = answer.kind === "answered" ? answer.answer : null;

  const actionRows = (): CommandPaletteItem[] => {
    if (picking) return [];
    const rows: CommandPaletteItem[] = [];
    const blockedRow = (reason: string) => () => {
      stay();
      setNotice({ refused: false, text: reason });
    };
    if (taskAtHand && (scope === "all" || (scope === "actions" && onScreen))) {
      const why = assign.access.can ? "" : assign.access.reason;
      rows.push({
        id: "act-assign",
        icon: <UsersGlyph size="sm" />,
        label: `Assign ${taskAtHand} to an agent…`,
        hint: why
          ? why
          : suggested
            ? `suggested: ${suggested.seat.name} — ${suggested.why}`
            : "choose who",
        meta: <Kbd subtle keys={capsOf(keyRow("search.assign").presses[0]!)} />,
        onSelect: why
          ? blockedRow(why)
          : () => {
              stay();
              openPick({ kind: "assign", item: taskAtHand });
            },
      });
    }
    if (term && (scope === "all" || scope === "actions")) {
      // DISABLED ONLY FOR A REASON THE PERSON CANNOT CHOOSE THEIR WAY PAST:
      // this browser may not write (F-7), or the company has no project at
      // all. A person with no default project is not one of them — the step
      // asks where.
      const why = !create.access.can
        ? create.access.reason
        : noProjects
          ? "This company has no project yet — add one under Work › Projects to file work."
          : "";
      const body = answeredMd ? answerBody(answeredMd) : "";
      const choose = hitProject
        ? `choose a project — ${hitProject.key} is suggested`
        : "choose a project";
      rows.push({
        id: "act-ask",
        icon: <MessageSquareGlyph size="sm" />,
        label: suggested
          ? `Ask ${suggested.seat.name} about “${term}”`
          : `Ask an agent about “${term}”…`,
        hint: why || "opens an ask, answer lands in your inbox",
        meta: <Kbd subtle keys={capsOf(keyRow("search.ask").presses[0]!)} />,
        onSelect: why
          ? blockedRow(why)
          : () => {
              stay();
              if (!suggested) {
                openPick({ kind: "ask", question: term, body, project, suggested: hitProject });
              } else if (project) void doAsk(suggested.seat, term, body, project);
              else
                openProjectPick(
                  { kind: "ask", seat: suggested.seat, question: term, body },
                  hitProject,
                );
            },
      });
      rows.push({
        id: "act-create",
        icon: <PlusGlyph size="sm" />,
        label: `Create task “${term}”`,
        hint: why || (project ? whereHint(project, projectWhy) : choose),
        meta: <Kbd subtle keys={capsOf(keyRow("search.create").presses[0]!)} />,
        onSelect: why
          ? blockedRow(why)
          : () => {
              stay();
              if (project) void doCreate(term, project);
              else openProjectPick({ kind: "create", title: term }, hitProject);
            },
      });
    }
    return rows;
  };

  // ---- the rows ------------------------------------------------------------
  const asItems = (hits: Hit[]): CommandPaletteItem[] =>
    hits.map((h) => ({
      id: h.id,
      icon: <Mark name={h.icon} size="sm" />,
      label: h.label,
      hint: h.hint,
      onSelect: h.go,
    }));

  const taskRows = (cap: number): CommandPaletteItem[] => {
    const hits = taskAnswer.data?.hits ?? [];
    return hits.slice(0, cap).map((item) => ({
      id: `task-${item.key}`,
      icon: <StatusMark status={item.status} />,
      label: (
        <>
          <span className="palette-key">{item.key}</span> {item.title}
        </>
      ),
      hint: [
        statusLabel(item.status),
        item.assignee ? (index.byHandle.get(item.assignee)?.name ?? item.assignee) : "unassigned",
      ].join(" · "),
      meta: <PriorityMark priority={item.priority} />,
      onSelect: () => nav.to(["work", item.key]),
    }));
  };

  const pageRows = (cap: number): CommandPaletteItem[] =>
    (pageAnswer.data?.hits ?? []).slice(0, cap).map((page) => ({
      id: `page-${page.id}`,
      icon: <FileTextGlyph size="sm" />,
      label: page.title,
      meta: page.container || undefined,
      onSelect: () => openPage(page.id, page.url),
    }));

  const openPage = (id: string, url: string) => {
    // A PAGE THIS ENGINE HOLDS OPENS HERE; a vendor wiki's opens there.
    if (url && /^https?:/.test(url) && pageAnswer.data?.backend !== "native") {
      window.open(url, "_blank", "noopener");
    } else nav.to(["knowledge", "pages", id]);
  };

  // THE ANSWER'S SOURCES ARE ROWS TOO, where the search did not already list
  // them: a chip in the lead is reached by a pointer, and the arrows are the
  // only way a keyboard reaches anything here. Their own group, UNDER the
  // actions and numbered as the answer cites them, so a long list of sources
  // does not push the three writes off the first screen.
  const sourceRows = (drawn: Set<string>): CommandPaletteItem[] => {
    if (!answeredMd) return [];
    return answeredMd.sources.flatMap((s, i) =>
      drawn.has(`${s.kind}:${s.ref}`)
        ? []
        : [
            {
              id: `source-${s.kind}-${s.ref}`,
              icon: s.kind === "task" ? <SearchGlyph size="sm" /> : <FileTextGlyph size="sm" />,
              label:
                s.kind === "task" ? (
                  <>
                    <span className="palette-key">{s.ref}</span> {s.title}
                  </>
                ) : (
                  s.title
                ),
              hint: `source ${i + 1} of the answer`,
              onSelect: () =>
                s.kind === "task" ? nav.to(["work", s.ref]) : openPage(s.ref, s.url ?? ""),
            },
          ],
    );
  };

  const agentGroups = (cap: number, withUnits: boolean): CommandPaletteGroup[] => {
    // THE ENGINE'S TIERS FIRST, in its order, each with why; then the local
    // index's matches it did not list — so a seat the tiers skip (a goal
    // that mentions the term) is still found, below the names that match.
    const byEngine: Seat[] = [];
    const why = new Map<string, string>();
    for (const c of nameAnswer.data?.candidates ?? []) {
      const seat = index.byHandle.get(c.handle);
      if (seat) {
        byEngine.push(seat);
        why.set(seat.handle, c.why);
      }
    }
    const local = matchingSeats(index, lower, Infinity).filter((s) => !why.has(s.handle));
    const seats = [...byEngine, ...local];
    const open = (seat: Seat) => () => nav.to(["agents", "seats", seat.handle]);
    const agentsRows = seats
      .filter((s) => s.kind === "agent")
      .slice(0, cap)
      .map((s) => seatItem(s, seatHint(s, why.get(s.handle)), open(s)));
    const people = seats
      .filter((s) => s.kind === "human")
      .slice(0, cap)
      .map((s) => seatItem(s, seatHint(s, why.get(s.handle)), open(s)));
    const groups: CommandPaletteGroup[] = [
      { id: "agents", label: "Agents", items: agentsRows },
      { id: "people", label: "People", items: people },
    ];
    if (withUnits) {
      groups.push({
        id: "teams",
        label: "Teams",
        items: matchingUnits(index, lower, cap).map((unit) => ({
          id: `unit-${unit.name}`,
          icon: <UsersGlyph size="sm" />,
          label: unit.name,
          hint: `${unit.type || "team"}${unit.lead ? ` · lead ${unit.lead}` : ""}`,
          // BY NAME: a unit's `id:` is part of the guarded configuration,
          // not the public projection, so no link built here carries one.
          onSelect: () => nav.to(["agents", "teams", unit.name]),
        })),
      });
    }
    return groups;
  };

  const projectGroups = (filing: Filing, suggested: ProjectSuggestion): CommandPaletteGroup[] => {
    const q = pickTerm.toLowerCase();
    const choose = (key: string) => () => {
      stay();
      file(filing, key);
    };
    const row = (p: WorkProjectRow, hint: string): CommandPaletteItem => ({
      id: `project-${p.key}`,
      icon: <FolderGlyph size="sm" />,
      label: (
        <>
          <span className="palette-key">{p.key}</span> {p.name || p.key}
        </>
      ),
      hint,
      onSelect: choose(p.key),
    });
    // THE TEAM THAT OWNS IT, where that says something the name does not: a
    // team's project is usually named after the team, and "Executives
    // Executives" is one fact read twice.
    const unitHint = (p: WorkProjectRow) =>
      p.unit?.name && p.unit.name !== p.name ? `owned by ${p.unit.name}` : "";
    const listed = (projects ?? []).filter(
      (p) => !q || p.key.toLowerCase().includes(q) || p.name.toLowerCase().includes(q),
    );
    const suggestion =
      !pickTerm && suggested ? listed.find((p) => p.key === suggested.key) : undefined;
    const groups: CommandPaletteGroup[] = [];
    // THE PROJECT THE TOP TASK HIT LIVES IN leads an untyped step, as the
    // approved palette's "in ENG" beside ENG-420 does — offered, not taken.
    if (suggestion && suggested) {
      groups.push({ id: "suggested", label: "Suggested", items: [row(suggestion, suggested.why)] });
    }
    groups.push({
      id: "projects",
      label: "Projects",
      items: listed.filter((p) => p !== suggestion).map((p) => row(p, unitHint(p))),
    });
    return groups;
  };

  const pickerGroups = (): CommandPaletteGroup[] => {
    if (pick?.kind === "project") return projectGroups(pick.filing, pick.suggested);
    const q = pickTerm.toLowerCase();
    const why = new Map<string, string>();
    const ordered: Seat[] = [];
    for (const c of nameAnswer.data?.candidates ?? []) {
      const seat = index.byHandle.get(c.handle);
      if (seat?.kind === "agent") {
        ordered.push(seat);
        why.set(seat.handle, c.why);
      }
    }
    for (const seat of matchingSeats(index, q, Infinity)) {
      if (seat.kind === "agent" && !why.has(seat.handle)) ordered.push(seat);
    }
    const choose = (seat: Seat) => () => {
      stay();
      if (pick?.kind === "assign") void doAssign(pick.item, seat);
      else if (pick?.kind === "ask") {
        if (pick.project) void doAsk(seat, pick.question, pick.body, pick.project);
        else
          openProjectPick(
            { kind: "ask", seat, question: pick.question, body: pick.body },
            pick.suggested,
          );
      }
    };
    // WHO HOLDS IT NOW, on the assign step: re-assigning a task to its own
    // holder is a no-op the person should see before pressing it.
    const holder = pick?.kind === "assign" ? (itemRead.data?.task.assignee ?? "") : "";
    const hintOf = (seat: Seat) =>
      seatHint(
        seat,
        [why.get(seat.handle), seat.handle === holder ? "holds it" : ""]
          .filter(Boolean)
          .join(" · "),
      );
    const groups: CommandPaletteGroup[] = [];
    // THE SUGGESTION LEADS AN UNTYPED PICKER: the seat the search term named,
    // if it named exactly one — the person still presses it.
    if (!pickTerm && suggestedForPick) {
      groups.push({
        id: "suggested",
        label: "Suggested",
        items: [
          seatItem(
            suggestedForPick.seat,
            seatHint(
              suggestedForPick.seat,
              [suggestedForPick.why, suggestedForPick.seat.handle === holder ? "holds it" : ""]
                .filter(Boolean)
                .join(" · "),
            ),
            choose(suggestedForPick.seat),
          ),
        ],
      });
    }
    groups.push({
      id: "pick-agents",
      label: "Agents",
      items: ordered
        .filter((s) => pickTerm || s.handle !== suggestedForPick?.seat.handle)
        .map((s) => seatItem(s, hintOf(s), choose(s))),
    });
    return groups;
  };

  const groups: CommandPaletteGroup[] = [];
  const push = (id: string, label: string, items: CommandPaletteItem[]) => {
    if (items.length) groups.push({ id, label, items });
  };

  if (picking) {
    groups.push(...pickerGroups().filter((g) => g.items.length));
  } else if (scope === "all") {
    if (!term) {
      push("recent", "Recent", asItems(recentHits(recents, nav)));
      push("go", "Go to", asItems(destinationHits("", nav, Infinity)));
    } else {
      push("ids", "Open by id", asItems(idHits(term, nav)));
      push("go", "Go to", asItems(destinationHits(lower, nav, GO_TO_ROWS)));
      push("tasks", "Tasks", taskRows(allRows));
      push("pages", "Pages", pageRows(allRows));
      for (const g of agentGroups(ALL_ROWS, true)) push(g.id, g.label, g.items);
      push("tools", "Tools", asItems(toolHits(tools, lower, nav, ALL_ROWS)));
      push("actions", "Actions", actionRows());
      const drawn = new Set([
        ...(taskAnswer.data?.hits ?? []).slice(0, allRows).map((h) => `task:${h.key}`),
        ...(pageAnswer.data?.hits ?? []).slice(0, allRows).map((p) => `page:${p.id}`),
      ]);
      push("sources", "Sources of the answer", sourceRows(drawn));
      push("search", "Search", asItems(searchHits(term, nav)));
    }
  } else if (scope === "tasks") {
    const rows = taskRows(TASK_HITS);
    if (taskAnswer.data && rows.length) {
      rows.push({
        id: "task-all",
        icon: <SearchGlyph size="sm" />,
        label: `See every match for “${term}” →`,
        hint: "the full search, with its modes",
        onSelect: () => nav.to(["work", "search"], { q: term }),
      });
    }
    push("tasks", "Tasks", rows);
  } else if (scope === "pages") {
    push("pages", "Pages", pageRows(Infinity));
    const drawn = new Set((pageAnswer.data?.hits ?? []).map((p) => `page:${p.id}`));
    push("sources", "Sources of the answer", sourceRows(drawn));
  } else if (scope === "agents") {
    for (const g of agentGroups(Infinity, true)) push(g.id, g.label, g.items);
  } else {
    // THE COMMANDS FIRST: in this scope the term is usually a command's name
    // ("dark", "keys"), and the writes it would also make — ask, create — are
    // the rows under them rather than the one Enter takes.
    push(
      "commands",
      "Commands",
      asItems(
        commandHits(prefs, lower, {
          // THE WHOLE URL, not the hash: a link is pasted into a message, and
          // a fragment alone resolves against whatever the reader has open.
          copyLink: () => void navigator.clipboard?.writeText(window.location.href),
          setToken: requestToken,
          clearRecents: forgetAll,
          showKeys: onShowKeys,
          hash: route.hash || "#/",
        }),
      ),
    );
    push("actions", "Actions", actionRows());
  }

  // ---- what the list says when it has no rows ------------------------------
  const emptyMessage = (): string => {
    if (pick?.kind === "project") {
      if (projects === null) {
        return projectsRead.error
          ? "The company’s projects could not be read — close this and try again."
          : "Reading the company’s projects…";
      }
      return pickTerm ? `No project matches “${pickTerm}”.` : "This company has no projects.";
    }
    if (picking)
      return pickTerm ? `No agent matches “${pickTerm}”.` : "This company has no agents.";
    if (scope === "tasks") {
      if (term.length < SEARCH_MIN)
        return "Type at least two characters to search the company’s work.";
      if (taskAnswer.error) return "Task search did not run — the note above says why.";
      if (!taskAnswer.data) return "Searching…";
      if (taskAnswer.data.available === false) {
        return "The search index is still building on this node — the work exists, it is not findable from here yet.";
      }
      return `No task matches “${term}”.`;
    }
    if (scope === "pages") {
      if (term.length < SEARCH_MIN)
        return "Type at least two characters to search the knowledge base.";
      if (pageAnswer.error) return "Knowledge search did not run — the note above says why.";
      if (!pageAnswer.data) return "Searching…";
      if (pageAnswer.data.available === false)
        return pageAnswer.data.note || "Knowledge search did not run.";
      return `No page matches “${term}”.`;
    }
    if (scope === "agents") return `Nobody in the chart matches “${term}”.`;
    return `Nothing matches “${term}”.`;
  };

  // ---- the lead --------------------------------------------------------------
  const scopeRefusal =
    scope === "tasks" ? taskAnswer.error : scope === "pages" ? pageAnswer.error : null;
  const lead = picking ? (
    <PickLead
      pick={pick}
      itemTitle={itemRead.data?.task.title}
      itemHolder={
        itemRead.data
          ? itemRead.data.task.assignee
            ? `held by ${index.byHandle.get(itemRead.data.task.assignee)?.name ?? itemRead.data.task.assignee}`
            : "unassigned"
          : undefined
      }
      notice={notice}
      busy={assign.busy || create.busy}
    />
  ) : (
    <>
      {scopeRefusal ? (
        // A SEARCH THAT DID NOT RUN IS NOT ONE THAT FOUND NOTHING, and the one
        // table of what each refusal means says which (`components/common.tsx`)
        // — "the engine does not serve this here" and "the socket went away"
        // both read as "nothing matched" otherwise.
        <QueryState error={scopeRefusal} loading={false} />
      ) : (
        <AnswerLead state={answer} nav={nav} openPage={openPage} />
      )}
      {/* WHAT A PRESS CAME TO, UNDER the answer rather than in its place: an
          answer already paid for is not taken off the screen by a row that
          could not act, or by the engine refusing one that could. */}
      <NoticeLine notice={notice} busy={create.busy} />
    </>
  );

  // ---- the keys only the palette answers -------------------------------------
  const onKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    const native = e.nativeEvent;
    // THE CHORD THAT OPENED IT CLOSES IT. The frame's binding cannot: while
    // this is up every page key stands aside, so the second press reached
    // nobody — and in a browser whose own Ctrl+K focuses the address bar it
    // went there, with the palette still open. Read here, on the surface's
    // own keys, so it never closes this from beneath a dialog raised over it.
    if (matchesRow("palette", native)) {
      e.preventDefault();
      onClose();
      return;
    }
    if (picking) {
      if (matchesRow("search.back", native) && pickQuery === "") {
        e.preventDefault();
        leavePick();
      }
      return;
    }
    const press = (id: string) => {
      const row = groups.flatMap((g) => g.items).find((item) => item.id === id);
      if (!row) return;
      e.preventDefault();
      row.onSelect();
      close();
    };
    if (matchesRow("search.ask", native)) press("act-ask");
    else if (matchesRow("search.assign", native)) press("act-assign");
    else if (matchesRow("search.create", native)) press("act-create");
  };

  const onQueryChange = (value: string) => {
    if (picking) {
      setPickQuery(value);
      return;
    }
    // A SIGIL AT THE START SELECTS ITS TAB and leaves the box — so the tab
    // row, not a character the reader has to know is special, says where
    // they are.
    const { scope: opened, rest } = readSigil(value);
    if (opened) {
      setScope(opened);
      setQuery(rest);
      return;
    }
    setQuery(value);
  };

  const mode = modeWord(taskAnswer.data, pageAnswer.data);

  // A PICKER HAS NO SCOPES: it is one list, of agents, until it is left.
  const scoped: CommandPaletteScopeProps = picking
    ? {}
    : {
        scopes: SCOPES.map((s) => ({ id: s.id, label: s.label })),
        scope,
        onScopeChange: (next: string) => setScope(next as ScopeId),
        groupsScope: scope,
        scopesLabel: "Search in",
      };

  return (
    <KitPalette
      open
      onClose={close}
      label="Search"
      className="palette"
      query={picking ? pickQuery : query}
      onQueryChange={onQueryChange}
      groups={groups}
      placeholder={
        pick?.kind === "assign"
          ? `Assign ${pick.item} to…`
          : pick?.kind === "ask"
            ? "Ask which agent?"
            : pick?.kind === "project"
              ? "File it in which project?"
              : "Search, or ask a question — # tasks, @ agents, > actions"
      }
      escapeHint
      lead={lead}
      emptyMessage={emptyMessage()}
      onKeyDown={onKeyDown}
      {...scoped}
      footer={
        <>
          <span className="palette-keys">
            <span>
              <Kbd subtle>↑</Kbd>
              <Kbd subtle>↓</Kbd> move
            </span>
            <span>
              <Kbd subtle keys={["Enter"]} /> {picking ? "choose" : "open"}
            </span>
            {picking ? (
              <span>
                <Kbd subtle keys={capsOf(keyRow("search.back").presses[0]!)} /> back
              </span>
            ) : (
              // A PHONE DROPS THIS ONE (frame.css): it has no chord keys, and
              // three keys with their words wrapped its band onto two lines.
              <span className="palette-keys-chord">
                <Kbd subtle keys={capsOf(keyRow("search.ask").presses[0]!)} /> ask an agent
              </span>
            )}
          </span>
          {/* THE MODE, where a ranked search is what the scope runs — drawn
              at the END OF THE SCOPE ROW, as the approved palette places it
              (frame.css), since the kit's scope row takes no trailing slot. */}
          {!picking && (scope === "all" || scope === "tasks" || scope === "pages") && (
            <span className="palette-mode" title={mode.title}>
              {mode.degraded ? <CircleAlertGlyph size="xs" /> : <ZapGlyph size="xs" />}
              {mode.word}
            </span>
          )}
          <span className="palette-scope-note">
            <GlobeGlyph size="xs" />{" "}
            {pick?.kind === "project"
              ? "The company’s projects"
              : picking
                ? "The company’s agents"
                : scope === "actions"
                  ? viewer.name
                    ? `Changes are made as ${viewer.name}`
                    : "Changes need a token bound to your seat"
                  : SCOPE_NOTES[scope]}
          </span>
        </>
      }
    />
  );
}

/** What the footer says each scope searches. */
const SCOPE_NOTES: Record<Exclude<ScopeId, "actions">, string> = {
  all: "Screens, tasks, pages, agents",
  tasks: "The company’s tasks",
  pages: "The knowledge base",
  agents: "The org chart",
};

/** The mode the searches on screen were served in, as the design's pill says it. */
function modeWord(...answers: (SearchOutcome | null | undefined)[]): {
  word: string;
  title: string;
  degraded: boolean;
} {
  // THE MODE SERVED, not the mode asked: a company with no embeddings asks
  // hybrid and is answered keyword, and a pill saying "Hybrid search" over a
  // keyword ranking is the claim this footer exists to make honestly.
  for (const a of answers) {
    if (a && a.served_mode && a.served_mode !== a.mode) {
      return {
        word: a.served_mode === "keyword" ? "Keyword search" : "Meaning search",
        title: `Asked for ${a.mode}, served ${a.served_mode}${a.degraded ? ` — ${a.degraded.replace(/_/g, " ")}` : ""}`,
        degraded: true,
      };
    }
  }
  return {
    word: "Hybrid search",
    title: "Words and meaning, fused — the engine's default",
    degraded: false,
  };
}

/** The body of an ask filed from a term the knowledge base already answered. */
function answerBody(answer: KnowledgeAnswerReceipt): string {
  // WHAT THE ASKER ALREADY SAW, so the agent starts where they are rather
  // than repeating the search: the answer, and the sources its [n] cite.
  const sources = answer.sources
    .map((s, i) => `[${i + 1}] ${s.kind === "task" ? `${s.ref} ` : ""}${s.title}`)
    .join("\n");
  return (
    "What the company's knowledge answered when this was asked:\n\n" +
    answer.answer_md +
    (sources ? `\n\nSources:\n${sources}` : "")
  );
}

/**
 * A term's answer from the company's knowledge, or the one line that says
 * what would let this reader have one.
 */
function AnswerLead({
  state,
  nav,
  openPage,
}: {
  state: AnswerState;
  nav: ReturnType<typeof useNavigator>;
  openPage: (id: string, url: string) => void;
}) {
  switch (state.kind) {
    case "none":
    case "waiting":
      return null;
    case "blocked":
      return (
        <p className="palette-answer-note">
          <BookOpenGlyph size="sm" /> {state.reason}
        </p>
      );
    case "asking":
      return (
        <div className="palette-answer" aria-busy="true">
          <AnswerHead />
          <p className="palette-answer-wait">Reading your company’s pages and tasks…</p>
        </div>
      );
    case "refused":
      return (
        <p className="palette-answer-note">
          <BookOpenGlyph size="sm" /> {state.sentence}
        </p>
      );
    case "unknown":
      return (
        <p className="palette-answer-note">
          <BookOpenGlyph size="sm" /> No answer came back — {state.reason}
        </p>
      );
    case "answered": {
      const { answer } = state;
      const spent = answer.tokens.input + answer.tokens.output;
      return (
        <div className="palette-answer">
          <AnswerHead />
          <div className="prose md muted palette-answer-body">
            {renderMarkdown(answer.answer_md)}
          </div>
          {answer.sources.length > 0 && (
            <div className="palette-answer-sources">
              <span className="palette-sources-label">Sources</span>
              {answer.sources.slice(0, SOURCE_CHIPS).map((s, i) => (
                <a
                  key={`${s.kind}-${s.ref}`}
                  className="palette-source"
                  href={s.kind === "task" ? `#/work/${encodeURIComponent(s.ref)}` : undefined}
                  onClick={(e) => {
                    e.preventDefault();
                    if (s.kind === "task") nav.to(["work", s.ref]);
                    else openPage(s.ref, s.url ?? "");
                  }}
                  // NOT A TAB STOP: focus stays in the field, and every source
                  // is also a row the arrows reach.
                  tabIndex={-1}
                >
                  <span className="palette-source-n">{i + 1}</span>
                  <span className="palette-source-text">
                    {s.kind === "task" ? <span className="palette-key">{s.ref}</span> : s.title}
                  </span>
                </a>
              ))}
              {answer.sources.length > SOURCE_CHIPS && (
                <span className="palette-sources-more">
                  +{answer.sources.length - SOURCE_CHIPS}
                  <span className="sr-only"> more, listed under Sources of the answer</span>
                </span>
              )}
            </div>
          )}
          {/* TOKENS, NEVER MONEY — the unit the company's budget counts. */}
          <p className="palette-answer-foot">
            {answer.cached
              ? "Answered before — no tokens spent"
              : `${fmtCount(spent)} tokens, charged to the company`}
            {answer.model ? ` · ${answer.model}` : ""}
          </p>
        </div>
      );
    }
  }
}

function AnswerHead() {
  return (
    <span className="palette-answer-head">
      <BookOpenGlyph size="sm" /> Answer from your company’s knowledge
    </span>
  );
}

/** What a press came to, while it is out or when it was refused. */
function NoticeLine({ notice, busy }: { notice: Notice | null; busy: boolean }) {
  if (busy) return <p className="palette-answer-note">Sending…</p>;
  if (!notice) return null;
  return <p className={noticeClass(notice)}>{notice.text}</p>;
}

/** The picker's head: what is being handed to whom, and what the engine said. */
function PickLead({
  pick,
  itemTitle,
  itemHolder,
  notice,
  busy,
}: {
  pick: Pick | null;
  itemTitle?: string;
  itemHolder?: string;
  notice: Notice | null;
  busy: boolean;
}): ReactNode {
  if (!pick) return null;
  return (
    <div className="palette-pick">
      {pick.kind === "assign" ? (
        <p>
          <span className="palette-key">{pick.item}</span>{" "}
          {itemTitle ? (
            <>
              {itemTitle} <span className="muted">· {itemHolder}</span>
            </>
          ) : (
            <span className="muted">reading…</span>
          )}
        </p>
      ) : pick.kind === "ask" ? (
        <p>
          “{pick.question}”{" "}
          <span className="muted">
            · the agent you choose owes the answer, and it lands in your inbox
          </span>
        </p>
      ) : pick.filing.kind === "ask" ? (
        <p>
          “{pick.filing.question}”{" "}
          <span className="muted">
            · asked of {pick.filing.seat.name} — choose the project the ask is filed in
          </span>
        </p>
      ) : (
        <p>
          “{pick.filing.title}” <span className="muted">· choose the project it is filed in</span>
        </p>
      )}
      {busy ? (
        <p className="palette-answer-note">Sending…</p>
      ) : notice ? (
        <p className={noticeClass(notice)}>{notice.text}</p>
      ) : null}
    </div>
  );
}

/**
 * A query's answer, only while it answers THIS term — see the note where it
 * is used. `enabled` false is no answer at all.
 */
function useSettled<T>(
  state: { data: T | null; error: string | null },
  term: string,
  enabled: boolean,
): { data: T | null; error: string | null } {
  const settled = useRef<{ data: T | null; error: string | null; term: string }>({
    data: null,
    error: null,
    term: "",
  });
  if (settled.current.data !== state.data || settled.current.error !== state.error) {
    settled.current = { data: state.data, error: state.error, term };
  }
  if (!enabled || settled.current.term !== term) return { data: null, error: null };
  return { data: settled.current.data, error: settled.current.error };
}
