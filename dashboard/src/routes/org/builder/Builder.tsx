/**
 * The Builder lens of the Org chart screen (`#/company?lens=builder`): editing the
 * organization, and creating the company where none exists.
 *
 * THE POSTURE IS WHAT THE ENGINE ANSWERS, never what the browser holds. A
 * stored token proves nothing (an engine with `api.auth.disabled` needs none,
 * and a rotated one is refused), so `GET /config` is read on mount and again
 * whenever the operator token changes, and its answer decides:
 *
 * | `GET /config` answers                            | The lens shows |
 * |---|---|
 * | 200                                              | edit mode |
 * | 404 `no_active_revision`, no company in the org  | create mode |
 * | 404 `no_active_revision`, a company in the org   | this node has not caught up (never create mode) |
 * | 401 or 403                                       | a request for a token, worded by whether one is stored |
 * | a plain 404, or a body that is not JSON          | this process does not serve the configuration |
 * | nothing (status 0)                               | the engine could not be reached |
 *
 * A token change mid-edit re-reads without discarding the draft: the draft
 * and its log stay on screen, the check runs again under the new token, and a
 * refusal pauses editing rather than throwing the work away.
 *
 * WHAT THIS COMPONENT OWNS is everything with a lifetime: the reducer, the
 * dry-run check (`useCheck.ts`), the live region, the shortcuts, the
 * selection in the URL, the fullscreen container and which dialog is open.
 * The views and dialogs it hosts are handed in as [BuilderSurfaces] and reach
 * all of it through `BuilderContext`, so none of them starts a request.
 *
 * THE LAYOUT FILLS THE SCREEN in the canvas view: the lens is a flex column
 * whose canvas takes the height left under the toolbar, so `.screen` has
 * nothing to scroll and a wheel over the page never lands in a canvas that is
 * half off screen. Fullscreen takes the whole builder container (toolbar,
 * view, dialogs, its own toast outlet and live region), because a fullscreen
 * element renders only its own subtree, and an editor opened in fullscreen
 * from a canvas alone would open invisibly behind it.
 */

import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useReducer,
  useRef,
  useState,
  type ComponentType,
  type ReactNode,
  type RefObject,
} from "react";
import { href, useLeaveGuard, useNavigator, useParam, useUnloadGuard } from "~/app/router.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useAgents, useConnection, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { apiToken, onTokenChanged, requestToken } from "~/protocol/index.ts";
import type { ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import type { Tone } from "@crewlethq/ui";
import {
  BuilderContext,
  keepsTheLens,
  type AddKind,
  type BuilderApi,
  type BuilderViewHandle,
  type ChartKind,
  type EditorSectionName,
} from "./BuilderContext.tsx";
import { allUnits, locate, type Draft } from "./model/draft.ts";
import { COMPANY_KEY, seatKey, type NodeKey } from "./model/keys.ts";
import type { PlacedProblem } from "./model/problems.ts";
import {
  builderReducer,
  handlesOf,
  hasChanges,
  INITIAL_BUILDER,
  isBaseKeyed,
  type BuilderAction,
  type BuilderState,
  type LastChange,
} from "./model/reducer.ts";
import { describeOperation } from "./model/operations.ts";
import type { CheckOutcome, CheckStatus } from "./model/scheduler.ts";
import { readyToUpdate } from "./model/writes.ts";
import { UpdateDraftDialog } from "./UpdateDraftDialog.tsx";
import { isRecord } from "./model/json.ts";
import {
  revisionOfEtag,
  type Clock,
  type ConfigTransport,
  type HttpAnswer,
} from "./model/transport.ts";
import type { DraftStorage } from "./model/persistence.ts";
import { clearDraft } from "./model/persistence.ts";
import { deriveChanges } from "./model/changes.ts";
import { saveRules } from "./model/scheduler.ts";
import { seatsNeedingContact } from "./model/templates.ts";
import type { KeySource } from "./model/keys.ts";
import { ReviewSaveDialog } from "./ReviewSaveDialog.tsx";
import { AfterSaveStrip } from "./AfterSaveStrip.tsx";
import { CreateCompany, NextSteps } from "./CreateCompany.tsx";
import { clearSavedRevision, recordSavedRevision, useSavedRevision } from "./savedRevision.ts";
import { browserClock, randomKeys, restTransport, sessionDraftStorage } from "./runtime.ts";
import { useSave, type SaveEvents } from "./useSave.ts";
import { useCheck } from "./useCheck.ts";
import { useDraftKeeping } from "./useDraftKeeping.ts";
import { addMenu, nodeMenu } from "./nodeActions.tsx";
import { useOpenScreen, useStructure } from "./useCharts.ts";
import { screenPath } from "./dialogParts.tsx";
import {
  type GlyphProps,
  AccountTreeGlyph,
  AddGlyph,
  CableGlyph,
  CheckGlyph,
  DeleteGlyph,
  DnsGlyph,
  ErrorGlyph,
  FullscreenExitGlyph,
  FullscreenGlyph,
  KeyGlyph,
  KeyboardArrowDownGlyph,
  KeyboardArrowUpGlyph,
  ListGlyph,
  MemoryGlyph,
  MoreVertGlyph,
  RedoGlyph,
  RefreshGlyph,
  RemoveGlyph,
  SaveGlyph,
  UndoGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import {
  Button,
  ButtonLink,
  Callout,
  cx,
  EmptyState,
  IconButton,
  InlineCode,
  isComposing,
  isModalLayerOpen,
  Kbd,
  LayerHost,
  Menu,
  type MenuEntry,
  ConfirmModal,
  Modal,
  SegmentedControl,
  Skeleton,
  TabPanel,
  Tag,
  ToastProvider,
  useToast,
} from "@crewlethq/ui";

// ---------------------------------------------------------------------------
// Surfaces
// ---------------------------------------------------------------------------

/** What a dialog about one node is given. */
export interface NodeDialogProps {
  nodeKey: NodeKey;
  onClose: () => void;
}

/** What the node editor is given: a dialog about one node, opened at a part of it. */
export interface EditorDialogProps extends NodeDialogProps {
  section?: EditorSectionName;
}

/** What the Add dialog is given. */
export interface AddDialogProps {
  /** A unit's key, or `null` for the company root. */
  parent: NodeKey | null;
  kind?: AddKind;
  onClose: () => void;
}

/**
 * The views and dialogs the Builder hosts. They read and act through
 * `BuilderContext`; the Builder decides which is mounted. Every one is
 * required: a lens missing its canvas or its editor is not a smaller lens
 * but a broken one, so an unbound surface is a type error rather than a
 * screen that apologises at run time.
 */
export interface BuilderSurfaces {
  canvas: ComponentType<{
    chart: ChartKind;
    /**
     * What the chart draws in its OWN corner, where the console chart keeps
     * it: the fullscreen toggle at the end of the zoom bar, and the switch
     * between the two charts as a bar under it. They were two controls in
     * the page toolbar, 800px from the canvas they act on.
     */
    chrome?: { controls?: ReactNode; switcher?: ReactNode };
    /**
     * The node an open surface is about, so the chart can ease onto it and
     * push the rest of itself back behind the decision. `null` gives the
     * reader their view back.
     */
    about?: string | null;
    /**
     * An add the chart draws ITSELF, in the ghost of the node about to exist,
     * rather than a dialog this host opens over it. Handed here only while a
     * chart with a place for it is on screen: see `adding` below.
     */
    adding?: {
      parent: NodeKey | null;
      kind?: AddKind;
      opening: number;
      onClose: () => void;
    } | null;
  }>;
  table: ComponentType;
  editor: ComponentType<EditorDialogProps>;
  /**
   * The add as a DIALOG, for the views that have no place to draw it in: the
   * table, and the reporting chart, which draws who reports to whom rather
   * than what is inside what. The structure chart draws the same form in the
   * chart instead, through `canvas`'s `adding`.
   */
  add: ComponentType<AddDialogProps>;
  move: ComponentType<NodeDialogProps>;
  remove: ComponentType<NodeDialogProps>;
  changeKind: ComponentType<NodeDialogProps>;
}

/** A dialog the lens is asked to open. */
type DialogRequest =
  | { readonly type: "editor"; readonly key: NodeKey; readonly section?: EditorSectionName }
  | { readonly type: "move" | "remove" | "changeKind"; readonly key: NodeKey }
  | { readonly type: "add"; readonly parent: NodeKey | null; readonly kind?: AddKind }
  | { readonly type: "discard" };

/**
 * A dialog the lens has open, and which opening of it this is.
 *
 * ONE MOUNT PER OPENING. The host keys each dialog by `opening`, so every
 * opening builds its form afresh (another node, or the same node again),
 * while the node an open dialog is about can change its key under it without
 * a remount: the first check keys the base by the engine's handles, and a
 * dialog opened before that follows its node to the new key. Keyed by the
 * node instead, that dialog was torn down and mounted again on the check's
 * answer, which played its entrance again and threw away its focus.
 */
type OpenDialog = DialogRequest & { readonly opening: number };

// ---------------------------------------------------------------------------
// Posture
// ---------------------------------------------------------------------------

export type Posture =
  | { readonly kind: "loading" }
  | {
      readonly kind: "edit";
      readonly document: Record<string, unknown>;
      readonly revision: string;
    }
  | { readonly kind: "create" }
  | { readonly kind: "behind" }
  | { readonly kind: "guarded"; readonly tokenStored: boolean }
  | { readonly kind: "unserved" }
  | { readonly kind: "unreachable"; readonly detail: string }
  | { readonly kind: "failed"; readonly detail: string };

/** What the org snapshot says about whether a company exists. */
export interface OrgKnowledge {
  /** False until the socket has delivered (or been refused) the org snapshot. */
  readonly known: boolean;
  /** The company name the snapshot carries; "" for none. */
  readonly name: string;
}

const text = (value: unknown): string => (typeof value === "string" ? value : "");

/** Decides the lens posture from `GET /config`'s answer and the org snapshot. */
export function postureOf(answer: HttpAnswer, org: OrgKnowledge, tokenStored: boolean): Posture {
  const body = isRecord(answer.body) ? answer.body : {};
  const code = text(body.error);
  if (answer.status === 200) {
    const revision = revisionOfEtag(answer.etag);
    if (!isRecord(answer.body) || revision === null) {
      return {
        kind: "failed",
        detail:
          "The engine answered without naming its active revision, so a save could not be conditional on it.",
      };
    }
    return { kind: "edit", document: answer.body, revision };
  }
  if (answer.status === 401 || answer.status === 403) return { kind: "guarded", tokenStored };
  if (code === "unreadable_body") return { kind: "unserved" };
  if (answer.status === 404) {
    if (code !== "no_active_revision") return { kind: "unserved" };
    // A NODE THAT HAS NOT CAUGHT UP IS NEVER OFFERED CREATE MODE. Its store
    // holds no revision while the fleet runs a company, and a create from
    // here would be refused at best. Until the snapshot has arrived, the
    // answer is not known either way.
    if (!org.known) return { kind: "loading" };
    return org.name ? { kind: "behind" } : { kind: "create" };
  }
  if (answer.status === 0) {
    return { kind: "unreachable", detail: text(body.detail) };
  }
  return {
    kind: "failed",
    detail: text(body.detail) || code || `The engine answered with status ${answer.status}.`,
  };
}

// ---------------------------------------------------------------------------
// Presentation of the check
// ---------------------------------------------------------------------------

interface StatusLook {
  readonly label: string;
  readonly tone: Tone;
  readonly icon: ComponentType<GlyphProps>;
}

/**
 * How the toolbar reads a check status. A refusal of the token is worded by
 * whether one is stored: "refused" names a credential the browser does not
 * hold when the operator has cleared it, which sends them looking for a
 * wrong token rather than a missing one.
 */
function statusLook(status: CheckStatus, problems: number, tokenStored: boolean): StatusLook {
  switch (status) {
    case "checking":
      return { label: "Checking", tone: "neutral", icon: RefreshGlyph };
    case "clean":
      return { label: "No problems", tone: "success", icon: CheckGlyph };
    case "problems":
      return { label: plural(problems, "problem"), tone: "danger", icon: ErrorGlyph };
    case "unreachable":
      return { label: "Could not reach the engine to check", tone: "warning", icon: CableGlyph };
    case "conflict":
      return { label: "The configuration changed", tone: "warning", icon: WarningGlyph };
    case "guarded":
      return {
        label: tokenStored ? "The engine refused the token" : "Needs an operator token",
        tone: "danger",
        icon: KeyGlyph,
      };
  }
}

/**
 * The conflict the check stands on, or `null`. Called only while the status
 * is `conflict`.
 *
 * THE LAST ANSWER, WHATEVER GENERATION IT WAS FOR. A conflict halts the check:
 * a draft that changes afterwards (a kept draft restored into it) sends
 * nothing more, so no answer for the new generation ever arrives, and the
 * engine still holds the newer revision the last answer named. Read only for
 * the current generation, that answer vanished and took the Update my draft
 * banner with it, leaving a lens paused with no way forward.
 */
function conflictOf(state: BuilderState): Extract<CheckOutcome, { status: "conflict" }> | null {
  const outcome = state.check.outcome;
  return outcome?.status === "conflict" ? outcome : null;
}

// ---------------------------------------------------------------------------
// Selection in the URL
// ---------------------------------------------------------------------------

/** The URL filters that name a node: a unit by name, a seat by handle. */
interface SelectionParams {
  readonly unit: string;
  readonly seat: string;
}

/**
 * Whether a key still names something in the draft.
 *
 * The company is a node like any other to a view (its card carries the
 * charter's Edit and the Add menu) and the only one `locate` cannot find: it
 * is the tree's root rather than an element of a list. Read as absent, a
 * selected company card was cleared again on the next state change.
 */
function isPresent(state: BuilderState, key: NodeKey): boolean {
  return key === COMPANY_KEY || locate(state.draft, key) !== undefined;
}

/** The filters that name `key`, or `null` when the node has no name the URL can carry yet. */
function paramsOf(state: BuilderState, key: NodeKey): SelectionParams | null {
  // The company is what the lens opens on, so it names itself with no filter.
  if (key === COMPANY_KEY) return { unit: "", seat: "" };
  const found = locate(state.draft, key);
  if (!found) return null;
  if (found.kind === "unit") {
    const name = found.node.data.name;
    return name ? { unit: name, seat: "" } : null;
  }
  const handle = handlesOf(state).get(key);
  return handle ? { unit: "", seat: handle } : null;
}

/** The node the filters name, or `null`. */
function keyOfParams(state: BuilderState, params: SelectionParams): NodeKey | null {
  if (params.seat) {
    const direct = seatKey(params.seat);
    if (locate(state.draft, direct)) return direct;
    for (const [key, handle] of handlesOf(state)) if (handle === params.seat) return key;
    return null;
  }
  if (params.unit) {
    for (const { unit } of allUnits(state.draft)) {
      if (unit.data.name === params.unit) return unit.key;
    }
  }
  return null;
}

// ---------------------------------------------------------------------------
// The live region
// ---------------------------------------------------------------------------

/**
 * How long the region stays empty before a sentence is written into it. A
 * screen reader announces a CHANGE of a polite region's text, so the same
 * sentence twice in a row (two undos of the same kind) would be announced
 * once unless the region is emptied first; a few frames apart is enough for
 * every screen reader to observe the empty state.
 */
const ANNOUNCE_DELAY_MS = 50;

/**
 * What the live region says about the last change to the draft.
 *
 * AN UNDO SAYS IT UNDID. The model describes an operation in the past tense
 * ("Edited CEO: goal."), and announced bare after an undo that sentence tells
 * a screen reader the edit was just made, the opposite of what happened.
 */
export function announcementOf(last: LastChange): string {
  switch (last.kind) {
    case "applied":
      return last.description;
    case "undone":
      return `Undone: ${last.description}`;
    case "redone":
      return `Redone: ${last.description}`;
  }
}

function useLiveRegion(): { text: string; announce: (message: string) => void } {
  const [text, setText] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const announce = useCallback((message: string) => {
    clearTimeout(timer.current);
    setText("");
    timer.current = setTimeout(() => setText(message), ANNOUNCE_DELAY_MS);
  }, []);
  return { text, announce };
}

// ---------------------------------------------------------------------------
// The lens
// ---------------------------------------------------------------------------

/**
 * Below this width the drawer takes the whole window and
 * a visualization has no room beside it, so a lens opened with no `view`
 * starts on the table.
 */
const NARROW_QUERY = "(max-width: 860px)";

/** Undo and redo, as the keyboard handler below accepts them. */
const UNDO_KEYS = ["Mod", "z"] as const;
const REDO_KEYS = ["Mod", "Shift", "z"] as const;

function prefersTable(): boolean {
  try {
    return globalThis.matchMedia?.(NARROW_QUERY).matches ?? false;
  } catch {
    return false;
  }
}

export function Builder({
  surfaces,
  transport = restTransport,
  clock = browserClock,
  storage,
  keys = randomKeys,
}: {
  surfaces: BuilderSurfaces;
  /** Injected by a suite; the browser bindings otherwise. */
  transport?: ConfigTransport;
  clock?: Clock;
  /** Where the draft's log is kept; the tab's session storage otherwise. */
  storage?: DraftStorage | null;
  /** Where write ids come from; the browser's random source otherwise. */
  keys?: KeySource;
}) {
  const container = useRef<HTMLDivElement>(null);
  const [kept] = useState(() => (storage === undefined ? sessionDraftStorage() : storage));
  return (
    <div ref={container} className="org-builder">
      {/* The builder's own layer host, with its own toast stack INSIDE it. A
          fullscreen element renders only its own subtree, so a dialog or a
          toast portalled to the application's root while the canvas is
          fullscreen is not painted at all: the reader presses Save and
          nothing happens. useToast finds the nearest provider. */}
      <LayerHost>
        <ToastProvider>
          <Lens
            surfaces={surfaces}
            transport={transport}
            clock={clock}
            storage={kept}
            keys={keys}
            container={container}
          />
        </ToastProvider>
      </LayerHost>
    </div>
  );
}

function Lens({
  surfaces,
  transport,
  clock,
  storage,
  keys,
  container,
}: {
  surfaces: BuilderSurfaces;
  transport: ConfigTransport;
  clock: Clock;
  storage: DraftStorage | null;
  keys: KeySource;
  container: RefObject<HTMLDivElement | null>;
}) {
  const nav = useNavigator();
  const toast = useToast();
  const org = useOrg();
  const { connected, authRejected } = useConnection();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const [narrowDefault] = useState(prefersTable);
  const [viewParam, setView] = useParam(
    "view",
    narrowDefault ? "table" : "visualization",
    "section",
  );
  const view = viewParam === "table" ? "table" : "visualization";
  const [chartParam, setChart] = useParam("chart", "structure", "section");
  const chart: ChartKind = chartParam === "reporting" ? "reporting" : "structure";
  const [unitParam] = useParam("unit", "", "filter");
  const [seatParam] = useParam("seat", "", "filter");
  const viewPanel = useId();
  const chartPanel = useId();

  const [state, dispatchRaw] = useReducer(builderReducer, INITIAL_BUILDER);
  const stateRef = useRef(state);
  stateRef.current = state;
  const [loaded, setLoaded] = useState(false);

  /* THE VISUALIZATION TAKES THE WINDOW. A canvas fills the box its screen
     gives it and clips, so the application frame's scroller has to stop being
     one while this view is on: a wheel turned over a page that scrolls
     otherwise lands in a canvas half off screen. The table is an ordinary
     column and gives the scroller straight back, and so does the posture
     screen this lens draws instead of either until the engine has answered.

     ABOVE THAT POSTURE RETURN, because a hook below one runs on some renders
     and not others. */
  useFillScreen(loaded && view === "visualization");
  const live = useLiveRegion();
  const { announce } = live;

  // ---- Reading the configuration -----------------------------------------

  const [read, setRead] = useState<{ answer: HttpAnswer; seq: number } | null>(null);
  const reading = useRef<{ seq: number; controller: AbortController } | null>(null);
  const readSeq = useRef(0);
  // A save loads the stored revision even though it names the base the draft
  // already stands on: the engine's document, not the one that was sent.
  const forceLoad = useRef(false);

  const load = useCallback(
    (force = false) => {
      reading.current?.controller.abort();
      const seq = ++readSeq.current;
      const controller = new AbortController();
      reading.current = { seq, controller };
      if (force) forceLoad.current = true;
      transport.current(controller.signal).then(
        (answer) => {
          if (reading.current?.seq !== seq) return;
          reading.current = null;
          setRead({ answer, seq });
        },
        () => {
          // Aborted by a newer read or by the unmount, which own the answer.
        },
      );
    },
    [transport],
  );

  useEffect(() => {
    load();
    return () => reading.current?.controller.abort();
  }, [load]);

  const orgName = org?.name ?? "";
  // The store starts with an empty projection, so an empty one proves nothing
  // until the socket has connected (and delivered its snapshot) or been
  // refused; a projection that names a company is known however it arrived.
  const orgKnown = connected || authRejected || orgName !== "";
  const posture = useMemo(
    (): Posture =>
      read
        ? postureOf(read.answer, { known: orgKnown, name: orgName }, apiToken() !== "")
        : { kind: "loading" },
    [read, orgKnown, orgName],
  );

  const check = useCheck({ state, stateRef, dispatch: dispatchRaw, loaded, transport, clock });

  // Adopt what a read found: the company to edit, or nothing to create from.
  const adopted = useRef<{ seq: number; kind: Posture["kind"] } | null>(null);
  useEffect(() => {
    if (!read) return;
    if (adopted.current?.seq === read.seq && adopted.current.kind === posture.kind) return;
    adopted.current = { seq: read.seq, kind: posture.kind };
    const current = stateRef.current;
    const hasWork = current.log.ops.length > 0 || current.log.undone.length > 0;
    const force = forceLoad.current;
    forceLoad.current = false;
    // A DRAFT WITH WORK ON IT STAYS ON ITS BASE, whatever the read found. A
    // newer revision is the check's to report as a conflict, and the operator
    // decides what happens to the work; without work there is nothing to lose
    // by standing on it. A forced read is no exception: it reads back what a
    // save stored, whose own answer already keyed the base and left the lens
    // editable, so an edit made while that read was out stands on the saved
    // revision already, and reading the same revision over it would throw the
    // edit away unasked for a document that differs from the one sent only by
    // the engine's own normalization.
    if (posture.kind === "edit") {
      const stale = current.mode !== "edit" || current.base.revision !== posture.revision;
      if (!loaded || ((force || stale) && !hasWork)) {
        dispatchRaw({
          type: "load",
          mode: "edit",
          document: posture.document,
          revision: posture.revision,
        });
        setLoaded(true);
      }
    } else if (posture.kind === "create") {
      if (!loaded || ((force || current.mode !== "create") && !hasWork)) {
        dispatchRaw({ type: "load", mode: "create", document: null, revision: null });
        setLoaded(true);
      }
    }
  }, [read, posture, loaded]);

  // A NEW TOKEN IS A NEW READER. The draft stays on screen; storage forgets
  // it (the tab may have changed hands), and both the configuration and the
  // check are asked again under the new credential.
  const { reset, requestReset } = check;
  useEffect(
    () =>
      onTokenChanged(() => {
        requestReset();
        dispatchRaw({ type: "tokenChanged" });
        load();
      }),
    [requestReset, load],
  );

  // An org push follows every apply, so the configuration may have moved
  // under the draft: check again rather than wait for the next edit. A create
  // draft has no configuration to move, and hears only of one appearing: a
  // push that names a company is exactly that, and its check is the refusal
  // that says so.
  const lastOrg = useRef(org);
  useEffect(() => {
    if (lastOrg.current === org) return;
    lastOrg.current = org;
    if (!loaded) return;
    if (stateRef.current.mode === "edit" || orgName !== "") reset();
  }, [org, orgName, loaded, reset]);

  // ---- Editing ------------------------------------------------------------

  const problemsCurrent = state.check.generation === state.generation;
  // THE LAST ANSWER ABOUT THIS DRAFT, whoever asked. The check machine knows
  // what it sent; a save's refusal is an answer about the same document that
  // the machine never saw, and it lands in the reducer. So a settled answer
  // for the current draft decides the status, and the machine decides it only
  // while a check is out or before any answer.
  const answered = problemsCurrent ? state.check.outcome : null;
  const status: CheckStatus =
    check.machine.status === "checking" || !answered ? check.machine.status : answered.status;
  const keeping = useDraftKeeping({ state, dispatch: dispatchRaw, loaded, storage, now: Date.now });
  const { forget } = keeping;
  // A refused read may be the tab changing hands: the kept draft is not
  // offered to whoever holds it next.
  useEffect(() => {
    if (posture.kind === "guarded") forget();
  }, [posture.kind, forget]);

  // What a save's answer leads to. A ref, because a save outlives the render
  // that started it, and may outlive the Builder.
  const saveEvents = useRef<SaveEvents>({
    onSending: () => {},
    onNotLanded: () => {},
    onLanded: () => {},
    onConflict: () => {},
    onRefused: () => {},
  });
  const save = useSave({ stateRef, transport, keys, events: saveEvents });

  // A SAVE A PREVIOUS VISIT NEVER HEARD BACK FROM is settled before its kept
  // log is offered: it may have landed, and the log replayed onto its own
  // revision would apply every operation twice (see `useDraftKeeping`).
  const { resume } = save;
  useEffect(() => {
    if (keeping.unsettled) resume(keeping.unsettled);
  }, [keeping.unsettled, resume]);

  // Read at render: a token change always dispatches, so this is current.
  const tokenStored = apiToken() !== "";
  const readOnlyReason = useMemo((): string | null => {
    if (save.unsettled || keeping.unsettled) return "the outcome of the last save is not known yet";
    if (keeping.offer) return "a kept draft is waiting for Keep or Discard";
    if (posture.kind === "guarded" || status === "guarded") {
      return tokenStored ? "the engine refused this browser's token" : "no operator token is set";
    }
    if (status === "conflict") return "the configuration changed since this draft was started";
    if (loaded && !isBaseKeyed(state)) return "the engine has not described this company yet";
    return null;
  }, [
    save.unsettled,
    keeping.unsettled,
    keeping.offer,
    posture.kind,
    status,
    loaded,
    state,
    tokenStored,
  ]);
  const readOnly = !loaded || readOnlyReason !== null;

  const dispatch = useCallback(
    (action: BuilderAction) => {
      const mutates =
        action.type === "record" ||
        action.type === "undo" ||
        action.type === "redo" ||
        action.type === "discard";
      if (mutates && readOnlyReason !== null) {
        announce(`Editing is paused because ${readOnlyReason}.`);
        return;
      }
      dispatchRaw(action);
    },
    [readOnlyReason, announce],
  );

  // Say what the last operation did, and put focus where it leads.
  const viewHandle = useRef<BuilderViewHandle | null>(null);
  const focusNode = useCallback((key: NodeKey) => viewHandle.current?.focusNode(key), []);
  // ONE IDENTITY FOR THE LENS'S LIFETIME. A view registers in an effect that
  // depends on this function, so a new one per state change unregistered and
  // registered the mounted view again after every edit and every answer.
  const registerView = useCallback((handle: BuilderViewHandle) => {
    viewHandle.current = handle;
    return () => {
      if (viewHandle.current === handle) viewHandle.current = null;
    };
  }, []);
  useEffect(() => {
    if (!state.last) return;
    announce(announcementOf(state.last));
    focusNode(state.last.focus);
  }, [state.last, announce, focusNode]);

  const [refusal, setRefusal] = useState<string | null>(null);
  useEffect(() => {
    if (!state.refusal) return;
    setRefusal(state.refusal.message);
    announce(state.refusal.message);
  }, [state.refusal, announce]);
  useEffect(() => {
    if (state.last) setRefusal(null);
  }, [state.last]);

  // Undo and redo from the keyboard, anywhere in the builder that is not a
  // text field (where the same keys undo typing) and never under a modal.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.defaultPrevented || isComposing(e) || isModalLayerOpen()) return;
      if (!(e.metaKey || e.ctrlKey) || e.altKey) return;
      if (e.code !== "KeyZ" && e.key.toLowerCase() !== "z") return;
      const root = container.current;
      const target = e.target instanceof HTMLElement ? e.target : null;
      if (!root || (target && target !== document.body && !root.contains(target))) return;
      if (target && isTextEntry(target)) return;
      e.preventDefault();
      dispatch({ type: e.shiftKey ? "redo" : "undo" });
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [dispatch, container]);

  // ---- Selection ------------------------------------------------------------

  const [selected, setSelected] = useState<NodeKey | null>(null);
  const selectedRef = useRef(selected);
  selectedRef.current = selected;
  const [dialog, setDialog] = useState<OpenDialog | null>(null);
  const openings = useRef(0);
  const openDialog = useCallback(
    (next: DialogRequest) => setDialog({ ...next, opening: ++openings.current }),
    [],
  );

  // A NODE HELD ACROSS A KEYING OF THE BASE KEEPS ITS PLACE. Until the first
  // check answers, a seat that declares no handle is keyed by its path, and
  // the answer re-keys the base by the engine's handles; a save re-keys the
  // nodes it created. The reducer lists what moved (`state.rekeyed`), and an
  // open dialog or the selection reads its node through that list in the
  // very render the keys change: followed a render later, an open editor was
  // drawn as gone ("This node is no longer in the draft") for that render and
  // then mounted again. The effect moves the held keys over for good, before
  // the selection effect below, which would otherwise clear a selection whose
  // key has gone.
  const { rekeyed } = state;
  const follow = useCallback((key: NodeKey) => rekeyed.get(key) ?? key, [rekeyed]);
  useEffect(() => {
    if (rekeyed.size === 0) return;
    if (selectedRef.current !== null) {
      selectedRef.current = follow(selectedRef.current);
      setSelected(selectedRef.current);
    }
    setDialog((open) => (open && "key" in open ? { ...open, key: follow(open.key) } : open));
  }, [rekeyed, follow]);
  const selection = selected === null ? null : follow(selected);

  // THE URL AND THE DRAFT BOTH MOVE THE SELECTION, and one effect answers
  // both: a link (or the command palette) naming a unit or a seat selects it,
  // and a rename in the draft rewrites the name the URL holds. Which of the
  // two moved is read off the draft: a URL that names a node still in the
  // draft is a new selection, and a selected node whose name the URL no
  // longer matches was renamed.
  useEffect(() => {
    const params = { unit: unitParam, seat: seatParam };
    const named = params.unit !== "" || params.seat !== "";
    const key = selectedRef.current;
    const present = key !== null && isPresent(state, key);
    if (present) {
      const now = paramsOf(state, key);
      if (!now || (now.unit === params.unit && now.seat === params.seat)) return;
      const fromUrl = keyOfParams(state, params);
      if (fromUrl && fromUrl !== key) setSelected(fromUrl);
      else nav.filter({ unit: now.unit || null, seat: now.seat || null });
      return;
    }
    const resolved = named ? keyOfParams(state, params) : null;
    if (resolved) {
      setSelected(resolved);
      return;
    }
    if (key !== null) {
      // The selected node was removed: the selection goes with it.
      setSelected(null);
      if (named) nav.filter({ unit: null, seat: null });
    }
  }, [state, unitParam, seatParam, nav]);

  const select = useCallback(
    (key: NodeKey | null) => {
      setSelected(key);
      const params = key ? paramsOf(stateRef.current, key) : null;
      nav.filter({ unit: params?.unit || null, seat: params?.seat || null });
    },
    [nav],
  );

  // ---- Dialogs ----------------------------------------------------------------

  const closeDialog = useCallback(() => setDialog(null), []);
  const openIfWritable = useCallback(
    (next: DialogRequest) => {
      if (readOnlyReason !== null) {
        announce(`Editing is paused because ${readOnlyReason}.`);
        return;
      }
      openDialog(next);
    },
    [readOnlyReason, announce, openDialog],
  );

  const exitFullscreen = useFullscreenExit();
  const askForToken = useCallback(() => {
    // The token dialog belongs to the shell, outside the fullscreen element.
    exitFullscreen();
    requestToken();
  }, [exitFullscreen]);

  // ---- Updating onto a newer revision -------------------------------------------

  const [updateNote, setUpdateNote] = useState<{ busy: boolean; message: string | null }>({
    busy: false,
    message: null,
  });
  const updateRead = useRef<AbortController | null>(null);
  useEffect(() => () => updateRead.current?.abort(), []);

  // Reads the revision the engine holds now, and hands the reducer the update
  // only once this node serves that revision or a later one: a node behind a
  // load balancer can still answer with the draft's own base, and rebasing
  // onto that would lose the change the conflict was about. `stand` is the
  // lens with no work moving onto that revision (see below): an update of
  // nothing, confirmed at once, so nothing is offered for review.
  const beginUpdate = useCallback(
    async (conflictRevisionId: string | null, stand = false) => {
      const base = stateRef.current.base.revision;
      if (base === null) return;
      updateRead.current?.abort();
      const controller = new AbortController();
      updateRead.current = controller;
      setUpdateNote({ busy: true, message: null });
      try {
        const ready = await readyToUpdate(
          transport,
          { baseRevision: base, conflictRevisionId },
          controller.signal,
        );
        if (controller.signal.aborted) return;
        if (ready.kind === "ready") {
          dispatchRaw({
            type: "updateBegin",
            document: ready.document,
            revision: ready.revisionId,
            derived: ready.derived,
          });
          // Only while there is still nothing to carry over: confirming an
          // update that carries work would drop any operation whose target
          // is gone without the operator seeing it listed.
          const { log } = stateRef.current;
          if (stand && log.ops.length === 0 && log.undone.length === 0) {
            dispatchRaw({ type: "updateConfirm" });
          }
          setUpdateNote({ busy: false, message: null });
        } else {
          setUpdateNote({
            busy: false,
            message:
              ready.kind === "behind"
                ? "This node has not caught up with the newer revision yet. Try again in a moment."
                : ready.detail,
          });
        }
      } catch {
        // Aborted: the Builder went away, or a newer attempt replaced this one.
      }
    },
    [transport],
  );

  const conflict = status === "conflict" ? conflictOf(state) : null;
  const creating = state.mode === "create";
  const templateApplied = state.log.ops.some((op) => op.type === "applyTemplate");
  useEffect(() => {
    if (conflict?.reason === "already_configured") setCompanyExists(true);
  }, [conflict]);

  // A DRAFT WITH NO WORK STANDS ON THE NEWER REVISION. The conflict flow
  // exists to protect the operator's changes; with none (a lens somebody is
  // only reading when a colleague saves, which the org push reports at
  // once) it would pause editing behind a banner offering to update nothing.
  // A kept draft still waiting for its decision is left alone: it was offered
  // against this base, and moving the base under the offer would refuse its
  // Keep.
  //
  // THROUGH THE UPDATE, NEVER A PLAIN READ. A document read from `GET
  // /config` keys the seats that declare no handle by their paths until the
  // next check answers, so every node something held lost its key for that
  // moment: an open editor with a typed, unapplied form was drawn as gone
  // and mounted again empty, and the selection was cleared. The update reads
  // the newer revision with the engine's description of it
  // (`writes.readyToUpdate`), so the base is keyed by handle at once and an
  // existing seat keeps its key across the two revisions.
  const draftIsEmpty = state.log.ops.length === 0 && state.log.undone.length === 0;
  const keptPending = keeping.pending || keeping.offer !== null;
  useEffect(() => {
    if (!conflict || !draftIsEmpty || keptPending) return;
    if (conflict.reason === "revision_advanced" || conflict.reason === "base_moved") {
      void beginUpdate(conflict.currentRevisionId, true);
    }
  }, [conflict, draftIsEmpty, keptPending, beginUpdate]);

  // ---- Saving -------------------------------------------------------------------

  const savedRevision = useSavedRevision();
  const [created, setCreated] = useState(false);
  // A company that appeared while a create draft was being written: the draft
  // cannot be applied to it, and must never be replayed onto it.
  const [companyExists, setCompanyExists] = useState(false);
  // The one way on from such a draft, offered by the dialog that reports it
  // and by the banner that stays once the operator keeps the draft to read.
  const openExistingCompany = useCallback(() => {
    setCompanyExists(false);
    dispatchRaw({ type: "discard" });
    load(true);
  }, [load]);
  const [reviewing, setReviewing] = useState(false);
  const reviewAfterUpdate = useRef(false);
  const changed = useMemo(() => hasChanges(state), [state]);

  // ---- Leaving with work --------------------------------------------------------

  // A CLOSED TAB TAKES THE DRAFT WITH IT. The log is kept in the tab's own
  // session storage, which a reload or a trip to another screen of this page
  // finds again and a closed tab does not, so while the draft has changes the
  // browser asks before the tab goes. Where storage cannot keep the draft at
  // all, leaving the lens loses it as well, and that move is asked about
  // first; moving within the lens (a view, a chart, a selection) keeps it.
  useUnloadGuard(changed);
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);
  useLeaveGuard(
    changed && !keeping.survives
      ? (to, leave) => {
          if (keepsTheLens(to)) return false;
          setLeaving({ leave });
          return true;
        }
      : null,
  );
  const rules = saveRules(status, changed);
  const openReview = useCallback(() => {
    save.prepare();
    setReviewing(true);
  }, [save]);

  saveEvents.current = {
    // These run even when the Builder has gone away while the save was out,
    // so each works on storage and the tab-lived store directly.
    onSending: (attempt) => keeping.markWrite(attempt.writeId),
    onNotLanded: () => keeping.markWrite(null),
    onLanded: (landed) => {
      // Recorded and cleared first: a kept log of a saved draft would be
      // offered for replay onto its own revision.
      recordSavedRevision({
        revisionId: landed.revisionId,
        parentRevisionId: landed.parentRevisionId,
        epoch: landed.epoch,
      });
      clearDraft(storage);
      keeping.markWrite(null);
      // A SAVE A PREVIOUS VISIT SENT IS NOT THE DRAFT ON SCREEN: this visit
      // stands on that save's base with nothing restored, so making the draft
      // the base would label the old document with the new revision. The
      // revision it stored is read like any newer one instead (unforced, so
      // it is never read over work), and a save of this visit's own draft
      // makes that draft the base until the stored document is read back.
      if (!landed.resumed) {
        dispatchRaw({ type: "saved", revisionId: landed.revisionId, derived: landed.derived });
      }
      setReviewing(false);
      if (landed.mode === "create") setCreated(true);
      // THE TOAST IS THE ANNOUNCEMENT: its host is a polite live region of
      // its own, so saying the same sentence through the Builder's region as
      // well had a screen reader read it twice.
      toast.ok(
        landed.resumed
          ? "The last save from this tab was stored. The engine is applying it."
          : "Saved. The engine is applying it.",
      );
      load(!landed.resumed);
    },
    onConflict: ({ reason, currentRevisionId }) => {
      setReviewing(false);
      reset();
      if (reason === "already_configured") setCompanyExists(true);
      if (reason === "revision_advanced" || reason === "base_moved") {
        reviewAfterUpdate.current = true;
        void beginUpdate(currentRevisionId);
      }
    },
    onRefused: (settled) => {
      // The refusal is the engine's answer about exactly this draft, so it is
      // placed like a check's; asking again would only repeat it.
      if (settled.generation === stateRef.current.generation) {
        dispatchRaw({ type: "checked", settled });
      } else {
        reset();
      }
    },
  };

  // An update that started from a refused save goes back to the review once
  // it is confirmed: the operator was saving, and still is.
  const reviewAfterConfirm = useRef(false);
  const confirmUpdate = useCallback(() => {
    reviewAfterConfirm.current = reviewAfterUpdate.current;
    reviewAfterUpdate.current = false;
    dispatchRaw({ type: "updateConfirm" });
  }, []);
  const cancelUpdate = useCallback(() => {
    reviewAfterUpdate.current = false;
    dispatchRaw({ type: "updateCancel" });
  }, []);
  useEffect(() => {
    if (!reviewAfterConfirm.current || state.update) return;
    reviewAfterConfirm.current = false;
    if (hasChanges(stateRef.current)) openReview();
  }, [state.update, openReview]);

  // ---- The context --------------------------------------------------------------

  const api = useMemo((): BuilderApi => {
    const byNode = problemsCurrent ? state.check.problems.byNode : new Map();
    const sources = (key: NodeKey, severity: PlacedProblem["severity"]) =>
      ((byNode.get(key) ?? []) as readonly PlacedProblem[])
        .filter((p) => p.severity === severity)
        .map((p) => p.source);
    const derived = problemsCurrent ? state.check.derived : null;
    return {
      state,
      dispatch,
      derived: derived ? { seats: derived.seats ?? [], units: derived.units ?? [] } : null,
      problemsFor: (key) => sources(key, "problem") as ConfigProblem[],
      warningsFor: (key) => sources(key, "warning") as ConfigWarning[],
      documentProblems: problemsCurrent
        ? state.check.problems.document
            .filter((p) => p.severity === "problem")
            .map((p) => p.source as ConfigProblem)
        : [],
      selection: { key: selection, select },
      openEditor: (key, section) => openDialog({ type: "editor", key, section }),
      openAdd: (parent, kind) => openIfWritable({ type: "add", parent, kind }),
      openMove: (key) => openIfWritable({ type: "move", key }),
      openDelete: (key) => openIfWritable({ type: "remove", key }),
      openChangeKind: (key) => openIfWritable({ type: "changeKind", key }),
      announce,
      focusNode,
      readOnly,
      agents,
      sandboxes,
      keys,
      registerView,
    };
  }, [
    state,
    problemsCurrent,
    dispatch,
    selection,
    select,
    openDialog,
    openIfWritable,
    announce,
    focusNode,
    readOnly,
    agents,
    sandboxes,
    keys,
    registerView,
  ]);

  // The structure chart the views draw, for the toolbar's node actions.
  const structure = useStructure(state);
  const openScreen = useOpenScreen();

  // ---- Rendering --------------------------------------------------------------

  if (!loaded) {
    return <PostureScreen posture={posture} onRetry={() => load()} onSetToken={askForToken} />;
  }

  const problemCount = problemsCurrent ? state.check.problems.problemCount : 0;
  const look = statusLook(status, problemCount, tokenStored);
  const canUndo = !readOnly && state.log.ops.length > 0;
  const canRedo = !readOnly && state.log.undone.length > 0;
  // THE CREATE FORM HAS NO TOOLBAR: there is no draft to undo, check or save
  // until a template is recorded. Not rendered rather than hidden with the
  // attribute, because `.org-builder-toolbar` sets `display: flex` and an
  // author rule beats the user agent's `[hidden] { display: none }`.
  const startingCompany = creating && !templateApplied;
  const pending = state.update;
  const Canvas = surfaces.canvas;
  const Table = surfaces.table;
  const fill = view === "visualization";
  const providers = state.base.document?.providers;
  const llm = isRecord(providers) ? providers.llm : undefined;
  // THE COMPANY RUNS AND ITS AGENTS WAIT. The engine applies a company with no
  // providers.llm and places its seats, then holds every delivery on the
  // seat's inbox until an apply brings a provider (engine/nomodels.go). The
  // dashboard writes none, so the lens says where one comes from.
  const noProvider = state.mode === "edit" && !(isRecord(llm) && Object.keys(llm).length > 0);
  const documentProblems = problemsCurrent ? state.check.problems.document : [];

  /*
   * WHAT THE CHART DRAWS IN ITS OWN CORNER. The console chart keeps one group
   * at the top right of the canvas: the zoom bar with the fullscreen toggle
   * at its end, then the switch between the two charts as a bar under it.
   * Both were controls of the page toolbar here, 800px from the chart they
   * act on, and the switch named two things that only exist inside it.
   *
   * The toggle follows the VIEW rather than living in one place: the table has
   * no canvas to hang it in, so it stays in the toolbar there. One control, in
   * the one place that view puts it.
   */
  const chartChrome = {
    controls: <FullscreenToggle container={container} />,
    switcher: (
      <SegmentedControl
        label="Chart"
        semantics="tabs"
        panelId={chartPanel}
        value={chart}
        onValueChange={setChart}
        size="sm"
        options={[
          { value: "structure", label: "Structure" },
          { value: "reporting", label: "Reporting" },
        ]}
      />
    ),
  };

  /*
   * WHERE AN ADD IS ASKED FOR, which is the one request this host does not
   * always answer with a dialog.
   *
   * THE STRUCTURE CHART DRAWS IT ITSELF, in the ghost of the node about to
   * exist, on the branch it will hang from, in the rank it will land in: a
   * form that asks for a child's name while saying nothing about where the
   * child goes is a form missing the one thing a chart is for, and a modal
   * over a blurred picture is that form with the answer hidden behind it. The
   * canvas is handed the request; nothing is mounted here.
   *
   * THE TABLE AND THE REPORTING CHART STILL ASK IN A DIALOG, because neither
   * draws a place for a unit's next child: the table is a grid of the rows
   * that exist, and the reporting chart draws who reports to whom, which is
   * derived and has no slot a new seat can be put into before it has a
   * manager. It is the ONE case that falls back, and it falls back to the same
   * form in a different shell rather than to a second, quieter add.
   *
   * WHICH VIEW IS ON SCREEN IS THIS HOST'S ANSWER and nobody else's, so the
   * decision is here: a canvas that claimed the add itself would leave the
   * host mounting a dialog for the same request whenever the reader was
   * looking at the table.
   *
   * AND AN ADD WHOSE PARENT HAS GONE HAS NOWHERE TO HANG. A unit can leave the
   * draft under an open add (an undo takes no dialog, so it reaches the toolbar
   * while the ghost is drawn), and a ghost on a branch that is not there is not
   * drawable. That add falls back to the dialog, which is where the refusal
   * saying so has always been drawn ("That unit is no longer in the draft"):
   * a chart that simply stopped drawing the ghost would take the form away
   * with no word about why.
   */
  const inTheChart = view === "visualization" && chart === "structure";
  const adding =
    dialog !== null &&
    dialog.type === "add" &&
    inTheChart &&
    structure.nodes.has(dialog.parent ?? COMPANY_KEY)
      ? dialog
      : null;
  const surface = adding === null ? dialog : null;

  /*
   * WHICH NODE THE OPEN SURFACE IS ABOUT, for the chart behind it. Every
   * dialog is about its own node and the discard question about the whole
   * draft. An add drawn IN the chart is not here at all: it is not a surface
   * over the picture, so the picture is neither pushed back nor moved off
   * what the reader was looking at -- the ghost itself is what the chart
   * eases onto, and the design system does that.
   */
  const about =
    surface === null || surface.type === "discard"
      ? null
      : surface.type === "add"
        ? (surface.parent ?? COMPANY_KEY)
        : surface.key;

  const handlers = {
    undo: () => dispatch({ type: "undo" }),
    redo: () => dispatch({ type: "redo" }),
    expandAll: () => viewHandle.current?.expandAll(),
    collapseAll: () => viewHandle.current?.collapseAll(),
    discard: () => openDialog({ type: "discard" }),
  };
  // THE TOOLBAR MIRRORS THE SELECTED NODE'S ACTIONS. A canvas card's own
  // buttons are pointer-only (a tree item may not contain tab stops), so this
  // is where a keyboard reaches them, and it is also where a node's actions
  // are when the outline is the view. They are the card's and the row's own
  // list (`nodeActions.nodeMenu`), not a copy of it: the same entries in the
  // same order under the same names and icons, Edit reports and the rule for
  // Open seat included, so what an operator learned on the canvas holds here.
  // With nothing selected it offers what the company's Add menu offers.
  const selectedView = selection !== null ? structure.nodes.get(selection) : undefined;
  const selectedName = selectedView
    ? selectedView.name || (selectedView.type === "company" ? "the company" : "the selected node")
    : "";
  const toolbarItems = selectedView
    ? nodeMenu(api, selectedView, openScreen)
    : addMenu(api, structure.nodes.get(COMPANY_KEY)!);

  const more: MenuEntry[] = [
    {
      key: "undo",
      label: "Undo",
      icon: <UndoGlyph />,
      onSelect: handlers.undo,
      disabled: !canUndo,
      hint: <Kbd keys={UNDO_KEYS} />,
    },
    {
      key: "redo",
      label: "Redo",
      icon: <RedoGlyph />,
      onSelect: handlers.redo,
      disabled: !canRedo,
      hint: <Kbd keys={REDO_KEYS} />,
    },
    // The narrow toolbar's menu offers the same two, under the marks the
    // design system's own tree controls draw for them: down for the state an
    // open row's chevron points at, up for the closed one. They wore the plus
    // and the minus, which is the drawing the Add beside them wears.
    { kind: "separator", key: "s1" },
    {
      key: "expand",
      label: "Expand all",
      icon: <KeyboardArrowDownGlyph />,
      onSelect: handlers.expandAll,
    },
    {
      key: "collapse",
      label: "Collapse all",
      icon: <KeyboardArrowUpGlyph />,
      onSelect: handlers.collapseAll,
    },
    { kind: "separator", key: "s2" },
    {
      key: "discard",
      label: "Discard changes",
      icon: <DeleteGlyph />,
      onSelect: handlers.discard,
      disabled: readOnly || !changed,
      danger: true,
    },
  ];

  return (
    <BuilderContext.Provider value={api}>
      <div className={cx("org-builder-body", fill && "fill")}>
        {!startingCompany && (
          <div className="org-builder-toolbar" role="toolbar" aria-label="Organization builder">
            <SegmentedControl
              label="Builder view"
              semantics="tabs"
              panelId={viewPanel}
              value={view}
              onValueChange={setView}
              size="sm"
              options={[
                { value: "visualization", label: "Visualization", icon: <AccountTreeGlyph /> },
                // `ListGlyph` rather than a table glyph: `@crewlethq/icons`
                // ships none, and a rows-of-records mark is what this one is.
                { value: "table", label: "Table", icon: <ListGlyph /> },
              ]}
            />
            <span className="org-builder-wide row gap-1">
              <IconButton
                label="Undo"
                icon={<UndoGlyph />}
                size="sm"
                onClick={handlers.undo}
                disabled={!canUndo}
                aria-keyshortcuts="Control+Z Meta+Z"
              />
              <IconButton
                label="Redo"
                icon={<RedoGlyph />}
                size="sm"
                onClick={handlers.redo}
                disabled={!canRedo}
                aria-keyshortcuts="Shift+Control+Z Shift+Meta+Z"
              />
              {/* THE PAIR LIVES HERE, ON BOTH VIEWS. The table's rows close
                  as the chart's cards do (a unit's row carries its own
                  chevron), so the guard that kept these two off it was wrong
                  about what it draws, and the design system's own pair over
                  the table was a second implementation of one action: two
                  buttons that moved from the toolbar to the table's top edge
                  when the reader changed view. `TableView` passes
                  `controls={false}` so this is the only one. */}
              <Button size="small" variant="tertiary" onClick={handlers.expandAll}>
                Expand all
              </Button>
              <Button size="small" variant="tertiary" onClick={handlers.collapseAll}>
                Collapse all
              </Button>
            </span>
            <span className="org-builder-narrow">
              <Menu label="More builder actions" items={more} />
            </span>
            <Menu
              label={selectedView ? `Actions for ${selectedName}` : "Add to the organization"}
              icon={selectedView ? <MoreVertGlyph /> : <AddGlyph />}
              items={toolbarItems}
              trigger={selectedView ? selectedName : "Add"}
            />
            <span className="spacer" />
            {/* ON THE TABLE ONLY: the visualization draws it at the end of the
                chart's own zoom bar, where the console chart keeps it. */}
            {view === "table" && <FullscreenToggle container={container} />}
            <Tag variant={look.tone} leadingIcon={<look.icon />}>
              {look.label}
            </Tag>
            <span className="org-builder-wide">
              <Button
                size="small"
                variant="tertiary"
                onClick={handlers.discard}
                disabled={readOnly || !changed}
              >
                Discard changes
              </Button>
            </span>
            <Button
              size="small"
              variant="primary"
              leadingIcon={<SaveGlyph />}
              onClick={openReview}
              disabled={!rules.review || save.unsettled}
              title={rules.reason ?? undefined}
            >
              Review and save
            </Button>
          </div>
        )}

        {(posture.kind === "guarded" || status === "guarded") && (
          <Callout
            variant="danger"
            icon={<KeyGlyph />}
            action={
              <Button variant="secondary" size="small" onClick={askForToken}>
                Set token
              </Button>
            }
          >
            {tokenStored
              ? "The engine refused this browser's token. Your draft is kept on this page; set a token the engine accepts to keep editing."
              : "Editing the organization needs an operator token. Your draft is kept on this page."}
          </Callout>
        )}
        {posture.kind === "unreachable" && (
          <Callout variant="warning" icon={<CableGlyph />}>
            The engine could not be reached to read the configuration again. Your draft is kept on
            this page.
          </Callout>
        )}
        {(posture.kind === "failed" ||
          posture.kind === "unserved" ||
          posture.kind === "behind") && (
          <Callout
            variant="warning"
            action={
              <Button variant="secondary" size="small" onClick={() => load()}>
                Retry
              </Button>
            }
          >
            The configuration could not be read again, so this draft stands on the revision it was
            started from.
          </Callout>
        )}
        {/* KEPT TO READ, NEVER TO SAVE. Every check and save of this draft is
            refused while the lens is read-only over it, so the way on stays
            on screen after the dialog that first offered it is closed. */}
        {conflict && conflict.reason === "already_configured" && !companyExists && (
          <Callout
            variant="warning"
            icon={<WarningGlyph />}
            action={
              <Button size="small" variant="primary" onClick={openExistingCompany}>
                Discard it and open the company
              </Button>
            }
          >
            A company was created on this engine while this draft was being written, so this draft
            cannot be saved.
          </Callout>
        )}
        {conflict && conflict.reason === "no_active_revision" && (
          <Callout
            variant="warning"
            icon={<WarningGlyph />}
            action={
              <Button
                variant="secondary"
                size="small"
                onClick={() => {
                  dispatchRaw({ type: "discard" });
                  load(true);
                }}
              >
                Discard and reload
              </Button>
            }
          >
            The configuration this draft edits is no longer active on this engine, so the draft
            cannot be saved.
          </Callout>
        )}
        {conflict && state.mode === "edit" && conflict.reason !== "no_active_revision" && (
          <Callout
            variant="warning"
            icon={<WarningGlyph />}
            action={
              <span className="row gap-1 wrap">
                {/* WHAT CHANGED SINCE THE DRAFT'S BASE is the newer revision
                    against that base. The Configuration screen compares with
                    the active revision unless told otherwise, and the base
                    against the active one reads every change backwards. */}
                {state.base.revision && conflict.currentRevisionId && (
                  <ButtonLink
                    size="small"
                    variant="tertiary"
                    href={href(screenPath("config"), {
                      lens: "diff",
                      revision: conflict.currentRevisionId,
                      against: state.base.revision,
                    })}
                  >
                    Show what changed
                  </ButtonLink>
                )}
                <Button
                  size="small"
                  variant="primary"
                  disabled={updateNote.busy}
                  onClick={() => void beginUpdate(conflict.currentRevisionId)}
                >
                  Update my draft
                </Button>
              </span>
            }
          >
            The configuration changed since you started editing.
            {updateNote.message && <span className="org-builder-note">{updateNote.message}</span>}
          </Callout>
        )}
        {loaded && !isBaseKeyed(state) && status === "unreachable" && (
          <Callout variant="warning" icon={<CableGlyph />}>
            The engine could not be reached to describe this company. Editing starts once it
            answers.
          </Callout>
        )}
        {keeping.offer && (
          <Callout
            variant="info"
            icon={<SaveGlyph />}
            action={
              <span className="row gap-1 wrap">
                <Button variant="secondary" size="small" onClick={keeping.discard}>
                  Discard it
                </Button>
                <Button size="small" variant="primary" onClick={keeping.keep}>
                  Keep the draft
                </Button>
              </span>
            }
          >
            This tab kept a draft with {plural(keeping.offer.ops.length, "change")}, last changed{" "}
            {fmtDateTime(new Date(keeping.offer.savedAt).toISOString())}. Keep it to go on editing,
            or discard it to start from the saved configuration.
          </Callout>
        )}
        {keeping.notice && (
          <Callout
            variant={keeping.notice.tone}
            action={
              <Button size="small" variant="tertiary" onClick={keeping.dismissNotice}>
                Dismiss
              </Button>
            }
          >
            {keeping.notice.message}
          </Callout>
        )}
        {noProvider && (
          <Callout variant="warning" icon={<MemoryGlyph />}>
            No model provider is configured, so no agent seat takes a turn: work sent to a seat
            waits on its inbox until one is added. The dashboard does not write providers: add one
            with <InlineCode>crewlet config import</InlineCode> or{" "}
            <InlineCode>PATCH /config</InlineCode>.
          </Callout>
        )}
        {refusal && (
          <Callout
            variant="warning"
            action={
              <Button size="small" variant="tertiary" onClick={() => setRefusal(null)}>
                Dismiss
              </Button>
            }
          >
            {refusal}
          </Callout>
        )}
        {documentProblems.length > 0 && <DocumentProblems problems={documentProblems} />}

        {savedRevision && <AfterSaveStrip saved={savedRevision} onDismiss={clearSavedRevision} />}

        {save.unsettled && !reviewing && (
          <Callout
            variant="warning"
            action={
              <span className="row gap-1 wrap">
                <Button variant="secondary" size="small" onClick={() => void save.checkAgain()}>
                  Check again
                </Button>
                {/* A save a previous visit sent has no draft on screen to
                    review: its log waits in storage until the save is known. */}
                {changed && (
                  <Button size="small" variant="primary" onClick={openReview}>
                    Open the review
                  </Button>
                )}
              </span>
            }
          >
            The engine did not confirm whether the last save was stored. Editing is paused until it
            does.
          </Callout>
        )}

        {created && <NextSteps onDismiss={() => setCreated(false)} />}

        {startingCompany ? (
          <CreateCompany
            keys={keys}
            disabled={readOnly}
            onApply={(intent) => dispatch({ type: "record", intent })}
          />
        ) : (
          <TabPanel id={viewPanel} value={view}>
            {view === "visualization" ? (
              <TabPanel id={chartPanel} value={chart}>
                <Canvas
                  chart={chart}
                  chrome={chartChrome}
                  about={about}
                  adding={
                    adding === null
                      ? null
                      : {
                          parent: adding.parent,
                          ...(adding.kind ? { kind: adding.kind } : {}),
                          opening: adding.opening,
                          onClose: closeDialog,
                        }
                  }
                />
              </TabPanel>
            ) : (
              <Table />
            )}
          </TabPanel>
        )}

        {reviewing && save.writeId && (
          <ReviewPanel
            state={state}
            status={status}
            rules={rules}
            writeId={save.writeId}
            phase={save.phase}
            onSave={(summary) => void save.save(summary)}
            onCheckAgain={() => void save.checkAgain()}
            onClose={() => {
              save.acknowledge();
              setReviewing(false);
            }}
          />
        )}

        {pending && (
          <UpdateDraftDialog
            update={pending}
            describe={(op) =>
              describeOperation(op, pending.restoring ? pending.baseDraft : state.draft)
            }
            nameOf={(key) => nameIn(key, [state.draft, state.baseDraft, pending.baseDraft])}
            onChoose={(index, choice) => dispatchRaw({ type: "updateChoose", index, choice })}
            onConfirm={confirmUpdate}
            onCancel={cancelUpdate}
          />
        )}

        {leaving && (
          <Modal
            open
            stackBody
            title="Leave the builder?"
            icon={<WarningGlyph />}
            onClose={() => setLeaving(null)}
            footer={
              <>
                <Button variant="secondary" onClick={() => setLeaving(null)}>
                  Stay
                </Button>
                <Button
                  variant="danger"
                  onClick={() => {
                    const { leave } = leaving;
                    setLeaving(null);
                    leave();
                  }}
                >
                  Leave without the draft
                </Button>
              </>
            }
          >
            <p>
              This browser cannot keep the draft, so leaving the builder discards every change in
              it. Save it first, or leave without it.
            </p>
          </Modal>
        )}

        {companyExists && (
          <Modal
            open
            stackBody
            title="A company already exists on this engine"
            icon={<WarningGlyph />}
            onClose={() => setCompanyExists(false)}
            footer={
              <>
                <Button variant="secondary" onClick={() => setCompanyExists(false)}>
                  Keep my draft
                </Button>
                <Button variant="danger" onClick={openExistingCompany}>
                  Discard it and open the company
                </Button>
              </>
            }
          >
            <p>
              A company was created on this engine while this draft was being written. A draft that
              starts a company cannot be applied to one that exists, and it is never replayed onto
              it: open the company and make the changes there.
            </p>
          </Modal>
        )}

        {surface && (
          <DialogHost
            dialog={surface}
            follow={follow}
            surfaces={surfaces}
            onClose={closeDialog}
            onDiscard={() => {
              dispatch({ type: "discard" });
              closeDialog();
            }}
          />
        )}

        {/* A HOOK, NOT A STYLE. `sr-only` does the hiding; this attribute only
            names the region so a case can find it. Written as data rather than
            as a class because a class that declares nothing reads as a missing
            rule to the next person, and to the scan that looks for one. */}
        <div className="sr-only" data-live-region role="status" aria-live="polite">
          {live.text}
        </div>
      </div>
    </BuilderContext.Provider>
  );
}

/** The review, with the changes derived from the draft as it stands. */
function ReviewPanel({
  state,
  status,
  rules,
  writeId,
  phase,
  onSave,
  onCheckAgain,
  onClose,
}: {
  state: BuilderState;
  status: CheckStatus;
  rules: ReturnType<typeof saveRules>;
  writeId: string;
  phase: Parameters<typeof ReviewSaveDialog>[0]["phase"];
  onSave: (summary: string) => void;
  onCheckAgain: () => void;
  onClose: () => void;
}) {
  const current = state.check.generation === state.generation;
  const changes = useMemo(
    () =>
      deriveChanges({
        base: { draft: state.baseDraft, derived: state.base.derived },
        next: { draft: state.draft, derived: current ? state.check.derived : null },
        ops: state.log.ops,
        reports: state.reports,
      }),
    [state, current],
  );
  const outcome = current ? state.check.outcome : null;
  const needsContact = useMemo(
    () =>
      seatsNeedingContact(state.draft).map((key) => locate(state.draft, key)?.node.data.name ?? ""),
    [state.draft],
  );
  return (
    <ReviewSaveDialog
      mode={state.mode}
      changes={changes}
      rules={rules}
      status={status}
      warnings={outcome?.status === "clean" ? outcome.warnings : []}
      problemCount={current ? state.check.problems.problemCount : 0}
      documentProblems={current ? state.check.problems.document : []}
      needsContact={needsContact}
      writeId={writeId}
      phase={phase}
      onSave={onSave}
      onCheckAgain={onCheckAgain}
      onClose={onClose}
    />
  );
}

/**
 * A node's name, from the first draft that holds its key. A conflict's keys
 * name nodes of the draft, of the base it stood on or of the newer revision,
 * and a key is the same node in all three.
 */
function nameIn(key: NodeKey, drafts: readonly Draft[]): string | null {
  for (const draft of drafts) {
    const found = locate(draft, key);
    if (found) return found.node.data.name || null;
  }
  return null;
}

/** Whether keys pressed in an element edit text there. */
function isTextEntry(el: HTMLElement): boolean {
  if (el.isContentEditable) return true;
  if (el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement) return true;
  if (!(el instanceof HTMLInputElement)) return false;
  return !["checkbox", "radio", "button", "submit", "reset", "range", "color", "file"].includes(
    el.type,
  );
}

function DialogHost({
  dialog,
  follow,
  surfaces,
  onClose,
  onDiscard,
}: {
  dialog: OpenDialog;
  /** Reads a key the dialog holds through the base's last keying (`state.rekeyed`). */
  follow: (key: NodeKey) => NodeKey;
  surfaces: BuilderSurfaces;
  onClose: () => void;
  onDiscard: () => void;
}) {
  switch (dialog.type) {
    case "discard":
      /*
       * ONE QUESTION, ONE ANSWER, in the shape every other confirmation in
       * this lens takes: the node editor asks the same thing and asks it as a
       * prompt. Written here as a framed Modal it carried a head band, a
       * pencil-sized mark and a close control that did exactly what the Keep
       * editing two inches below it did, so one question was drawn two ways on
       * one screen. `destructive` makes it an alertdialog, which asks a
       * reader's software to announce the consequence with the name.
       */
      return (
        <ConfirmModal
          open
          destructive
          title="Discard changes?"
          message="Every change in this draft is discarded. The saved configuration is not touched."
          cancelLabel="Keep editing"
          confirmLabel="Discard changes"
          onClose={onClose}
          onConfirm={onDiscard}
        />
      );
    case "add": {
      const Add = surfaces.add;
      return (
        <Add
          key={dialog.opening}
          parent={dialog.parent === null ? null : follow(dialog.parent)}
          kind={dialog.kind}
          onClose={onClose}
        />
      );
    }
    case "editor": {
      const Editor = surfaces.editor;
      return (
        <Editor
          key={dialog.opening}
          nodeKey={follow(dialog.key)}
          section={dialog.section}
          onClose={onClose}
        />
      );
    }
    default: {
      const Node = surfaces[dialog.type];
      return <Node key={dialog.opening} nodeKey={follow(dialog.key)} onClose={onClose} />;
    }
  }
}

function DocumentProblems({ problems }: { problems: readonly PlacedProblem[] }) {
  return (
    <Callout
      variant="danger"
      className="org-builder-problems"
      role="group"
      aria-label="Problems with the whole configuration"
      icon={<ErrorGlyph size="sm" />}
    >
      <ul className="col gap-1">
        {problems.map((p, i) => (
          <li key={i}>
            <span>{p.message}</span>
            {p.link === "integrations" && (
              <>
                {" "}
                <a className="t-link" href={href(screenPath("integrations"))}>
                  Open Integrations
                </a>
              </>
            )}
            {p.link === "schedules" && (
              <>
                {" "}
                <a className="t-link" href={href(screenPath("schedules"))}>
                  Open Schedules
                </a>
              </>
            )}
          </li>
        ))}
      </ul>
    </Callout>
  );
}

function PostureScreen({
  posture,
  onRetry,
  onSetToken,
}: {
  posture: Posture;
  onRetry: () => void;
  onSetToken: () => void;
}) {
  const retry = (
    <Button variant="secondary" onClick={onRetry}>
      Retry
    </Button>
  );
  switch (posture.kind) {
    case "loading":
    case "edit":
    case "create":
      return <Skeleton label="Loading the company" variant="text" rows={4} />;
    case "behind":
      return (
        <EmptyState
          icon={<RefreshGlyph />}
          title="This node has not caught up with the fleet's configuration yet."
          description="The fleet already runs a company. Try again once this node has applied its revision, or open the dashboard on another node."
          action={retry}
        />
      );
    case "guarded":
      return (
        <EmptyState
          icon={<KeyGlyph />}
          title={
            posture.tokenStored
              ? "The engine refused this browser's token."
              : "Editing the organization needs an operator token."
          }
          description="The configuration is guarded, reads included."
          action={
            <Button variant="primary" onClick={onSetToken}>
              Set token
            </Button>
          }
        />
      );
    case "unserved":
      return (
        <EmptyState
          icon={<DnsGlyph />}
          title="This process does not serve the configuration"
          description="The builder reads and writes the company through the node that runs it. Open the dashboard on a node running the engine."
        />
      );
    case "unreachable":
      return (
        <EmptyState
          icon={<CableGlyph />}
          title="The engine could not be reached"
          description={posture.detail || "Nothing answered the request for the configuration."}
          action={retry}
        />
      );
    case "failed":
      return (
        <EmptyState
          icon={<ErrorGlyph />}
          title="The engine could not serve the configuration"
          description={posture.detail}
          action={retry}
        />
      );
  }
}

// ---------------------------------------------------------------------------
// Fullscreen
// ---------------------------------------------------------------------------

function fullscreenSupported(el: HTMLElement | null): boolean {
  return (
    !!el &&
    typeof el.requestFullscreen === "function" &&
    typeof document !== "undefined" &&
    document.fullscreenEnabled === true
  );
}

function useFullscreenExit(): () => void {
  return useCallback(() => {
    if (document.fullscreenElement && typeof document.exitFullscreen === "function") {
      void document.exitFullscreen().catch(() => {});
    }
  }, []);
}

/**
 * Fullscreen for the builder container, where the browser offers it. The
 * control is not drawn where the API is missing (iPhone Safari), rather than
 * drawn and failing.
 */
function FullscreenToggle({ container }: { container: RefObject<HTMLDivElement | null> }) {
  const [supported, setSupported] = useState(false);
  const [active, setActive] = useState(false);
  useEffect(() => {
    setSupported(fullscreenSupported(container.current));
    const onChange = () => setActive(document.fullscreenElement === container.current);
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, [container]);
  if (!supported) return null;
  return (
    <IconButton
      label={active ? "Leave fullscreen" : "Fullscreen"}
      icon={active ? <FullscreenExitGlyph /> : <FullscreenGlyph />}
      size="sm"
      onClick={() => {
        const el = container.current;
        if (!el) return;
        if (active) void document.exitFullscreen().catch(() => {});
        else void el.requestFullscreen().catch(() => {});
      }}
    />
  );
}
