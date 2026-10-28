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
 *    pages (three of each), seats and units, the tools the company connected,
 *    a row into the Artifacts scope, and the three actions;
 *    when nothing is typed, the turns running now and then the recents;
 *  - TASKS — `work_search`, hybrid, eight hits, with its four answers kept
 *    apart (too short, searching, refused, nothing matched) and a row to the
 *    full search screen;
 *  - PAGES — `knowledge`, the company's one knowledge backend;
 *  - ARTIFACTS: `artifacts` with the term, everything the reader can open,
 *    eight hits; All offers a row that carries the term here rather than
 *    asking a fourth question itself;
 *  - AGENTS — the org chart with each seat's status from the agents push, the
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
 * Three rows change the company, each through `useAct` AS THE SIGNED-IN
 * PRINCIPAL (ADR-0024), the seat the directory binds them to where it binds
 * one, their own login where it does not. Each files or moves a task in
 * Worklet, so where the company runs none of it none of the three is
 * offered (`lib/services.ts`); elsewhere each is drawn for every reader and
 * says why where it cannot act:
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

import {
  useDeferredValue,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import {
  ArtifactKindGlyph,
  CommandPalette as KitPalette,
  Kbd,
  KeyChip,
  ThemeSwatch,
  type CommandPaletteGroup,
  type CommandPaletteItem,
  type CommandPaletteScopeProps,
  useToast,
} from "@crewlethq/ui";
import { isThemeId, themeGroups, themeRegistry } from "@crewlethq/tokens";
import {
  BookOpenGlyph,
  CircleAlertGlyph,
  Columns3Glyph,
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
import { ProductMark } from "@crewlethq/icons/product-mark";
import { useAssistantControl } from "../assistant/control.ts";
import { capsOf, keyRow, matchesRow } from "../keymap.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { DOCLET_SEARCH_BACKEND, useSearchQuery } from "~/lib/useSearchQuery.ts";
import { useAct } from "~/lib/useAct.ts";
import { useRecents, forgetAll } from "~/lib/recents.ts";
import { previewPalette, useViewerPrefs } from "~/lib/prefs.ts";
import { useKeyHints } from "~/app/layout.ts";
import { themeEntry, useApplyTheme } from "../appearance.tsx";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { useAgents, useConnection, useOrg, useTools } from "~/lib/store-hooks.ts";
import {
  activityOf,
  handleLabel,
  indexOrg,
  labelOf,
  liveRowFor,
  nameOfIn,
  statusOf,
  stateLine,
  unitRoute,
  workingLongestFirst,
  type Seat,
} from "~/lib/seats.ts";
import { turnIdOf, watchLink } from "~/lib/turns.ts";
import { itemPath, PROJECT_PAGE, projectPath, statusLabel, unfinished } from "~/lib/work.ts";
import { rankProjects } from "~/lib/projectRank.ts";
import { useOpenProjectSwitcher } from "../header/ProjectSwitcher.tsx";
import { fmtCount } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { goSignIn } from "~/lib/session.ts";
import { copyPageLink } from "~/lib/pageLink.ts";
import type { ActResult } from "~/protocol/act.ts";
import type { LogRefusal, QueryRefusal, SearchOutcome, WorkProjectRow } from "~/protocol/index.ts";
import { SEARCH_MODES, modeLabel, searchTooLong, servedNote } from "~/lib/search.ts";
import { PriorityMark, StatusMark } from "~/components/work.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { KIND_LIST } from "~/lib/artifacts/words.ts";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { QueryState } from "~/components/common.tsx";
import {
  ALL_ROWS,
  ALL_ROWS_UNDER_ANSWER,
  GO_TO_ROWS,
  SCOPES,
  SOURCE_CHIPS,
  ARTIFACT_HITS,
  artifactHits,
  SEARCH_MIN,
  TASK_HITS,
  colleagueSendable,
  commandHits,
  destinationGroups,
  destinationHits,
  idHits,
  matchingSeats,
  matchingUnits,
  readSigil,
  recentHits,
  routeProject,
  routeTask,
  score,
  searchHits,
  switchProjectHit,
  toolHits,
  type Hit,
  type ScopeId,
  type Standing,
} from "./hits.ts";
import { useKnowledgeAnswer, type AnswerState, type KnowledgeAnswerReceipt } from "./answer.ts";
import { useServicesRun, useServicesAsk } from "~/lib/services.ts";
import { LimitRemedy } from "~/components/LimitRemedy.tsx";

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
  const assistant = useAssistantControl();
  // WHO THE LOCKS ARE READ FOR, or null while nobody has said — a lock
  // claimed on a guess is a refusal nobody made.
  const standing: Standing = viewer.loading
    ? null
    : viewer.anonymous
      ? { anonymous: true }
      : { anonymous: false, grants: viewer.grants };
  const prefs = useViewerPrefs();
  // THE KEYCAPS ARE FOR A READER WITH KEYS (`useKeyHints`): on a phone the
  // rows' chords and the footer's legend named keys nobody there has.
  const keyHints = useKeyHints();
  const recents = useRecents();
  const agents = useAgents();
  const org = useOrg();
  const tools = useTools();
  const toast = useToast();
  const index = useMemo(() => indexOrg(org), [org]);
  // A BROWSER THE ENGINE REFUSED ASKS IT NOTHING. Its questions would wait on a socket nobody dials, drawn as
  // searches that never finish; the seats and tools a refusal dropped are not
  // here to list (`Store.setAccessRefused`); and the writes say why they are
  // held, through the gate every write reads.
  const served = useConnection().accessRefused === null;
  // WHICH OF CREWLET'S OWN SERVICES THE COMPANY RUNS: a search, a scope, a
  // destination or a write into one it does not is not offered at all.
  const runs = useServicesRun();
  const ask = useServicesAsk();

  // THE RUNNING SET IS READ ONCE, AS THE PALETTE OPENS — which turns Running
  // now offers and in what order. The kit's listbox holds the highlight by
  // INDEX, so a group that gained or lost a row on an agents push (twice a
  // tool-loop round) moved every row under it, and Enter committed a row
  // nobody highlighted. Opening is this surface's re-sort boundary: it is
  // mounted only while open (`app/Shell.tsx`), so a fresh ⌘K reads afresh.
  // Later pushes reach only each row's words.
  const [running] = useState(() =>
    workingLongestFirst(agents).flatMap((row) => {
      const turnId = turnIdOf(row);
      return turnId ? [{ id: row.id, turnId }] : [];
    }),
  );

  const [scope, setScope] = useState<ScopeId>("all");
  const [query, setQuery] = useState("");
  const [pick, setPick] = useState<Pick | null>(null);
  const [pickQuery, setPickQuery] = useState("");
  // THE THEME STEP, the kit's own step: a list of the themes in place of the
  // results, whose highlight previews behind the palette and whose Enter
  // applies (see `app/appearance.tsx`).
  const [themeStep, setThemeStep] = useState(false);
  const [themeQuery, setThemeQuery] = useState("");
  const applyTheme = useApplyTheme();
  const openThemeStep = () => {
    setThemeStep(true);
    setThemeQuery("");
  };
  const leaveThemeStep = () => {
    setThemeStep(false);
    setThemeQuery("");
  };
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
  // THE ENGINE'S QUESTIONS READ THE TERM DEFERRED. Asked with the term as
  // typed, every keystroke changed each search's parameters, so the render a
  // keystroke commits ran each search's effect, and each set its state
  // again. Under keystrokes arriving back to back, React counted those
  // updates towards its nested-update limit and threw a keystroke away
  // (error #185). Deferred, a keystroke's own render leaves every search as
  // it was; React asks again in a render of its own, which the next keystroke
  // interrupts rather than piles onto. What is drawn from the chart and the
  // screens still reads the term as typed.
  const searchTerm = useDeferredValue(term);

  // ---- the three questions ---------------------------------------------
  // A TERM PAST THE ENGINE'S SEARCH BOUND ASKS NEITHER SEARCH NOR THE ANSWER,
  // and the lead says why (`searchTooLong`): sent, both would be refused, and
  // a list that silently lost its tasks and pages reads as nothing matching.
  const searchable =
    (scope === "all" || scope === "tasks" || scope === "pages" || scope === "artifacts") &&
    !picking;
  const tooLong = searchable ? searchTooLong(term) : null;
  const askable = searchable && searchTooLong(searchTerm) === null;
  const wantsTasks =
    served &&
    runs.tracker &&
    !picking &&
    (scope === "all" || scope === "tasks") &&
    searchTerm.length >= SEARCH_MIN &&
    askable;
  const tasks = useSearchQuery(
    "work_search",
    { q: searchTerm, mode: "hybrid", limit: TASK_HITS },
    { enabled: wantsTasks },
  );
  const wantsPages =
    served &&
    runs.knowledge &&
    !picking &&
    (scope === "all" || scope === "pages") &&
    searchTerm.length >= SEARCH_MIN &&
    askable;
  const pages = useSearchQuery("knowledge", { q: searchTerm }, { enabled: wantsPages });
  // ARTIFACTS IN THEIR OWN SCOPE ONLY, so All still asks three questions. The
  // listing is open to every signed-in reader and the engine decides each row.
  const wantsArtifacts =
    served && !picking && scope === "artifacts" && searchTerm.length >= SEARCH_MIN && askable;
  const artifacts = useQuery(
    "artifacts",
    { scope: "all", q: searchTerm, sort: "updated", "max-keys": ARTIFACT_HITS },
    { enabled: wantsArtifacts },
  );
  const pickingAgent = pick?.kind === "assign" || pick?.kind === "ask";
  const nameTerm = useDeferredValue(picking ? pickTerm : term);
  const wantsNames =
    served &&
    (pickingAgent || (!picking && (scope === "all" || scope === "agents"))) &&
    colleagueSendable(nameTerm);
  const names = useQuery("colleague", { q: nameTerm }, { enabled: wantsNames });
  // THE COMPANY'S PROJECTS, once: to name the project a create lands in, and
  // to offer them when nothing on screen or in the chart says which.
  // A WORKLET QUESTION, asked only of a reader holding Worklet: without it the
  // engine refuses the list, and nothing a reader without it can do here
  // files a task anyway.
  const projectsRead = useQuery(
    "work_projects",
    { limit: PROJECT_PAGE },
    { enabled: served && runs.tracker && viewer.worklet },
  );
  const projects: WorkProjectRow[] | null = projectsRead.data?.projects ?? null;
  const projectName = (key: string) => projects?.find((p) => p.key === key)?.name ?? "";
  // THE SWITCHER, which "Switch project…" opens over whatever the palette
  // leaves behind it as it closes.
  const openSwitcher = useOpenProjectSwitcher();
  // NOT OFFERED WHERE THE ENGINE SERVES NO COMPANY, NOR TO A READER WITHOUT
  // WORKLET: the switcher lists projects, which is Worklet's question.
  const switchHits =
    served && runs.tracker && viewer.worklet ? [switchProjectHit(openSwitcher)] : [];

  // WHICH TERM EACH ANSWER ON HAND IS FOR. `useQuery` keeps the previous
  // answer across a change of parameters — right for a polled table, wrong
  // for a search box, where it listed "auth"'s items as hits for "authz" with
  // nothing saying a search was in flight. The answer carries no echo of its
  // question, so the term is remembered against the answer's own identity:
  // a new object is this term's answer arriving, and nothing else.
  const taskAnswer = useSettled(tasks, searchTerm, wantsTasks);
  const pageAnswer = useSettled(pages, searchTerm, wantsPages);
  const artifactAnswer = useSettled(artifacts, searchTerm, wantsArtifacts);
  const nameAnswer = useSettled(names, nameTerm, wantsNames);

  // ---- the answer --------------------------------------------------------
  const answer = useKnowledgeAnswer(
    term,
    runs.knowledge && !picking && (scope === "all" || scope === "pages"),
  );
  // A QUESTION THIS READER WILL BE ANSWERED leads the list with a card, and
  // All makes room for the actions under it (`ALL_ROWS_UNDER_ANSWER`).
  const answering =
    answer.kind === "waiting" || answer.kind === "asking" || answer.kind === "answered";
  const allRows = answering ? ALL_ROWS_UNDER_ANSWER : ALL_ROWS;

  // ---- the writes --------------------------------------------------------
  const assign = useAct("update_work_item");
  const create = useAct("create_work_item");
  const pickItem = pick?.kind === "assign" ? pick.item : "";
  const itemRead = useQuery(
    "work_item",
    { id: pickItem },
    { enabled: pickItem !== "" && ask.tracker && viewer.worklet },
  );

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
    editPickQuery("");
  };
  /** The project step, for a filing that has everything but where. */
  const openProjectPick = (filing: Filing, suggestion: ProjectSuggestion) => {
    setPick({ kind: "project", filing, suggested: suggestion });
    editPickQuery("");
  };
  const leavePick = () => {
    setPick(null);
    setSuggestedForPick(null);
  };

  // A FRESH KEYSTROKE CLEARS WHAT THE LAST PRESS SAID, in the same event
  // that changes the term, the picker's term or the scope. An effect keyed on
  // them set state once more after every keystroke, which is the cascade the
  // deferred term above exists to prevent.
  const editQuery = (next: string) => {
    setNotice(null);
    setQuery(next);
  };
  const editPickQuery = (next: string) => {
    setNotice(null);
    setPickQuery(next);
  };
  const editScope = (next: ScopeId) => {
    setNotice(null);
    setScope(next);
  };

  // PAIRED BY THE SEAT'S OWN ROW, never by its name: two seats may share a
  // name, and a seat's ring read off its namesake's row is a state it is not in.
  const liveOf = (seat: Seat) => liveRowFor(agents, seat);

  const seatItem = (seat: Seat, hint: string, onSelect: () => void): CommandPaletteItem => {
    const state = activityOf(liveOf(seat));
    return {
      id: `seat-${seat.handle}`,
      icon: (
        <SeatAvatar
          name={seat.name}
          kind={seat.kind}
          avatar={seat.avatar}
          size="xs"
          status={seat.kind === "agent" ? statusOf(state) : undefined}
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
      setNotice({ refused: false, text: `Loading ${item}. Pick again in a moment.` });
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
    // it. The route change closes the palette. An unknown outcome has no task
    // to go to, and its toast is what says so. AT THE ADDRESS THE RECEIPT
    // NAMES (`item`), never its key: a key a restored counter handed out again
    // is one another task already claimed, and it opens that task.
    const address =
      result.kind === "unknown" ? undefined : (result.receipt as { item?: string } | null)?.item;
    if (address) nav.to(["worklet", address]);
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
        title={why ? `${key}: ${why}` : undefined}
      >{`in ${key}${name ? ` · ${name}` : ""}`}</span>
    );
  };

  // --- assistant page actions (WP12) ---
  // THE QUESTION, ASKED OF THE ASSISTANT, beside the page it was typed on, as
  // the LAST answer for any typed text (05-ux-design, "Keyboard"): a row the
  // reader goes to, never the one Enter takes in place of a match. Offered
  // only where this reader may ask, and asked as the panel would ask it
  // (`control.ts`).
  const assistantRows = (): CommandPaletteItem[] =>
    term && !picking && (scope === "all" || scope === "actions") && assistant.unavailable === null
      ? [
          {
            id: "act-assistant",
            icon: <ProductMark product="assistant" size="sm" />,
            label: `Ask the assistant: “${term}”`,
            hint: "answers in the panel, with this page attached",
            onSelect: () => assistant.ask(term),
          },
        ]
      : [];
  // --- end assistant page actions (WP12) ---

  // ---- the three action rows -----------------------------------------------
  const answeredMd = answer.kind === "answered" ? answer.answer : null;

  const actionRows = (): CommandPaletteItem[] => {
    if (picking) return [];
    const rows: CommandPaletteItem[] = [];
    if (!runs.tracker) return rows;
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
            ? `suggested: ${suggested.seat.name}, ${suggested.why}`
            : "choose who",
        meta: keyHints ? (
          <Kbd subtle keys={capsOf(keyRow("search.assign").presses[0]!)} />
        ) : undefined,
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
          ? "No projects yet. Add one in Worklet."
          : "";
      const body = answeredMd ? answerBody(answeredMd) : "";
      const choose = hitProject
        ? `choose a project, ${hitProject.key} suggested`
        : "choose a project";
      rows.push({
        id: "act-ask",
        icon: <MessageSquareGlyph size="sm" />,
        label: suggested
          ? `Ask ${suggested.seat.name} about “${term}”`
          : `Ask an agent about “${term}”…`,
        hint: why || "opens an ask, answer lands in your inbox",
        meta: keyHints ? <Kbd subtle keys={capsOf(keyRow("search.ask").presses[0]!)} /> : undefined,
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
        meta: keyHints ? (
          <Kbd subtle keys={capsOf(keyRow("search.create").presses[0]!)} />
        ) : undefined,
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
      icon: h.kind ? (
        <ArtifactKindGlyph kind={h.kind} size="sm" />
      ) : (
        <NamedGlyph name={h.icon} size="sm" />
      ),
      label: h.label,
      ...(h.hint ? { hint: h.hint } : {}),
      ...(h.meta ? { meta: h.meta } : {}),
      onSelect: h.go,
    }));

  /**
   * THE TURNS RUNNING AS THE PALETTE OPENED, under Recent in an empty
   * palette: one row per seat the engine said was working, oldest first as
   * every list of them is, each going to the turn's watch link — its
   * Transcript, the phase it is on open. A seat whose turn had no id yet is
   * left out rather than offered as a way to nowhere.
   *
   * UNDER RECENT, NOT ABOVE IT: "⌘K, Enter" is the reader's way back to where
   * they just were, and a group above Recent would make that habit open a
   * running turn instead whenever the company happens to be working.
   *
   * A TURN THAT ENDS WHILE THE PALETTE IS OPEN KEEPS ITS ROW (see `running`
   * above), still opening that turn, and says it finished — the row a reader
   * is reaching for must not leave under the pointer.
   */
  const runningRows = (): CommandPaletteItem[] =>
    running.map(({ id, turnId }) => {
      const row = agents.find((a) => a.id === id) ?? null;
      const seat = index.byHandle.get(row?.handle ?? "") ?? null;
      const name = seat?.name ?? row?.role ?? id;
      const watch = watchLink(turnId);
      return {
        id: `running-${id}`,
        icon: (
          <SeatAvatar
            name={name}
            kind="agent"
            avatar={seat?.avatar}
            size="xs"
            status={statusOf(activityOf(row))}
            decorative
          />
        ),
        label: name,
        // WHAT IT IS DOING AND ON WHAT, in the words every running-turn row
        // says ("Executing ENG-412", "Coding run on ENG-9") — while the seat
        // is still on the turn the row was read for.
        hint:
          row && turnIdOf(row) === turnId ? stateLine(row, { now: Date.now(), seat }) : "Finished",
        onSelect: () => nav.to(watch.path, watch.query),
      };
    });

  const taskRows = (cap: number): CommandPaletteItem[] => {
    const hits = taskAnswer.data?.hits ?? [];
    return hits.slice(0, cap).map((item) => ({
      // THE ITEM'S ID, which is what the list keys an option on: a key is an
      // ADDRESS two items can hold at once (`key_collision`), and a search that
      // finds both would draw two options under one key.
      id: `task-${item.id}`,
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
      // AND IT GOES TO THE ITEM'S ADDRESS for the same reason: the key opens
      // whichever of the two claimed it first.
      onSelect: () => nav.to(itemPath(item)),
    }));
  };

  // THE COMPANY'S PROJECTS, ranked as the switcher ranks them
  // (`lib/projectRank.ts`), each going to its page.
  const projectRows = (cap: number): CommandPaletteItem[] =>
    rankProjects(projects ?? [], term)
      .slice(0, cap)
      .map((p) => ({
        id: `project-${p.key}`,
        icon: <Columns3Glyph size="sm" />,
        label: (
          <>
            <KeyChip highlight={term}>{p.key}</KeyChip> {p.name || p.key}
          </>
        ),
        hint: p.task_counts ? `${unfinished(p.task_counts)} open` : undefined,
        onSelect: () => nav.to(projectPath(p.key)),
      }));

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
    if (url && /^https?:/.test(url) && pageAnswer.data?.backend !== DOCLET_SEARCH_BACKEND) {
      window.open(url, "_blank", "noopener");
    } else nav.to(["doclet", "pages", id]);
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
                s.kind === "task" ? nav.to(["worklet", s.ref]) : openPage(s.ref, s.url ?? ""),
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
    const open = (seat: Seat) => () => nav.to(["company", "org", "seats", seat.handle]);
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
          // BY KEY, which is what tells two units sharing a name apart — as a
          // row id, in the hint a reader picks between them by, and in the
          // route. Keyed on the name, the second such unit was a duplicate row
          // id and both rows opened the first one's page.
          id: `unit-${unit.id}`,
          icon: <UsersGlyph size="sm" />,
          label: unit.name,
          hint: `${unit.type || "team"} · ${unit.id}${unit.lead ? ` · lead ${unit.lead}` : ""}`,
          onSelect: () => nav.to(unitRoute(unit)),
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

  /** The themes, in their groups, narrowed by the step's query: a name, a tag or a group. */
  const themeRows = (): CommandPaletteGroup[] => {
    const q = themeQuery.trim().toLowerCase();
    return themeGroups
      .map((group) => ({
        id: `themes-${group.id}`,
        label: group.name,
        items: themeRegistry
          .filter((theme) => theme.group === group.id)
          .filter(
            (theme) =>
              !q ||
              [theme.name, theme.tag, group.name].some((word) => word.toLowerCase().includes(q)),
          )
          .map((theme): CommandPaletteItem => ({
            id: `${THEME_ROW}${theme.id}`,
            icon: <ThemeSwatch swatch={theme.swatch} size="chip" />,
            label: theme.name,
            hint: theme.tag,
            meta: theme.id === prefs.palette ? "Current" : undefined,
            onSelect: () => applyTheme(theme.id),
          })),
      }))
      .filter((group) => group.items.length > 0);
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

  if (themeStep) {
    groups.push(...themeRows());
  } else if (picking) {
    groups.push(...pickerGroups().filter((g) => g.items.length));
  } else if (scope === "all") {
    if (!term) {
      push("recent", "Recent", asItems(recentHits(recents, nav, runs)));
      push("running", "Running now", runningRows());
      // EVERY PLACE, UNDER THE SIDEBAR'S OWN HEADINGS ([destinationGroups]),
      // and Switch project with Worklet's, where it belongs.
      for (const g of destinationGroups(nav, standing, runs)) {
        const extra = g.place === "workspace" ? switchHits : [];
        push(`go-${g.place}`, g.label, asItems([...g.hits, ...extra]));
      }
    } else {
      push("ids", "Open by id", asItems(idHits(term, nav)));
      push(
        "go",
        "Go to",
        asItems([
          ...destinationHits(lower, nav, GO_TO_ROWS, standing, runs),
          ...switchHits.filter((hit) => score(hit.label, lower) >= 0),
        ]),
      );
      push("projects", "Projects", projectRows(allRows));
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
      push("search", "Search", asItems(searchHits(term, nav, runs)));
      // A SCOPE TO GO ON IN, NOT A MATCH: with nothing found above it, a row
      // first in the list is the one Enter takes, and Enter on a question then
      // left the reader in the artifacts scope rather than at an answer.
      if (term.length >= SEARCH_MIN && tooLong === null) {
        push("artifacts", "Artifacts", [
          {
            id: "artifact-scope",
            icon: <SearchGlyph size="sm" />,
            label: `Find “${term}” in artifacts →`,
            hint: KIND_LIST,
            onSelect: () => {
              editScope("artifacts");
              return false;
            },
          },
        ]);
      }
      push("assistant", "Assistant", assistantRows());
    }
  } else if (scope === "tasks") {
    const rows = taskRows(TASK_HITS);
    if (taskAnswer.data && rows.length) {
      rows.push({
        id: "task-all",
        icon: <SearchGlyph size="sm" />,
        label: `See every match for “${term}” →`,
        hint: "the full search, with its modes",
        onSelect: () => nav.to(["worklet", "search"], { q: term }),
      });
    }
    push("tasks", "Tasks", rows);
  } else if (scope === "pages") {
    push("pages", "Pages", pageRows(Infinity));
    const drawn = new Set((pageAnswer.data?.hits ?? []).map((p) => `page:${p.id}`));
    push("sources", "Sources of the answer", sourceRows(drawn));
  } else if (scope === "artifacts") {
    push("artifacts", "Artifacts", asItems(artifactHits(artifactAnswer.data?.contents ?? [], nav)));
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
          // WHAT THE BROWSER DID IS SAID, in a toast because the palette
          // closes on the press: see `lib/pageLink.ts`.
          copyLink: () =>
            void copyPageLink().then((word) =>
              word === "Copied"
                ? toast.ok("Copied the link to this page")
                : toast.show({
                    variant: "warning",
                    title: "The browser refused the clipboard",
                    message: "Copy the address from the address bar instead.",
                  }),
            ),
          signIn: goSignIn,
          clearRecents: forgetAll,
          showKeys: onShowKeys,
          changeTheme: openThemeStep,
          themeName: themeEntry(prefs.palette).name,
          switchProject: served && runs.tracker && viewer.worklet ? openSwitcher : null,
          hash: route.hash || "#/",
        }),
      ),
    );
    push("actions", "Actions", actionRows());
    push("assistant", "Assistant", assistantRows());
  }

  // ---- what the list says when it has no rows ------------------------------
  const emptyMessage = (): string => {
    if (themeStep) return "No theme matches.";
    if (pick?.kind === "project") {
      if (projects === null) {
        return projectsRead.error
          ? "Couldn’t load projects. Try again."
          : "Reading the company’s projects…";
      }
      return pickTerm ? `No project matches “${pickTerm}”.` : "This company has no projects.";
    }
    if (picking)
      return pickTerm ? `No agent matches “${pickTerm}”.` : "This company has no agents.";
    if (scope === "tasks") {
      // WITHHELD FIRST: no term a reader without Worklet types finds a task.
      if (tasks.withheld) return tasks.withheld;
      if (term.length < SEARCH_MIN) return "Type two or more characters.";
      if (tooLong) return tooLong;
      if (taskAnswer.error) return "Task search did not run.";
      if (!taskAnswer.data) return "Searching…";
      if (taskAnswer.data.available === false) {
        return "Still indexing. Try again shortly.";
      }
      return `No task matches “${term}”.`;
    }
    if (scope === "pages") {
      if (pages.withheld) return pages.withheld;
      if (term.length < SEARCH_MIN) return "Type two or more characters.";
      if (tooLong) return tooLong;
      if (pageAnswer.error) return "Page search did not run.";
      if (!pageAnswer.data) return "Searching…";
      if (pageAnswer.data.available === false)
        return pageAnswer.data.note || "Page search did not run.";
      return `No page matches “${term}”.`;
    }
    if (scope === "artifacts") {
      if (term.length < SEARCH_MIN) return "Type two or more characters.";
      if (tooLong) return tooLong;
      if (artifactAnswer.error) return "Artifact search did not run.";
      if (!artifactAnswer.data) return "Searching…";
      return `No artifact you can open matches “${term}”.`;
    }
    if (scope === "agents") return `Nobody in the chart matches “${term}”.`;
    return `Nothing matches “${term}”.`;
  };

  // ---- the lead --------------------------------------------------------------
  const scopeRead = scope === "tasks" ? taskAnswer : scope === "pages" ? pageAnswer : null;
  const scopeRefusal = scopeRead?.error ?? null;
  const lead = themeStep ? null : picking ? (
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
        <QueryState
          error={scopeRefusal}
          refusal={scopeRead?.refusal ?? null}
          detail={scopeRead?.detail ?? undefined}
          loading={false}
        />
      ) : tooLong ? (
        // NOT SENT, and said where an answer would be: All still lists the
        // screens and actions the term matches, and nothing else on the
        // surface would say why its tasks and pages are missing.
        <p className="palette-answer-note">{tooLong}</p>
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
    // THE THEME STEP'S KEYS ARE THE KIT'S: Escape and an empty Backspace go
    // back to the commands, and nothing here may also act on them.
    if (themeStep) return;
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
    if (themeStep) {
      setThemeQuery(value);
      return;
    }
    if (picking) {
      editPickQuery(value);
      return;
    }
    // A SIGIL AT THE START SELECTS ITS TAB and leaves the box — so the tab
    // row, not a character the reader has to know is special, says where
    // they are.
    const { scope: opened, rest } = readSigil(value);
    if (opened) {
      editScope(opened);
      editQuery(rest);
      return;
    }
    editQuery(value);
  };

  const mode = modeWord(taskAnswer.data, pageAnswer.data);

  // A PICKER HAS NO SCOPES: it is one list, of agents, until it is left. Nor
  // does the theme step, which is one list of themes.
  const scoped: CommandPaletteScopeProps =
    picking || themeStep
      ? {}
      : {
          scopes: SCOPES.filter((s) => !("service" in s) || runs[s.service]).map((s) => ({
            id: s.id,
            label: s.label,
          })),
          scope,
          onScopeChange: (next: string) => editScope(next as ScopeId),
          groupsScope: scope,
          scopesLabel: "Search in",
        };

  return (
    <KitPalette
      open
      onClose={close}
      label="Search"
      className="palette"
      query={themeStep ? themeQuery : picking ? pickQuery : query}
      onQueryChange={onQueryChange}
      groups={groups}
      step={
        themeStep
          ? {
              label: "Change theme",
              onBack: leaveThemeStep,
              // Opened on the theme in effect, so nothing repaints until the
              // reader moves.
              startAt: `${THEME_ROW}${prefs.palette}`,
            }
          : undefined
      }
      // THE HIGHLIGHT PREVIEWS, in the theme step and nowhere else: the
      // whole frame takes the theme the highlight rests on, and the palette
      // closing, the step being left or the pointer leaving the list puts
      // back the theme in effect.
      onHighlight={(item) => previewPalette(themeStep ? themeOfRow(item?.id) : null)}
      placeholder={
        themeStep
          ? `Search ${themeRegistry.length} themes`
          : pick?.kind === "assign"
            ? `Assign ${pick.item} to…`
            : pick?.kind === "ask"
              ? "Ask which agent?"
              : pick?.kind === "project"
                ? "File it in which project?"
                : "Search, or ask a question: # tasks, @ agents, > actions"
      }
      escapeHint
      lead={lead}
      emptyMessage={emptyMessage()}
      onKeyDown={onKeyDown}
      {...scoped}
      footer={
        <>
          {keyHints && (
            <span className="palette-keys">
              <span>
                <Kbd subtle>↑</Kbd>
                <Kbd subtle>↓</Kbd> {themeStep ? "preview" : "move"}
              </span>
              <span>
                <Kbd subtle keys={["Enter"]} /> {themeStep ? "apply" : picking ? "choose" : "open"}
              </span>
              {themeStep ? (
                <span>
                  <Kbd subtle keys={["Escape"]} /> back to Actions
                </span>
              ) : picking ? (
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
          )}
          {/* THE MODE, where a ranked search is what the scope runs — drawn
              at the END OF THE SCOPE ROW, as the approved palette places it
              (frame.css), since the kit's scope row takes no trailing slot. */}
          {!picking &&
            !themeStep &&
            (scope === "all" || scope === "tasks" || scope === "pages") && (
              <span className="palette-mode" title={mode.title}>
                {mode.degraded ? <CircleAlertGlyph size="xs" /> : <ZapGlyph size="xs" />}
                {mode.word}
              </span>
            )}
          <span className="palette-scope-note">
            <GlobeGlyph size="xs" />{" "}
            {themeStep
              ? "This browser only"
              : pick?.kind === "project"
                ? "The company’s projects"
                : picking
                  ? "The company’s agents"
                  : scope === "actions"
                    ? actingAs(viewer)
                    : SCOPE_NOTES[scope]}
          </span>
        </>
      }
    />
  );
}

/**
 * Who the Actions scope's changes are made as: the signed-in principal — the
 * seat's name where the directory binds them to one, their login where it
 * binds none, since an unbound principal acts under their own login — or the
 * sign-in a reader nobody resolved needs first.
 */
function actingAs(viewer: ViewerState): string {
  if (viewer.loading) return "Checking who you are";
  if (viewer.anonymous) return "Changes need you signed in";
  return `Changes are made as ${viewer.name || viewer.login}`;
}

/** The prefix of a theme row's id in the theme step: the rest is the theme's id. */
const THEME_ROW = "theme-";

/** The theme a theme row names, or null for any other row. */
function themeOfRow(id: string | undefined) {
  const theme = id?.startsWith(THEME_ROW) ? id.slice(THEME_ROW.length) : "";
  return isThemeId(theme) ? theme : null;
}

/** What the footer says each scope searches. */
const SCOPE_NOTES: Record<Exclude<ScopeId, "actions">, string> = {
  all: "Screens, tasks, pages, agents",
  tasks: "The company’s tasks",
  pages: "The knowledge base",
  artifacts: "Every artifact you can open",
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
        word: `${modeLabel(a.served_mode)} search`,
        // THE SEARCH SCREENS' OWN SENTENCE ([servedNote]), so the pill and
        // the note above a ranking say one thing in one vocabulary — "Asked
        // for Hybrid, served Meaning" — never the wire's `semantic`.
        title: servedNote(a) ?? `Served ${modeLabel(a.served_mode)}`,
        degraded: true,
      };
    }
  }
  return {
    word: `${modeLabel("hybrid")} search`,
    title: SEARCH_MODES[0]!.hint,
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
        <div className="palette-answer">
          <p className="palette-answer-note">
            <BookOpenGlyph size="sm" /> {state.sentence}
          </p>
          <LimitRemedy remedy={state.remedy} />
        </div>
      );
    case "unknown":
      return (
        <p className="palette-answer-note">
          <BookOpenGlyph size="sm" /> No answer: {state.reason}
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
                  href={s.kind === "task" ? `#/worklet/${encodeURIComponent(s.ref)}` : undefined}
                  onClick={(e) => {
                    e.preventDefault();
                    if (s.kind === "task") nav.to(["worklet", s.ref]);
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
              ? "Cached answer, no tokens spent"
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
          “{pick.question}” <span className="muted">· the answer lands in your inbox</span>
        </p>
      ) : pick.filing.kind === "ask" ? (
        <p>
          “{pick.filing.question}”{" "}
          <span className="muted">· asked of {pick.filing.seat.name}. Choose a project.</span>
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
  state: {
    data: T | null;
    error: string | null;
    refusal?: QueryRefusal | LogRefusal | null;
    detail?: string | null;
  },
  term: string,
  enabled: boolean,
): Settled<T> {
  const settled = useRef<Settled<T> & { term: string }>({
    data: null,
    error: null,
    refusal: null,
    detail: null,
    term: "",
  });
  if (settled.current.data !== state.data || settled.current.error !== state.error) {
    // THE REFUSAL WITH ITS ERROR, from the same answer: the banner names the
    // grant that would have admitted the reader, and a refusal kept from an
    // older answer would name the wrong one.
    settled.current = {
      data: state.data,
      error: state.error,
      refusal: state.refusal ?? null,
      detail: state.detail ?? null,
      term,
    };
  }
  if (!enabled || settled.current.term !== term) {
    return { data: null, error: null, refusal: null, detail: null };
  }
  const { data, error, refusal, detail } = settled.current;
  return { data, error, refusal, detail };
}

/** What a settled search answered, refusal and all. */
interface Settled<T> {
  data: T | null;
  error: string | null;
  refusal: QueryRefusal | LogRefusal | null;
  detail: string | null;
}
