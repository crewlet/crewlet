/**
 * Writing the company's charter where it is read: Company › Principles, each
 * part edited in place in its own card.
 *
 * # Per part, not a page mode
 *
 * The mission, the vision and the standing policies are three statements
 * changed for different reasons, and each save is one configuration revision
 * every agent reads from its next turn. So each card has its own Edit, opens
 * in place and saves on its own: the person rewriting the vision keeps the
 * mission and the policies on screen as the context they are writing against,
 * the revision says which statement moved ("Changed the vision"), and a save
 * carries one part, so a colleague's change to another part is never a
 * conflict. Several parts may be open at once. A page mode would turn every
 * card into a form to change one sentence, and would make one revision of
 * three unrelated edits.
 *
 * # What this file holds
 *
 * [useCharterEditing] runs once for the page and owns everything that is
 * about the page rather than one card: the editing state (`state.ts`), the
 * saves in flight (`save.ts`), the focus each step leaves where it should be,
 * the one question asked before unsaved work is lost (a move away, a reload,
 * a closed tab, or Cancel), the toasts, and the address that says which parts
 * are open (`address.ts`). Each card's controls and editor are built from it
 * by `partEdit.tsx`.
 *
 * # Who may write it
 *
 * The charter is the company document's, so writing it takes what any change
 * to the company takes: Admin, on a document this deployment lets the
 * reader's credential change (ADR-0030; `mayChangeConfig`). A reader without
 * that is shown the charter and nothing to write it with: no Edit, no Add, no
 * control that would only ever say no. A reader who may write it but cannot
 * right now (offline, still being identified) is shown the controls disabled,
 * with the reason.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { ConfirmModal, useToast } from "@crewlethq/ui";
import { useLeaveGuard, useNavigator, useRoute, useUnloadGuard } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import type { EngineHealth } from "~/contract/health.ts";
import { useLastHealth } from "~/lib/useLastHealth.ts";
import { useConfigWriteAccess, type ConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { OrgProjection } from "~/protocol/types.ts";
import { mayWriteCharter } from "./access.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { charterDraft } from "./assistantAction.ts";
import { EDIT_PARAM, editValue, partsIn } from "./address.ts";
import {
  CHARTER_PARTS,
  PART_TITLES,
  charterOf,
  hasProblems,
  normalized,
  isEmptyPart,
  partName,
  partOf,
  problemsOf,
  type CharterPart,
  type PartValue,
} from "./charter.ts";
import { readCharter, savePart } from "./save.ts";
import {
  INITIAL,
  effective,
  isDirty,
  landedView,
  reducer,
  type EditingState,
  type Origin,
} from "./state.ts";
import { failureWords, refusedProblems } from "./words.ts";
import "~/styles/charter.css";

/**
 * Where the focus goes once the next render has drawn it, for a step the
 * editor does not take itself (it takes the focus as it opens).
 */
type FocusTarget =
  /** The part's first field, its caret at the end. */
  | "field"
  /** The first field the engine or the rules refused. */
  | "invalid"
  /** Back to the control that opened the part: its Edit, or its Add when it is unwritten. */
  | "return";

/** What a card's controls and editor are built from. */
export interface Editing {
  readonly org: OrgProjection;
  readonly state: EditingState;
  /** Whether this reader may write the charter at all: the controls exist. */
  readonly mayEdit: boolean;
  /** Whether a control may be pressed now, and why not. */
  readonly access: ConfigWriteAccess;
  /**
   * This node's last health push (`useLastHealth`, kept through a dropped
   * socket): the epoch it applied and its last apply attempt, which say where
   * it stands on a landed save (`state.ts`, [landedView]).
   */
  readonly health: EngineHealth | null;
  /** Open the part; `focus` false opens it without taking the focus (a second part an address names). */
  readonly open: (part: CharterPart, origin: Origin, focus?: boolean) => void;
  readonly change: (part: CharterPart, draft: PartValue) => void;
  /** The policy typed in the new-item box and not yet added. */
  readonly pending: (part: CharterPart, text: string) => void;
  readonly save: (part: CharterPart) => void;
  /** Close the part, asking first when it holds changes. */
  readonly cancel: (part: CharterPart) => void;
  readonly keepMine: (part: CharterPart) => void;
  readonly takeTheirs: (part: CharterPart) => void;
  /** Where the part's editor is drawn, so the focus can be put inside it. */
  readonly editorRef: (part: CharterPart) => (el: HTMLElement | null) => void;
  /** The control that opens the part (its Edit or its Add), so the focus can come back to it. */
  readonly triggerRef: (part: CharterPart) => (el: HTMLElement | null) => void;
}

/** A question asked before work is lost. */
type Asking =
  | { readonly kind: "leave"; readonly leave: () => void }
  | { readonly kind: "cancel"; readonly part: CharterPart };

/**
 * The page's editing of the charter, from the org projection it draws: the
 * state every card is built from, and the one dialog that asks before unsaved
 * writing is lost (it portals, so wherever the page places it is fine).
 */
export function useCharterEditing(org: OrgProjection): {
  readonly editing: Editing;
  readonly dialog: ReactNode;
} {
  const [state, dispatch] = useReducer(reducer, INITIAL);
  const stateRef = useRef(state);
  stateRef.current = state;
  const viewer = useViewer();
  const mayEdit = mayWriteCharter(viewer);
  const access = useConfigWriteAccess();
  const health = useLastHealth();
  const toast = useToast();
  const charter = useMemo(() => charterOf(org), [org]);
  const charterRef = useRef(charter);
  charterRef.current = charter;

  // ---- focus -------------------------------------------------------------

  const editors = useRef(new Map<CharterPart, HTMLElement>());
  const triggers = useRef(new Map<CharterPart, HTMLElement>());
  const focusNext = useRef(new Map<CharterPart, FocusTarget>());
  const refCallbacks = useRef(new Map<string, (el: HTMLElement | null) => void>());
  const refFor = useCallback(
    (store: Map<CharterPart, HTMLElement>, kind: string, part: CharterPart) => {
      const key = `${kind}:${part}`;
      let callback = refCallbacks.current.get(key);
      if (!callback) {
        callback = (el) => {
          if (el) store.set(part, el);
          else store.delete(part);
        };
        refCallbacks.current.set(key, callback);
      }
      return callback;
    },
    [],
  );
  const editorRef = useCallback(
    (part: CharterPart) => refFor(editors.current, "editor", part),
    [refFor],
  );
  const triggerRef = useCallback(
    (part: CharterPart) => refFor(triggers.current, "trigger", part),
    [refFor],
  );
  const [, redraw] = useState(0);
  const focusAfterRender = useCallback((part: CharterPart, target: FocusTarget) => {
    focusNext.current.set(part, target);
    // A request with no state change behind it still needs a render to be taken.
    redraw((n) => n + 1);
  }, []);
  // AFTER EVERY RENDER, the focus each step asked for, once the element it
  // names is drawn. A request whose element is not there yet waits for the
  // render that draws it.
  useLayoutEffect(() => {
    for (const [part, target] of focusNext.current) {
      const el = focusTarget(target, editors.current.get(part), triggers.current.get(part));
      if (!el) continue;
      focusNext.current.delete(part);
      el.focus();
      if (el instanceof HTMLTextAreaElement || el instanceof HTMLInputElement) {
        el.setSelectionRange(el.value.length, el.value.length);
      }
    }
  });

  // ---- the saves ---------------------------------------------------------

  const inflight = useRef(new Map<CharterPart, AbortController>());
  const seeding = useRef(new Map<CharterPart, AbortController>());
  useEffect(() => {
    const running = inflight.current;
    const reading = seeding.current;
    return () => {
      for (const controller of [...running.values(), ...reading.values()]) controller.abort();
    };
  }, []);

  const open = useCallback(
    (part: CharterPart, origin: Origin, focus = true) => {
      // AN OPEN PART ASKED FOR AGAIN is only shown: the editor took the focus
      // as it opened, and takes it again here.
      if (stateRef.current.sessions[part]) {
        if (focus) focusAfterRender(part, "field");
        return;
      }
      dispatch({ type: "open", part, base: partOf(charterRef.current, part), origin, focus });
      // OPENED FROM THE PAGE'S COPY, which the engine's can be ahead of: the
      // engine's own charter is read at once, and an untouched draft starts
      // from it (`state.ts`, "seed"). A read that fails leaves the page's
      // copy as the base, which a save then checks against the engine.
      const controller = new AbortController();
      seeding.current.get(part)?.abort();
      seeding.current.set(part, controller);
      void readCharter(controller.signal)
        .then((read) => {
          if (read.kind === "charter") {
            dispatch({ type: "seed", part, value: partOf(read.charter, part) });
          }
        })
        .catch((err: unknown) => {
          if (!isAbort(err)) throw err;
        })
        .finally(() => {
          if (seeding.current.get(part) === controller) seeding.current.delete(part);
        });
    },
    [focusAfterRender],
  );

  const pending = useCallback((part: CharterPart, text: string) => {
    dispatch({ type: "pending", part, text });
  }, []);

  const change = useCallback((part: CharterPart, draft: PartValue) => {
    dispatch({ type: "change", part, draft });
  }, []);

  // --- assistant page actions (WP12) ---
  // THE OPEN PARTS, declared once (`useAssistantDraft`): every part's value
  // reaches the assistant, and a new value for an open part is set through
  // the editor's own `change`, as typing writes it; Save stays theirs.
  useAssistantDraft(
    charterDraft({
      state,
      charter,
      change,
      blocked: () =>
        !mayEdit ? "Writing the principles takes Admin." : access.can ? null : access.reason,
    }),
  );
  // --- end assistant page actions (WP12) ---

  const close = useCallback(
    (part: CharterPart) => {
      inflight.current.get(part)?.abort();
      seeding.current.get(part)?.abort();
      dispatch({ type: "close", part });
      focusAfterRender(part, "return");
    },
    [focusAfterRender],
  );

  const save = useCallback(
    (part: CharterPart) => {
      const session = stateRef.current.sessions[part];
      // THE EDITOR HOLDS SAVE while nothing changed or a conflict stands
      // (`PartEditor.tsx`); a press that reaches here anyway sends nothing.
      if (!session || (session.phase.kind !== "editing" && session.phase.kind !== "failed")) return;
      if (!isDirty(session)) return;
      const draft = effective(session);
      if (hasProblems(problemsOf(part, draft, { was: session.base }))) {
        focusAfterRender(part, "invalid");
        return;
      }
      const title = PART_TITLES[part];
      const controller = new AbortController();
      inflight.current.set(part, controller);
      dispatch({ type: "saving", part });
      void (async () => {
        try {
          const outcome = await savePart({ part, base: session.base, draft }, controller.signal);
          switch (outcome.kind) {
            case "saved":
              dispatch({
                type: "saved",
                part,
                value: normalized(draft),
                activated: outcome.activated,
                shownWas: partOf(charterRef.current, part),
              });
              focusAfterRender(part, "return");
              toast.ok("Every agent reads it from its next turn.", { title: `${title} saved` });
              return;
            case "unchanged":
              dispatch({
                type: "saved",
                part,
                value: normalized(draft),
                activated: null,
                shownWas: partOf(charterRef.current, part),
              });
              focusAfterRender(part, "return");
              toast.ok(`The company already holds ${partName(part)} as written.`, {
                title: "Nothing to save",
              });
              return;
            case "conflict":
              // NO TOAST AND NO FOCUS MOVE: the question is an alert inside
              // the card, read out as it appears, and the person's place in
              // their draft is theirs to keep.
              dispatch({ type: "conflict", part, theirs: outcome.theirs });
              return;
            default: {
              const message = failureWords(outcome, part);
              const refused = refusedProblems(outcome, part);
              // SAID ONCE, BESIDE THE SAVE THAT CAUSED IT: the editor's own
              // alert carries it, and a toast saying it again would be read
              // twice. The card is open whenever an answer arrives (closing
              // it or leaving the page aborts the save), so nobody misses it.
              dispatch({ type: "failed", part, message, refused });
              if (refused) focusAfterRender(part, "invalid");
            }
          }
        } catch (err) {
          // AN ABORT IS THE PAGE GOING AWAY or the part being closed, which
          // nobody is waiting on an answer for.
          if (isAbort(err)) return;
          const reason = (err instanceof Error ? err.message : String(err)).replace(/\.+$/, "");
          dispatch({
            type: "failed",
            part,
            message: `The change could not be sent: ${reason}.`,
            refused: null,
          });
        } finally {
          if (inflight.current.get(part) === controller) inflight.current.delete(part);
        }
      })();
    },
    [close, focusAfterRender, toast],
  );

  const [asking, setAsking] = useState<Asking | null>(null);
  // THE QUESTION KEEPS ITS WORDS while the dialog closes, rather than
  // emptying under the reader's eyes as it animates out.
  const [question, setQuestion] = useState<{
    readonly leaving: boolean;
    readonly parts: readonly CharterPart[];
    /** The part was unwritten before this edit, so nothing goes back to anything. */
    readonly unwritten: boolean;
  }>({ leaving: false, parts: [], unwritten: false });
  const ask = useCallback((next: Asking, parts: readonly CharterPart[], unwritten = false) => {
    setAsking(next);
    setQuestion({ leaving: next.kind === "leave", parts, unwritten });
  }, []);

  const cancel = useCallback(
    (part: CharterPart) => {
      const session = stateRef.current.sessions[part];
      // NOT WHILE IT SAVES: the write is already on its way and may land
      // whatever this page does, so closing would only hide its outcome.
      if (!session || session.phase.kind === "saving") return;
      if (isDirty(session)) ask({ kind: "cancel", part }, [part], isEmptyPart(session.base));
      else close(part);
    },
    [ask, close],
  );

  const keepMine = useCallback(
    (part: CharterPart) => {
      dispatch({ type: "keep_mine", part });
      focusAfterRender(part, "field");
    },
    [focusAfterRender],
  );

  const takeTheirs = useCallback(
    (part: CharterPart) => {
      dispatch({ type: "use_theirs", part, shownWas: partOf(charterRef.current, part) });
      focusAfterRender(part, "return");
    },
    [focusAfterRender],
  );

  // A LANDED SAVE IS DROPPED once the page reads it back, or this node is
  // done with its revision, so the card shows the projection again, and with
  // it any later change. A refused one is kept, for the card to say so beside
  // the projection, until either happens.
  useEffect(() => {
    for (const part of CHARTER_PARTS) {
      const landed = state.landed[part];
      if (landed && landedView(landed, partOf(charter, part), health) === "settled") {
        dispatch({ type: "settle", part });
      }
    }
  }, [state.landed, charter, health]);

  // ---- unsaved work ------------------------------------------------------

  const dirty = CHARTER_PARTS.filter((part) => isDirty(state.sessions[part]));
  useUnloadGuard(dirty.length > 0);
  useLeaveGuard(
    dirty.length > 0
      ? (to, leave) => {
          // THIS PAGE WRITING ITS OWN ADDRESS (a part opening or closing) is
          // not a move away from it.
          // Asked of the router's own reading of the address, so the bare
          // Company landing (`#/company`) and `#/company/principles`, which
          // are this one screen, are both this page.
          const where = resolve([...to.path]);
          if (where.resolved && where.screen === "principles") return false;
          ask({ kind: "leave", leave }, dirty);
          return true;
        }
      : null,
  );

  // ---- the address -------------------------------------------------------

  const route = useRoute();
  const nav = useNavigator();
  const param = route.query.get(EDIT_PARAM);
  const openKey = editValue(CHARTER_PARTS.filter((part) => state.sessions[part]));
  const lastOpen = useRef(openKey);
  // WHAT IS OPEN IS WRITTEN TO THE ADDRESS, by replacing the entry, whenever
  // a part opens or closes. Only then: an address arriving is read by the
  // effect below and never written over before it has been.
  useEffect(() => {
    if (lastOpen.current === openKey) return;
    lastOpen.current = openKey;
    if ((param ?? "") !== openKey) nav.filter({ [EDIT_PARAM]: openKey || null });
  }, [openKey, param, nav]);
  // AN ADDRESS NAMING PARTS OPENS THEM, once it is known whether this reader
  // may write the charter at all; for one who may not, it is dropped, and
  // the page is simply the page. Read when the address changes, never when
  // the open parts do, so a part just closed is not opened again by the
  // address it is about to be taken out of.
  useEffect(() => {
    const named = partsIn(param);
    if (param === null || viewer.loading) return;
    if (!mayEdit) {
      nav.filter({ [EDIT_PARAM]: null });
      return;
    }
    named.forEach((part, i) => {
      if (!stateRef.current.sessions[part]) open(part, "address", i === 0);
    });
    if (editValue(named) !== param) {
      // A VALUE NAMING NOTHING THIS PAGE EDITS (an old builder address, a
      // typo) is replaced with what is actually open.
      nav.filter({ [EDIT_PARAM]: editValue(named) || null });
    }
    // `open` and `nav` are stable, and the open parts are read through the
    // ref on purpose: see above.
  }, [param, mayEdit, viewer.loading, open, nav]);

  const editing = useMemo<Editing>(
    () => ({
      org,
      state,
      mayEdit,
      access,
      health,
      open,
      change,
      pending,
      save,
      cancel,
      keepMine,
      takeTheirs,
      editorRef,
      triggerRef,
    }),
    [
      org,
      state,
      mayEdit,
      access,
      health,
      open,
      change,
      pending,
      save,
      cancel,
      keepMine,
      takeTheirs,
      editorRef,
      triggerRef,
    ],
  );

  const dialog = (
    <ConfirmModal
      open={asking !== null}
      onClose={() => {
        const was = asking;
        setAsking(null);
        if (was?.kind === "cancel") focusAfterRender(was.part, "field");
      }}
      onConfirm={() => {
        const was = asking;
        setAsking(null);
        if (was?.kind === "cancel") {
          close(was.part);
        } else if (was?.kind === "leave") {
          for (const part of dirty) dispatch({ type: "close", part });
          was.leave();
        }
      }}
      title={question.leaving ? "Leave without saving?" : "Discard your changes?"}
      message={
        question.leaving
          ? `Your changes to ${listOf(question.parts.map(partName))} will be lost.`
          : question.unwritten
            ? `What you wrote for ${partName(question.parts[0] ?? "mission")} will be discarded.`
            : question.parts[0] === "policies"
              ? "The standing policies go back to how they were saved."
              : `${capitalized(partName(question.parts[0] ?? "mission"))} goes back to how it was saved.`
      }
      confirmLabel={question.leaving ? "Discard and leave" : "Discard changes"}
      cancelLabel="Keep editing"
      destructive
    />
  );
  return { editing, dialog };
}

/** The element a focus request names, once it is drawn. */
function focusTarget(
  target: FocusTarget,
  editor: HTMLElement | undefined,
  trigger: HTMLElement | undefined,
): HTMLElement | null {
  if (target === "return") return trigger ?? null;
  if (!editor) return null;
  const field = editor.querySelector<HTMLElement>("textarea, input");
  if (target === "invalid") {
    return editor.querySelector<HTMLElement>('[aria-invalid="true"]') ?? field;
  }
  return field;
}

function capitalized(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/** "the mission", "the mission and the vision", "the mission, the vision and the standing policies". */
function listOf(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}
