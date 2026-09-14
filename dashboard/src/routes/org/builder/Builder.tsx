/**
 * The Builder lens of the Org chart screen (`#/org?lens=builder`): editing the
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
 * A first dry run answering 503 `no_control_plane` makes the lens read-only:
 * the process can read the configuration and cannot write it. A token change
 * mid-edit re-reads without discarding the draft: the draft and its log stay
 * on screen, the check runs again under the new token, and a refusal pauses
 * editing rather than throwing the work away.
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
  type RefObject,
} from "react";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useAgents, useConnection, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { apiToken, onTokenChanged, requestToken } from "~/protocol/index.ts";
import type { ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { Kbd } from "~/ui/Kbd.tsx";
import { Menu, type MenuEntry } from "~/ui/Menu.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { isComposing } from "~/ui/keys.ts";
import {
  Badge,
  Banner,
  Button,
  ButtonLink,
  Empty,
  Segmented,
  Skeleton,
  TabPanel,
  cx,
  type Tone,
} from "~/ui/primitives.tsx";
import { ToastProvider, useToast } from "~/ui/Toast.tsx";
import { isModalOpen } from "~/ui/useModal.ts";
import {
  BuilderContext,
  type AddKind,
  type BuilderApi,
  type BuilderViewHandle,
} from "./BuilderContext.tsx";
import { handlesByKey } from "./model/document.ts";
import { allUnits, locate } from "./model/draft.ts";
import { COMPANY_KEY, handleOfKey, seatKey, type NodeKey } from "./model/keys.ts";
import type { PlacedProblem } from "./model/problems.ts";
import {
  builderReducer,
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

// ---------------------------------------------------------------------------
// Surfaces
// ---------------------------------------------------------------------------

/** What a dialog about one node is given. */
export interface NodeDialogProps {
  nodeKey: NodeKey;
  onClose: () => void;
}

/** What the Add dialog is given. */
export interface AddDialogProps {
  /** A unit's key, or `null` for the company root. */
  parent: NodeKey | null;
  kind?: AddKind;
  onClose: () => void;
}

/**
 * Which chart the canvas draws. The Builder owns the `chart` section param
 * (its toolbar is where it is chosen), so the canvas is HANDED the answer
 * rather than reading the URL a second time, where the two could disagree.
 */
export type BuilderChart = "structure" | "reporting";

/**
 * The views and dialogs the Builder hosts. They read and act through
 * `BuilderContext`; the Builder decides which is mounted.
 *
 * A surface this build does not carry is `null`, and the Builder says so where
 * it would have drawn it rather than drawing nothing.
 */
export interface BuilderSurfaces {
  canvas: ComponentType<{ chart: BuilderChart }> | null;
  outline: ComponentType | null;
  editor: ComponentType<NodeDialogProps> | null;
  add: ComponentType<AddDialogProps> | null;
  move: ComponentType<NodeDialogProps> | null;
  remove: ComponentType<NodeDialogProps> | null;
  changeKind: ComponentType<NodeDialogProps> | null;
}

type OpenDialog =
  | { readonly type: "editor" | "move" | "remove" | "changeKind"; readonly key: NodeKey }
  | { readonly type: "add"; readonly parent: NodeKey | null; readonly kind?: AddKind }
  | { readonly type: "discard" };

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
  readonly icon: IconName;
}

function statusLook(status: CheckStatus, problems: number): StatusLook {
  switch (status) {
    case "checking":
      return { label: "Checking", tone: "neutral", icon: "refresh" };
    case "clean":
      return { label: "No problems", tone: "positive", icon: "check" };
    case "problems":
      return { label: plural(problems, "problem"), tone: "critical", icon: "alert" };
    case "unreachable":
      return { label: "Could not reach the engine to check", tone: "caution", icon: "plug" };
    case "readonly":
      return { label: "Read-only here", tone: "neutral", icon: "eye" };
    case "conflict":
      return { label: "The configuration changed", tone: "caution", icon: "alert" };
    case "guarded":
      return { label: "The engine refused the token", tone: "critical", icon: "key" };
  }
}

/** The conflict the last check of the current draft reported, or `null`. */
function conflictOf(state: BuilderState): Extract<CheckOutcome, { status: "conflict" }> | null {
  const outcome = state.check.generation === state.generation ? state.check.outcome : null;
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

/** The engine's handle for every seat of the draft, from a check of this very draft. */
function currentHandles(state: BuilderState): ReadonlyMap<NodeKey, string> {
  if (state.check.generation !== state.generation || !state.check.sent) return new Map();
  return handlesByKey(state.check.sent, state.check.derived);
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
  const handle = handleOfKey(key) ?? currentHandles(state).get(key);
  return handle ? { unit: "", seat: handle } : null;
}

/** The node the filters name, or `null`. */
function keyOfParams(state: BuilderState, params: SelectionParams): NodeKey | null {
  if (params.seat) {
    const direct = seatKey(params.seat);
    if (locate(state.draft, direct)) return direct;
    for (const [key, handle] of currentHandles(state)) if (handle === params.seat) return key;
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
 * Below this width the drawer takes the whole window (`components.css`), and
 * a canvas has no room beside it, so a lens opened with no `view` starts on
 * the outline.
 */
const NARROW_QUERY = "(max-width: 860px)";

/** Undo and redo, as the keyboard handler below accepts them. */
const UNDO_KEYS = ["Mod", "z"] as const;
const REDO_KEYS = ["Mod", "Shift", "z"] as const;

function prefersOutline(): boolean {
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
  const [narrowDefault] = useState(prefersOutline);
  const [viewParam, setView] = useParam("view", narrowDefault ? "outline" : "canvas", "section");
  const view = viewParam === "outline" ? "outline" : "canvas";
  const [chartParam, setChart] = useParam("chart", "structure", "section");
  const chart = chartParam === "reporting" ? "reporting" : "structure";
  const [unitParam] = useParam("unit", "", "filter");
  const [seatParam] = useParam("seat", "", "filter");
  const viewPanel = useId();
  const chartPanel = useId();

  const [state, dispatchRaw] = useReducer(builderReducer, INITIAL_BUILDER);
  const stateRef = useRef(state);
  stateRef.current = state;
  const [loaded, setLoaded] = useState(false);
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
    if (posture.kind === "edit") {
      // A draft with work on it stays on its base: the check reports the newer
      // revision as a conflict, and the operator decides what happens to the
      // work. Without work there is nothing to lose by standing on the new one.
      const stale = current.mode !== "edit" || current.base.revision !== posture.revision;
      if (!loaded || force || (stale && !hasWork)) {
        dispatchRaw({
          type: "load",
          mode: "edit",
          document: posture.document,
          revision: posture.revision,
        });
        setLoaded(true);
      }
    } else if (posture.kind === "create") {
      if (!loaded || force || (current.mode !== "create" && !hasWork)) {
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
  // under the draft: check again rather than wait for the next edit.
  const lastOrg = useRef(org);
  useEffect(() => {
    if (lastOrg.current === org) return;
    lastOrg.current = org;
    if (loaded && stateRef.current.mode === "edit") reset();
  }, [org, loaded, reset]);

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
    onLanded: () => {},
    onConflict: () => {},
    onRefused: () => {},
  });
  const save = useSave({ stateRef, transport, keys, events: saveEvents });

  const readOnlyReason = useMemo((): string | null => {
    if (save.unsettled) return "the outcome of the last save is not known yet";
    if (keeping.offer) return "a kept draft is waiting for Keep or Discard";
    if (posture.kind === "guarded") return "the engine refused this browser's token";
    if (status === "guarded") return "the engine refused this browser's token";
    if (status === "readonly") return "this process cannot write the configuration";
    if (status === "conflict") return "the configuration changed since this draft was started";
    if (loaded && !isBaseKeyed(state)) return "the engine has not described this company yet";
    return null;
  }, [save.unsettled, keeping.offer, posture.kind, status, loaded, state]);
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
      if (e.defaultPrevented || isComposing(e) || isModalOpen()) return;
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

  const [dialog, setDialog] = useState<OpenDialog | null>(null);
  const closeDialog = useCallback(() => setDialog(null), []);
  const openIfWritable = useCallback(
    (next: OpenDialog) => {
      if (readOnlyReason !== null) {
        announce(`Editing is paused because ${readOnlyReason}.`);
        return;
      }
      setDialog(next);
    },
    [readOnlyReason, announce],
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
  // onto that would lose the change the conflict was about.
  const beginUpdate = useCallback(
    async (conflictRevisionId: string | null) => {
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

  // ---- Saving -------------------------------------------------------------------

  const savedRevision = useSavedRevision();
  const [created, setCreated] = useState(false);
  // A company that appeared while a create draft was being written: the draft
  // cannot be applied to it, and must never be replayed onto it.
  const [companyExists, setCompanyExists] = useState(false);
  const [reviewing, setReviewing] = useState(false);
  const reviewAfterUpdate = useRef(false);
  const changed = useMemo(() => hasChanges(state), [state]);
  const rules = saveRules(status, changed);
  const openReview = useCallback(() => {
    save.prepare();
    setReviewing(true);
  }, [save]);

  saveEvents.current = {
    onLanded: (landed) => {
      // Recorded and cleared first: this runs even when the Builder has gone
      // away while the save was in flight, and a kept log of a saved draft
      // would be offered for replay onto its own revision.
      recordSavedRevision({
        revisionId: landed.revisionId,
        parentRevisionId: landed.parentRevisionId,
        epoch: landed.epoch,
      });
      clearDraft(storage);
      dispatchRaw({ type: "saved", revisionId: landed.revisionId, derived: landed.derived });
      setReviewing(false);
      if (landed.mode === "create") setCreated(true);
      // THE TOAST IS THE ANNOUNCEMENT: its host is a polite live region of
      // its own, so saying the same sentence through the Builder's region as
      // well had a screen reader read it twice.
      toast.ok("Saved. The engine is applying it.");
      load(true);
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
      selection: { key: selected, select },
      openEditor: (key) => setDialog({ type: "editor", key }),
      openAdd: (parent, kind) => openIfWritable({ type: "add", parent, kind }),
      openMove: (key) => openIfWritable({ type: "move", key }),
      openDelete: (key) => openIfWritable({ type: "remove", key }),
      openChangeKind: (key) => openIfWritable({ type: "changeKind", key }),
      announce,
      focusNode,
      readOnly,
      agents,
      sandboxes,
      registerView: (handle) => {
        viewHandle.current = handle;
        return () => {
          if (viewHandle.current === handle) viewHandle.current = null;
        };
      },
    };
  }, [
    state,
    problemsCurrent,
    dispatch,
    selected,
    select,
    openIfWritable,
    announce,
    focusNode,
    readOnly,
    agents,
    sandboxes,
  ]);

  // ---- Rendering --------------------------------------------------------------

  if (!loaded) {
    return <PostureScreen posture={posture} onRetry={() => load()} onSetToken={askForToken} />;
  }

  const problemCount = problemsCurrent ? state.check.problems.problemCount : 0;
  const look = statusLook(status, problemCount);
  const canUndo = !readOnly && state.log.ops.length > 0;
  const canRedo = !readOnly && state.log.undone.length > 0;
  // THE CREATE FORM HAS NO TOOLBAR: there is no draft to undo, check or save
  // until a template is recorded. Not rendered rather than hidden with the
  // attribute, because `.org-builder-toolbar` sets `display: flex` and an
  // author rule beats the user agent's `[hidden] { display: none }`.
  const startingCompany = creating && !templateApplied;
  const Canvas = surfaces.canvas;
  const Outline = surfaces.outline;
  const fill = view === "canvas";
  const providers = state.base.document?.providers;
  const llm = isRecord(providers) ? providers.llm : undefined;
  const noProvider = state.mode === "edit" && !(isRecord(llm) && Object.keys(llm).length > 0);
  const documentProblems = problemsCurrent ? state.check.problems.document : [];

  const handlers = {
    undo: () => dispatch({ type: "undo" }),
    redo: () => dispatch({ type: "redo" }),
    expandAll: () => viewHandle.current?.expandAll(),
    collapseAll: () => viewHandle.current?.collapseAll(),
    discard: () => setDialog({ type: "discard" }),
  };
  // THE TOOLBAR MIRRORS THE SELECTED NODE'S ACTIONS. A canvas card's own
  // buttons are pointer-only (a tree item may not contain tab stops), so this
  // is where a keyboard reaches them, and it is also where a node's actions
  // are when the outline is the view.
  const companySelected = selected === COMPANY_KEY;
  const selectedNode = selected && !companySelected ? locate(state.draft, selected) : undefined;
  const selectedName = companySelected
    ? state.draft.company.name || "the company"
    : selectedNode?.node.data.name || "the selected node";
  // READ-ONLY DISABLES, IT DOES NOT HIDE. A menu that loses half its entries
  // teaches an operator nothing about what the builder does, and an entry
  // that is offered and then only answers into the live region tells a
  // sighted operator nothing at all. Edit and Open seat change no draft and
  // stay available.
  const addItems = (parent: NodeKey | null): MenuEntry[] => [
    {
      key: "add-unit",
      label: "Add unit",
      icon: "folder",
      disabled: readOnly,
      onSelect: () => api.openAdd(parent, "unit"),
    },
    {
      key: "add-agent",
      label: "Add agent seat",
      icon: "cpu",
      disabled: readOnly,
      onSelect: () => api.openAdd(parent, "agent"),
    },
    {
      key: "add-human",
      label: "Add human seat",
      icon: "user",
      disabled: readOnly,
      onSelect: () => api.openAdd(parent, "human"),
    },
  ];
  const nodeItems = (): MenuEntry[] => {
    if (!selected || (!selectedNode && !companySelected)) return addItems(null);
    const items: MenuEntry[] = [
      { key: "edit", label: "Edit", icon: "pencil", onSelect: () => api.openEditor(selected) },
    ];
    // The company holds seats and units as a unit does, and its Edit is the
    // charter's. Nothing moves or deletes the document the company IS, so
    // its menu ends there.
    if (companySelected) {
      items.push({ kind: "separator", key: "s" }, ...addItems(null));
      return items;
    }
    const seatHandle = handleOfKey(selected) ?? currentHandles(state).get(selected);
    if (selectedNode!.kind === "unit")
      items.push({ kind: "separator", key: "s" }, ...addItems(selected));
    else if (seatHandle) {
      items.push({
        key: "open",
        label: "Open seat",
        icon: "external",
        onSelect: () => nav.to(["seats", seatHandle]),
      });
      items.push({
        key: "kind",
        label:
          selectedNode!.node.data.kind === "human"
            ? "Change to agent seat"
            : "Change to human seat",
        icon: "users",
        disabled: readOnly,
        onSelect: () => api.openChangeKind(selected),
      });
    }
    items.push(
      { kind: "separator", key: "s2" },
      {
        key: "move",
        label: "Move to",
        icon: "move",
        disabled: readOnly,
        onSelect: () => api.openMove(selected),
      },
      {
        key: "delete",
        label: "Delete",
        icon: "trash",
        danger: true,
        disabled: readOnly,
        onSelect: () => api.openDelete(selected),
      },
    );
    return items;
  };

  const more: MenuEntry[] = [
    {
      key: "undo",
      label: "Undo",
      icon: "undo",
      onSelect: handlers.undo,
      disabled: !canUndo,
      hint: <Kbd keys={UNDO_KEYS} />,
    },
    {
      key: "redo",
      label: "Redo",
      icon: "redo",
      onSelect: handlers.redo,
      disabled: !canRedo,
      hint: <Kbd keys={REDO_KEYS} />,
    },
    { kind: "separator", key: "s1" },
    { key: "expand", label: "Expand all", icon: "plus", onSelect: handlers.expandAll },
    { key: "collapse", label: "Collapse all", icon: "minus", onSelect: handlers.collapseAll },
    { kind: "separator", key: "s2" },
    {
      key: "discard",
      label: "Discard changes",
      icon: "trash",
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
            <Segmented
              ariaLabel="Builder view"
              semantics="tabs"
              panelId={viewPanel}
              value={view}
              onChange={setView}
              size="sm"
              options={[
                { value: "canvas", label: "Canvas", icon: "sitemap" },
                { value: "outline", label: "Outline", icon: "list" },
              ]}
            />
            {view === "canvas" && (
              <Segmented
                ariaLabel="Chart"
                semantics="tabs"
                panelId={chartPanel}
                value={chart}
                onChange={setChart}
                size="sm"
                options={[
                  { value: "structure", label: "Structure" },
                  { value: "reporting", label: "Reporting" },
                ]}
              />
            )}
            <span className="org-builder-wide row gap-1">
              <Button
                size="sm"
                icon="undo"
                title="Undo"
                onClick={handlers.undo}
                disabled={!canUndo}
                aria-keyshortcuts="Control+Z Meta+Z"
              />
              <Button
                size="sm"
                icon="redo"
                title="Redo"
                onClick={handlers.redo}
                disabled={!canRedo}
                aria-keyshortcuts="Shift+Control+Z Shift+Meta+Z"
              />
              <Button size="sm" variant="ghost" onClick={handlers.expandAll}>
                Expand all
              </Button>
              <Button size="sm" variant="ghost" onClick={handlers.collapseAll}>
                Collapse all
              </Button>
            </span>
            <span className="org-builder-narrow">
              <Menu label="More builder actions" items={more} />
            </span>
            <Menu
              label={selected ? `Actions for ${selectedName}` : "Add to the organization"}
              icon={selected ? "more" : "plus"}
              items={nodeItems()}
              size="sm"
            >
              {selected ? selectedName : "Add"}
            </Menu>
            <span className="spacer" />
            <FullscreenToggle container={container} />
            <Badge tone={look.tone} icon={look.icon}>
              {look.label}
            </Badge>
            <span className="org-builder-wide">
              <Button
                size="sm"
                variant="ghost"
                onClick={handlers.discard}
                disabled={readOnly || !changed}
              >
                Discard changes
              </Button>
            </span>
            <Button
              size="sm"
              variant="primary"
              icon="save"
              onClick={openReview}
              disabled={!rules.review || save.unsettled}
              title={rules.reason ?? undefined}
            >
              Review and save
            </Button>
          </div>
        )}

        {(posture.kind === "guarded" || status === "guarded") && (
          <Banner
            tone="critical"
            icon="key"
            action={
              <Button size="sm" onClick={askForToken}>
                Set token
              </Button>
            }
          >
            {apiToken()
              ? "The engine refused this browser's token. Your draft is kept on this page; set a token the engine accepts to keep editing."
              : "Editing the organization needs an operator token. Your draft is kept on this page."}
          </Banner>
        )}
        {posture.kind === "unreachable" && (
          <Banner tone="caution" icon="plug">
            The engine could not be reached to read the configuration again. Your draft is kept on
            this page.
          </Banner>
        )}
        {(posture.kind === "failed" ||
          posture.kind === "unserved" ||
          posture.kind === "behind") && (
          <Banner
            tone="caution"
            action={
              <Button size="sm" onClick={() => load()}>
                Retry
              </Button>
            }
          >
            The configuration could not be read again, so this draft stands on the revision it was
            started from.
          </Banner>
        )}
        {status === "readonly" && (
          <Banner tone="neutral" icon="eye">
            This process cannot write the configuration because it has no coordination store.
          </Banner>
        )}
        {conflict && conflict.reason === "no_active_revision" && (
          <Banner
            tone="caution"
            icon="alert"
            action={
              <Button
                size="sm"
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
          </Banner>
        )}
        {conflict && state.mode === "edit" && conflict.reason !== "no_active_revision" && (
          <Banner
            tone="caution"
            icon="alert"
            action={
              <span className="row gap-1 wrap">
                {/* WHAT CHANGED SINCE THE DRAFT'S BASE is the newer revision
                    against that base. The Configuration screen compares with
                    the active revision unless told otherwise, and the base
                    against the active one reads every change backwards. */}
                {state.base.revision && conflict.currentRevisionId && (
                  <ButtonLink
                    size="sm"
                    variant="ghost"
                    href={href(["config"], {
                      lens: "diff",
                      revision: conflict.currentRevisionId,
                      against: state.base.revision,
                    })}
                  >
                    Show what changed
                  </ButtonLink>
                )}
                <Button
                  size="sm"
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
          </Banner>
        )}
        {loaded && !isBaseKeyed(state) && status === "unreachable" && (
          <Banner tone="caution" icon="plug">
            The engine could not be reached to describe this company. Editing starts once it
            answers.
          </Banner>
        )}
        {keeping.offer && (
          <Banner
            tone="info"
            icon="save"
            action={
              <span className="row gap-1 wrap">
                <Button size="sm" onClick={keeping.discard}>
                  Discard it
                </Button>
                <Button size="sm" variant="primary" onClick={keeping.keep}>
                  Keep the draft
                </Button>
              </span>
            }
          >
            This tab kept a draft with {plural(keeping.offer.ops.length, "change")}, last changed{" "}
            {fmtDateTime(new Date(keeping.offer.savedAt).toISOString())}. Keep it to go on editing,
            or discard it to start from the saved configuration.
          </Banner>
        )}
        {keeping.notice && (
          <Banner
            tone={keeping.notice.tone}
            action={
              <Button size="sm" variant="ghost" onClick={keeping.dismissNotice}>
                Dismiss
              </Button>
            }
          >
            {keeping.notice.message}
          </Banner>
        )}
        {noProvider && (
          <Banner tone="caution" icon="cpu">
            No model provider is configured, so no agent seat can run. The dashboard does not write
            providers: add one with <code className="inline">crewlet config import</code> or{" "}
            <code className="inline">PATCH /config</code>.
          </Banner>
        )}
        {refusal && (
          <Banner
            tone="caution"
            action={
              <Button size="sm" variant="ghost" onClick={() => setRefusal(null)}>
                Dismiss
              </Button>
            }
          >
            {refusal}
          </Banner>
        )}
        {documentProblems.length > 0 && <DocumentProblems problems={documentProblems} />}

        {savedRevision && <AfterSaveStrip saved={savedRevision} onDismiss={clearSavedRevision} />}

        {save.unsettled && !reviewing && (
          <Banner
            tone="caution"
            action={
              <span className="row gap-1 wrap">
                <Button size="sm" onClick={() => void save.checkAgain()}>
                  Check again
                </Button>
                <Button size="sm" variant="primary" onClick={openReview}>
                  Open the review
                </Button>
              </span>
            }
          >
            The engine did not confirm whether the last save was stored. Editing is paused until it
            does.
          </Banner>
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
            {view === "canvas" ? (
              <TabPanel id={chartPanel} value={chart}>
                {Canvas ? <Canvas chart={chart} /> : <MissingSurface name="The canvas" />}
              </TabPanel>
            ) : Outline ? (
              <Outline />
            ) : (
              <MissingSurface name="The outline" />
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

        {state.update && (
          <UpdateDraftDialog
            update={state.update}
            describe={(op) =>
              describeOperation(op, state.update?.restoring ? state.update.baseDraft : state.draft)
            }
            onChoose={(index, choice) => dispatchRaw({ type: "updateChoose", index, choice })}
            onConfirm={confirmUpdate}
            onCancel={cancelUpdate}
          />
        )}

        {companyExists && (
          <Dialog
            title="A company already exists on this engine"
            icon="alert"
            onClose={() => setCompanyExists(false)}
            footer={
              <>
                <Button onClick={() => setCompanyExists(false)}>Keep my draft</Button>
                <Button
                  variant="danger"
                  onClick={() => {
                    setCompanyExists(false);
                    dispatchRaw({ type: "discard" });
                    load(true);
                  }}
                >
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
          </Dialog>
        )}

        {dialog && (
          <DialogHost
            dialog={dialog}
            surfaces={surfaces}
            onClose={closeDialog}
            onDiscard={() => {
              dispatch({ type: "discard" });
              closeDialog();
            }}
          />
        )}

        <div className="sr-only org-builder-live" role="status" aria-live="polite">
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

/** Whether keys pressed in an element edit text there. */
function isTextEntry(el: HTMLElement): boolean {
  if (el.isContentEditable) return true;
  if (el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement) return true;
  if (!(el instanceof HTMLInputElement)) return false;
  return !["checkbox", "radio", "button", "submit", "reset", "range", "color", "file"].includes(
    el.type,
  );
}

/** Where a view this build does not carry would have been drawn. */
function MissingSurface({ name }: { name: string }) {
  return (
    <Empty
      icon="sitemap"
      title={`${name} is not part of this build`}
      hint="Open the Chart or Directory lens to read the organization."
    />
  );
}

function DialogHost({
  dialog,
  surfaces,
  onClose,
  onDiscard,
}: {
  dialog: OpenDialog;
  surfaces: BuilderSurfaces;
  onClose: () => void;
  onDiscard: () => void;
}) {
  switch (dialog.type) {
    case "discard":
      return (
        <Dialog
          title="Discard changes"
          icon="trash"
          onClose={onClose}
          footer={
            <>
              <Button onClick={onClose}>Keep editing</Button>
              <Button variant="danger" onClick={onDiscard}>
                Discard changes
              </Button>
            </>
          }
        >
          <p>Every change in this draft is discarded. The saved configuration is not touched.</p>
        </Dialog>
      );
    case "add": {
      const Add = surfaces.add;
      return Add ? (
        <Add parent={dialog.parent} kind={dialog.kind} onClose={onClose} />
      ) : (
        <MissingDialog onClose={onClose} />
      );
    }
    default: {
      const Node = {
        editor: surfaces.editor,
        move: surfaces.move,
        remove: surfaces.remove,
        changeKind: surfaces.changeKind,
      }[dialog.type];
      return Node ? (
        <Node nodeKey={dialog.key} onClose={onClose} />
      ) : (
        <MissingDialog onClose={onClose} />
      );
    }
  }
}

function MissingDialog({ onClose }: { onClose: () => void }) {
  return (
    <Dialog
      title="Not available"
      onClose={onClose}
      footer={<Button onClick={onClose}>Close</Button>}
    >
      <p>This action is not part of this build of the dashboard.</p>
    </Dialog>
  );
}

function DocumentProblems({ problems }: { problems: readonly PlacedProblem[] }) {
  return (
    <div
      className="banner critical org-builder-problems"
      role="group"
      aria-label="Problems with the whole configuration"
    >
      <Icon name="alert" size="sm" />
      <ul className="col gap-1">
        {problems.map((p, i) => (
          <li key={i}>
            <span>{p.message}</span>
            {p.link === "integrations" && (
              <>
                {" "}
                <a className="t-link" href={href(["integrations"])}>
                  Open Integrations
                </a>
              </>
            )}
            {p.link === "schedules" && (
              <>
                {" "}
                <a className="t-link" href={href(["schedules"])}>
                  Open Schedules
                </a>
              </>
            )}
          </li>
        ))}
      </ul>
    </div>
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
  const retry = <Button onClick={onRetry}>Retry</Button>;
  switch (posture.kind) {
    case "loading":
    case "edit":
    case "create":
      return <Skeleton rows={4} />;
    case "behind":
      return (
        <Empty
          icon="refresh"
          title="This node has not caught up with the fleet's configuration yet."
          hint="The fleet already runs a company. Try again once this node has applied its revision, or open the dashboard on another node."
          action={retry}
        />
      );
    case "guarded":
      return (
        <Empty
          icon="key"
          title={
            posture.tokenStored
              ? "The engine refused this browser's token."
              : "Editing the organization needs an operator token."
          }
          hint="The configuration is guarded, reads included."
          action={
            <Button variant="primary" onClick={onSetToken}>
              Set token
            </Button>
          }
        />
      );
    case "unserved":
      return (
        <Empty
          icon="server"
          title="This process does not serve the configuration. Open the dashboard on a node running the engine."
        />
      );
    case "unreachable":
      return (
        <Empty
          icon="plug"
          title="The engine could not be reached"
          hint={posture.detail || "Nothing answered the request for the configuration."}
          action={retry}
        />
      );
    case "failed":
      return (
        <Empty
          icon="alert"
          title="The engine could not serve the configuration"
          hint={posture.detail}
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
    <Button
      size="sm"
      variant="ghost"
      icon={active ? "minimize" : "maximize"}
      title={active ? "Leave fullscreen" : "Fullscreen"}
      onClick={() => {
        const el = container.current;
        if (!el) return;
        if (active) void document.exitFullscreen().catch(() => {});
        else void el.requestFullscreen().catch(() => {});
      }}
    />
  );
}
