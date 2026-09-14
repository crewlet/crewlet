/**
 * The org builder's context: what every view and dialog of the Builder lens
 * reads, and the only way they change anything.
 *
 * ONE DOOR. The canvas, the outline, the node editor and the dialogs never
 * call the transport or storage. They read the draft and the engine's last
 * answer from here, record operations with `dispatch`, and open each other
 * through the `open*` functions. Everything with a lifetime (the dry-run
 * check, the save, the kept draft, the live region) belongs to `Builder.tsx`,
 * which provides this value, so a view can be rendered in a test against a
 * plain object and cannot start a request of its own.
 *
 * WHAT THE ENGINE SAID IS PER NODE. `problemsFor` and `warningsFor` answer
 * from the last check of the CURRENT draft, placed through the path index of
 * the document that check sent (see `model/problems.ts`): a check that is
 * still out, or answered for an older draft, contributes nothing, so a badge
 * never names a problem the draft no longer has.
 *
 * A VIEW REGISTERS ITSELF. Focus, reveal and expanding a whole tree are the
 * mounted view's to perform (the canvas pans, the outline scrolls), while the
 * decision of WHICH node to focus after an operation, an undo or a redo is
 * the Builder's. So the view that is on screen hands the Builder a
 * [BuilderViewHandle] through [useBuilderView], and `focusNode` and the
 * toolbar's Expand all and Collapse all go through whichever handle is
 * registered.
 */

import { createContext, useContext, useEffect } from "react";
import type {
  AgentRow,
  ConfigProblem,
  ConfigWarning,
  DerivedSeat,
  DerivedUnit,
  SandboxEntry,
} from "~/protocol/index.ts";
import type { KeySource, NodeKey } from "./model/keys.ts";
import type { BuilderAction, BuilderState } from "./model/reducer.ts";

/** What the Add dialog is asked to add. */
export type AddKind = "unit" | "agent" | "human";

/**
 * Which chart the canvas draws. The Builder owns the `chart` section param
 * (its toolbar is where it is chosen), so the canvas is HANDED the answer
 * rather than reading the URL a second time, where the two could disagree.
 */
export type ChartKind = "structure" | "reporting";

/** The engine's derivation of the current draft, with its lists always present. */
export interface BuilderDerived {
  readonly seats: DerivedSeat[];
  readonly units: DerivedUnit[];
}

/**
 * A part of the node editor it can be opened at: a seat's Reports (whom it
 * manages) or a unit's Leadership (its lead). The editor then starts on that
 * part's first field rather than on the name at the top of a long form, so an
 * action about one field ("Edit reports", a lead chosen outside the unit)
 * lands on it. A part the node does not have opens the editor as usual.
 */
export type EditorSectionName = "reports" | "leadership";

/** What the mounted view does for the Builder. */
export interface BuilderViewHandle {
  /** Moves focus to the node without scrolling, then reveals it. */
  focusNode(key: NodeKey): void;
  expandAll(): void;
  collapseAll(): void;
}

export interface BuilderApi {
  /** The model's state: the base, the draft, the log and the last check. */
  state: BuilderState;
  /** Records an operation, undoes, redoes. Never a transport call. */
  dispatch(action: BuilderAction): void;
  /** From the last check of the current draft; `null` until one answers with a derivation. */
  derived: BuilderDerived | null;
  /** The problems the last check of the current draft placed on this node. */
  problemsFor(key: NodeKey): ConfigProblem[];
  /** The warnings the last check of the current draft placed on this node. */
  warningsFor(key: NodeKey): ConfigWarning[];
  /** The problems that name no node: a document-level refusal, an integration block. */
  documentProblems: ConfigProblem[];
  /** The selected node, mirrored in the URL's `unit=` and `seat=` filters. */
  selection: { key: NodeKey | null; select(key: NodeKey | null): void };
  /** Opens the node's editor, at `section` when one is named. */
  openEditor(key: NodeKey, section?: EditorSectionName): void;
  /** `parent` is a unit's key, or `null` for the company root. */
  openAdd(parent: NodeKey | null, kind?: AddKind): void;
  openMove(key: NodeKey): void;
  openDelete(key: NodeKey): void;
  openChangeKind(key: NodeKey): void;
  /** Says a sentence through the Builder's one polite live region. */
  announce(message: string): void;
  /** Focuses and reveals a node in whichever view is mounted. */
  focusNode(key: NodeKey): void;
  /**
   * True in the guarded and read-only postures, in a conflict, and while a
   * kept draft waits for the operator's decision: views draw the draft and
   * record nothing.
   */
  readOnly: boolean;
  /** Live state, for the StateBadge on saved agent seats. */
  agents: AgentRow[];
  sandboxes: SandboxEntry[];
  /**
   * Where a view or dialog that creates a node mints its key, in its own event
   * handler (see `model/keys.ts`). The Builder's own source, which is the
   * browser's random one (`runtime.randomKeys`) unless a suite injects
   * another, so every key and write id the lens makes comes from one place.
   */
  keys: KeySource;
  /**
   * Registers the mounted view's handle; the result unregisters it. Views use
   * [useBuilderView] rather than calling this directly. Its identity is stable
   * for the Builder's lifetime, so a view registers once per mount.
   */
  registerView(handle: BuilderViewHandle): () => void;
}

export const BuilderContext = createContext<BuilderApi | null>(null);

/** The Builder's context. Throws outside the Builder, which is a wiring defect, not a state. */
export function useBuilder(): BuilderApi {
  const api = useContext(BuilderContext);
  if (!api) throw new Error("useBuilder outside the org builder");
  return api;
}

/**
 * Registers a view's handle for as long as the view is mounted. Pass a
 * handle whose identity is stable (a `useMemo` over the view's own refs), or
 * the view is registered again on every render.
 */
export function useBuilderView(handle: BuilderViewHandle): void {
  const { registerView } = useBuilder();
  useEffect(() => registerView(handle), [registerView, handle]);
}
