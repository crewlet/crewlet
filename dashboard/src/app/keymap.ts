/**
 * Every key this dashboard answers, in ONE table.
 *
 * # Why a table
 *
 * The keys used to be spelled where they were bound: the frame's own four in
 * `Shell.tsx`, the peek's three in `DetailRail.tsx`, the grid's three in
 * `DataGrid.tsx`, the digits in `tabs.ts`, the drawer's in the sidebar and the
 * builder's undo in a listener of its own. Nothing could list them, so nothing
 * did — there was no legend — and nothing could compare them, so two of them
 * already fought: a bare `/` opened the command palette from every screen,
 * including the eight that draw a search box of their own, so the key a reader
 * pressed to search what they were LOOKING AT took them away from it.
 *
 * Now a binding names a ROW here and never a key. [useKeymap] turns rows into
 * listeners, the legend (`?`, `KeyLegend.tsx`) renders the same rows, and
 * `keymap.test.ts` refuses two rows that answer one press in scopes that can be
 * live at once — so a key added anywhere is a key the legend shows and the
 * collision check reads, with no second edit to forget.
 *
 * # Scopes
 *
 * A row's scope is WHERE IT IS LIVE, which is what the legend groups by and
 * what the collision check reads. The page's scopes — the frame, a list, an
 * object, a peek, a chart, the builder — can all be live together (a list
 * with a peek open on an object with tabs), so a press is unique across all of
 * them. The LAYER scope is the one that is never live beside them: while a
 * dialog, a menu or the drawer holds the keyboard, every page key stands aside
 * (`lib/keys.ts`), so its Escape shares nothing with the peek's.
 *
 * # Rows the kit binds
 *
 * `by: "kit"` rows are bound by `@crewlethq/ui` itself — the canvas's zoom
 * keys, the layer stack's Escape. They are here so the legend is the whole
 * answer to "what can I press" and so the collision check sees them: a page
 * binding `0` would fight the chart's fit exactly as surely as one of ours.
 * [useKeymap] refuses to bind one, because a second binding of a key the kit
 * already answers is two handlers for one press.
 */

import { useKeyChords, type Chord, type Sequence } from "~/lib/keys.ts";
import {
  WORKSPACES,
  serviceShown,
  workspaceRow,
  workspaceShown,
  type ServicesRun,
  type Service,
  type Workspace,
} from "./nav.ts";

/** Where a row is live. See the file's doc. */
export type KeyScope =
  | "frame"
  | "list"
  | "object"
  | "peek"
  | "artifact"
  | "gallery"
  | "canvas"
  | "draft"
  | "compose"
  | "create"
  | "charter"
  | "search"
  | "layer";

/** The scopes in the order the legend draws them, with what each heading says. */
export const KEY_SCOPES: readonly { scope: KeyScope; title: string }[] = [
  { scope: "frame", title: "Anywhere" },
  { scope: "list", title: "In a list" },
  { scope: "peek", title: "With a peek open" },
  { scope: "object", title: "On a page with tabs" },
  { scope: "artifact", title: "On an open artifact" },
  { scope: "gallery", title: "In the artifacts gallery and its folders" },
  { scope: "canvas", title: "On a chart" },
  { scope: "draft", title: "Editing a draft: the org builder, or Home while it is arranged" },
  { scope: "compose", title: "Writing a reply" },
  { scope: "create", title: "Making a new artifact" },
  { scope: "charter", title: "Writing the company's principles" },
  { scope: "search", title: "In search" },
  {
    scope: "layer",
    title: "In a dialog, a menu, the navigation drawer or an artifact's panel",
  },
];

/** One press: a key, its modifiers, and the prefix it follows if it is a sequence. */
export interface Press {
  /** `KeyboardEvent.key`, lowercased: `"k"`, `"escape"`, `"["`, `"?"`. */
  key: string;
  /** The platform's command key: Command on a Mac, Control elsewhere. */
  mod?: boolean;
  /**
   * Shift, for a LETTER only. A symbol that needs Shift to type (`?`, `+`)
   * is written as the symbol, because that is what `KeyboardEvent.key`
   * reports and what a reader's keycap says.
   */
  shift?: boolean;
  /**
   * Option on a Mac, Alt elsewhere. ONLY for a row a surface reads through
   * [matchesRow] from inside itself — the palette's accelerators, pressed in
   * its field, where a bare letter is a letter of the query and Alt is what
   * turns it into a key. Matched by the physical key, because Option+A on a
   * Mac types `å`. [useKeymap] refuses one, like Shift.
   */
  alt?: boolean;
  /** The key pressed first, for a two-key sequence: `g` then `h`. */
  after?: string;
}

export interface KeyRow {
  id: string;
  scope: KeyScope;
  /** Every press that does it. More than one is ONE action with two keys. */
  presses: readonly Press[];
  /** What it does, as the legend says it. */
  does: string;
  /** Who binds it. See the file's doc. */
  by: "dashboard" | "kit";
  /**
   * Whether it fires from inside a field. Only a key a reader in a field
   * could mean for the page: Escape out of a peek's field, the command key
   * with K. A bare letter never does — it is a letter in the field.
   */
  whileTyping?: boolean;
  /**
   * The service built into Crewlet the row's place reads (`nav.ts`): a jump
   * to Work is no key at all where the company runs no Crewlet Works, so the
   * legend leaves it out and the frame binds nothing to it.
   */
  service?: Service;
  /**
   * The workspace a `g` jump goes to. Such a row is a key exactly where that
   * workspace is a row (`nav.ts`'s `workspaceShown`), which no single service
   * can say: My work is offered where the tracker OR the code host runs, so
   * its jump names no service and would otherwise be listed everywhere.
   */
  workspace?: Workspace;
}

/**
 * Whether a key is one for a company running `runs`: a `g` jump where its
 * workspace is a row, as the frame binds it (`Shell.tsx`), and any other row
 * where the service it reads runs. What the legend lists.
 */
export function keyShown(row: KeyRow, runs: ServicesRun): boolean {
  const ws = row.workspace ? workspaceRow(row.workspace) : undefined;
  return ws ? workspaceShown(ws, runs) : serviceShown(row, runs);
}

/**
 * The digits an object's tabs take, one row with nine presses: the legend
 * says "1–9" once, and [useKeymap] hands the handler which one was pressed.
 */
const DIGITS: Press[] = Array.from({ length: 9 }, (_, i) => ({ key: String(i + 1) }));

const FRAME: KeyRow[] = [
  {
    id: "palette",
    scope: "frame",
    presses: [{ key: "k", mod: true }],
    does: "Open search, or close it",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "search",
    scope: "frame",
    presses: [{ key: "/" }],
    does: "Search this screen, or everything",
    by: "dashboard",
  },
  { id: "keys", scope: "frame", presses: [{ key: "?" }], does: "Show these keys", by: "dashboard" },
  // `g` then a letter jumps to a workspace, DERIVED from `nav.ts` so a
  // workspace added there is a row here with no second edit. A sequence
  // rather than a modifier, because every single-modifier combination worth
  // having is already the browser's.
  ...WORKSPACES.map((ws): KeyRow => ({
    id: `go.${ws.key}`,
    scope: "frame",
    presses: [{ after: "g", key: ws.chord }],
    does: `Go to ${ws.label}`,
    by: "dashboard",
    workspace: ws.key,
  })),
  // TWO `g` SEQUENCES ARE NOT WORKSPACES, so neither is a `go.` row, the
  // family derived from `nav.ts`. The first is a running turn: an object, and
  // reaching it was a workspace, a card, a profile and a tab away.
  {
    id: "running",
    scope: "frame",
    presses: [{ after: "g", key: "r" }],
    does: "Watch running turns",
    by: "dashboard",
  },
  // THE SECOND IS THE SCOPE OF WORK: the switcher under Work's trail, or
  // centred over any other page, where nothing moves until a project is chosen.
  {
    id: "project",
    scope: "frame",
    presses: [{ after: "g", key: "p" }],
    does: "Switch project",
    by: "dashboard",
    // A PROJECT IS CREWLET WORKS', so where the company runs none there is
    // nothing to switch between.
    service: "tracker",
  },
  {
    id: "drawer",
    scope: "frame",
    presses: [{ key: "\\", mod: true }],
    does: "Open the navigation",
    by: "dashboard",
    whileTyping: true,
  },
];

const PAGE: KeyRow[] = [
  // ON A TASK OPENED FROM A LIST the task is that list's row, so the same two
  // keys step to the task before and after it (`routes/work/ListPosition.tsx`).
  {
    id: "list.next",
    scope: "list",
    presses: [{ key: "j" }],
    does: "Next row or task",
    by: "dashboard",
  },
  {
    id: "list.previous",
    scope: "list",
    presses: [{ key: "k" }],
    does: "Previous row or task",
    by: "dashboard",
  },
  {
    id: "list.open",
    scope: "list",
    presses: [{ key: "enter" }],
    does: "Open the row",
    by: "dashboard",
  },
  {
    id: "peek.previous",
    scope: "peek",
    presses: [{ key: "[" }],
    does: "Previous object in the list",
    by: "dashboard",
  },
  {
    id: "peek.next",
    scope: "peek",
    presses: [{ key: "]" }],
    does: "Next object in the list",
    by: "dashboard",
  },
  {
    // ESCAPE CLOSES FROM INSIDE A FIELD TOO: the peek holds inputs, and a
    // reader who has focused one and wants out means the peek.
    id: "peek.close",
    scope: "peek",
    presses: [{ key: "escape" }],
    does: "Close the peek",
    by: "dashboard",
    whileTyping: true,
  },
  { id: "tab", scope: "object", presses: DIGITS, does: "Go to that tab", by: "dashboard" },
  // AN OPEN ARTIFACT'S PANELS, each a letter that opens it and closes it
  // again (`routes/artifacts/ArtifactPage.tsx`). Its first letter wherever it
  // is free; Escape from inside a panel closes it too, the panel's own key
  // (the kit's `ArtifactPanel`), listed under the layer's Escape because it
  // is the same gesture.
  {
    id: "artifact.details",
    scope: "artifact",
    presses: [{ key: "i" }],
    does: "Show or hide its details",
    by: "dashboard",
  },
  {
    id: "artifact.history",
    scope: "artifact",
    presses: [{ key: "h" }],
    does: "Show or hide its versions and history",
    by: "dashboard",
  },
  {
    id: "artifact.data",
    scope: "artifact",
    presses: [{ key: "d" }],
    does: "Show or hide its data, where it keeps any",
    by: "dashboard",
  },
  {
    id: "artifact.people",
    scope: "artifact",
    presses: [{ key: "p" }],
    does: "Show or hide who is here",
    by: "dashboard",
  },
  {
    id: "artifact.share",
    scope: "artifact",
    presses: [{ key: "s" }],
    does: "Share it",
    by: "dashboard",
  },
  // THE GALLERY'S GESTURES on its listings and its folders
  // (`routes/artifacts/listing.tsx`, `Folders.tsx`, `FolderPage.tsx`). Bare
  // letters, because a window chord compares no Shift and no Alt: the
  // shifted and Alt keys a desktop file manager uses would fire for the
  // plain letter here.
  {
    id: "gallery.select",
    scope: "gallery",
    presses: [{ key: "x" }],
    does: "Start picking artifacts, or stop and clear the picks",
    by: "dashboard",
  },
  {
    id: "gallery.move",
    scope: "gallery",
    presses: [{ key: "m" }],
    does: "Move what is picked to a folder",
    by: "dashboard",
  },
  {
    id: "gallery.folder",
    scope: "gallery",
    presses: [{ key: "n" }],
    does: "Make a new folder here",
    by: "dashboard",
  },
  {
    id: "gallery.up",
    scope: "gallery",
    presses: [{ key: "u" }],
    does: "Go up to the folder above",
    by: "dashboard",
  },
  {
    id: "canvas.in",
    scope: "canvas",
    presses: [{ key: "+" }],
    does: "Zoom in, with focus in the chart",
    by: "kit",
  },
  {
    id: "canvas.out",
    scope: "canvas",
    presses: [{ key: "-" }],
    does: "Zoom out, with focus in the chart",
    by: "kit",
  },
  {
    id: "canvas.fit",
    scope: "canvas",
    presses: [{ key: "0" }],
    does: "Fit the whole chart in view",
    by: "kit",
  },
  {
    // A draft's undo is its own listener rather than [useKeymap]'s (see
    // `lib/useUndoKeys.ts`): it is scoped to presses from inside the surface
    // keeping the draft, which a window chord cannot see, so it reads its
    // presses from here through [matchesRow]. A field keeps the key: the same
    // press undoes typing there. The org builder and Home's board are never
    // on screen together, so one pair of rows serves both.
    id: "draft.undo",
    scope: "draft",
    // `shift: false` rather than absent: Mod+Shift+Z is redo, so undo has
    // to refuse Shift rather than ignore it.
    presses: [{ key: "z", mod: true, shift: false }],
    does: "Undo the last change to the draft",
    by: "dashboard",
  },
  {
    id: "draft.redo",
    scope: "draft",
    presses: [{ key: "z", mod: true, shift: true }],
    does: "Redo it",
    by: "dashboard",
  },
];

/**
 * THE COMPOSER'S KEY, read by the Inbox's reply field from inside itself
 * through [matchesRow]: Enter alone is a new line in a reply, so sending takes
 * the command key, as it does in every chat surface a reader already uses.
 */
const COMPOSE: KeyRow[] = [
  {
    id: "compose.send",
    scope: "compose",
    presses: [{ key: "enter", mod: true }],
    does: "Send the reply",
    by: "dashboard",
    whileTyping: true,
  },
];

/**
 * A CREATE PAGE'S KEY, bound by the shared frame every kind's create page
 * stands in (`routes/artifacts/new/CreateShell.tsx`) to the page's one primary
 * action, so every kind starts with the same press and none binds its own. It
 * fires from inside a field, since a create page is mostly fields, and it
 * shares its press with the composer's Send, which no create page draws
 * ([SCOPES_APART]).
 */
const CREATE: KeyRow[] = [
  {
    id: "create.start",
    scope: "create",
    presses: [{ key: "enter", mod: true }],
    does: "Do the page's main action: create it, or ask the agent",
    by: "dashboard",
    whileTyping: true,
  },
];

/**
 * THE PRINCIPLES EDITOR'S KEYS, bound by the kit's `InlineEditor` that each
 * part of the charter is written in (`routes/settings/principles/edit/`), and
 * listed here so the legend and the collision check see them. Both fire from
 * inside a field, since the editor is one: Enter alone is a new line in a
 * statement and the next policy in the list, so saving takes the command key,
 * and Escape is the way out of the part without its changes.
 */
const CHARTER: KeyRow[] = [
  {
    id: "charter.save",
    scope: "charter",
    presses: [{ key: "enter", mod: true }],
    does: "Save the part you are editing",
    by: "kit",
    whileTyping: true,
  },
  {
    id: "charter.cancel",
    scope: "charter",
    presses: [{ key: "escape" }],
    does: "Stop editing it, asking first if it has changes",
    by: "kit",
    whileTyping: true,
  },
];

/**
 * Scopes never live on one screen, which may therefore share a press: a
 * reply's composer is the Inbox's and a task's, and a create page is
 * neither, so Send and a create page's main action are both the command key
 * with Enter. Company › Principles holds no composer, no create page and no
 * peek, so its editor's Save and Escape are free there.
 */
export const SCOPES_APART: readonly (readonly [KeyScope, KeyScope])[] = [
  ["compose", "create"],
  ["charter", "compose"],
  ["charter", "create"],
  ["charter", "peek"],
];

/** Whether two scopes can be live on one screen. */
export function liveTogether(a: KeyScope, b: KeyScope): boolean {
  return !SCOPES_APART.some(([x, y]) => (x === a && y === b) || (x === b && y === a));
}

/**
 * THE PALETTE'S OWN KEYS, read by it from its field through [matchesRow]. They
 * are live while it is the top layer and never beside a page key, so they
 * share a namespace with the layer's Escape and with nothing else.
 */
const SEARCH: KeyRow[] = [
  {
    id: "search.scope",
    scope: "search",
    presses: [{ key: "tab" }],
    does: "Next scope; Shift+Tab goes back",
    by: "kit",
    whileTyping: true,
  },
  {
    id: "search.ask",
    scope: "search",
    presses: [{ key: "enter", mod: true }],
    does: "Ask an agent about what you typed",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "search.assign",
    scope: "search",
    presses: [{ key: "a", alt: true }],
    does: "Assign the task at hand to an agent",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "search.create",
    scope: "search",
    presses: [{ key: "c", alt: true }],
    does: "Create a task titled what you typed",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "search.back",
    scope: "search",
    presses: [{ key: "backspace" }],
    does: "Back to the results, from an empty picker",
    by: "dashboard",
    whileTyping: true,
  },
];

const LAYER: KeyRow[] = [
  {
    id: "layer.close",
    scope: "layer",
    presses: [{ key: "escape" }],
    does: "Close what is on top",
    by: "kit",
    whileTyping: true,
  },
];

/** The table. */
export const KEYMAP: readonly KeyRow[] = [
  ...FRAME,
  ...PAGE,
  ...COMPOSE,
  ...CREATE,
  ...CHARTER,
  ...SEARCH,
  ...LAYER,
];

/**
 * Whether a row is live only while a layer holds the keyboard — its own
 * namespace, since every page key stands aside then (`lib/keys.ts`).
 */
export function layerScoped(scope: KeyScope): boolean {
  return scope === "layer" || scope === "search";
}

const BY_ID = new Map(KEYMAP.map((row) => [row.id, row]));

/** A row by its id, or a thrown error naming the id nobody declared. */
export function keyRow(id: string): KeyRow {
  const row = BY_ID.get(id);
  if (!row) throw new Error(`keymap: no row "${id}"; declare it in app/keymap.ts`);
  return row;
}

/**
 * Whether a press is one of this row's, for a listener that cannot be a
 * window chord (the builder's, which is scoped to its own subtree). Shift is
 * compared for a row that names it and ignored for one that does not, so
 * `+` matches however the reader's layout produces it.
 */
export function matchesRow(id: string, e: KeyboardEvent): boolean {
  const mod = e.metaKey || e.ctrlKey;
  return keyRow(id).presses.some(
    (p) =>
      !p.after &&
      (p.key === e.key.toLowerCase() || sameLetterKey(p.key, e.code)) &&
      Boolean(p.mod) === mod &&
      Boolean(p.alt) === e.altKey &&
      (p.shift === undefined ? true : p.shift === e.shiftKey),
  );
}

/**
 * Whether a physical key is this letter, for a layout whose letters are not
 * Latin: on a Russian layout the key a reader presses for Ctrl+Z reports
 * `я`, and the builder's undo answered it by its code before it read this
 * table, so it still does.
 */
function sameLetterKey(key: string, code: string): boolean {
  return /^[a-z]$/.test(key) && code === `Key${key.toUpperCase()}`;
}

/**
 * A row's presses as the kit's `useShortcut` spells them, for a surface on
 * the layer stack that answers a page key itself — the palette closes on its
 * own chord, which the page's binding cannot see while the palette is up.
 */
export function shortcutKeys(id: string): { key: string; mod?: boolean }[] {
  return keyRow(id).presses.map((p) => (p.mod ? { key: p.key, mod: true } : { key: p.key }));
}

/** One binding: what to do, and whether it is live now. */
export type KeyHandler =
  | ((e: KeyboardEvent, index: number) => void)
  | {
      /** `index` is which of the row's presses it was — the digit's position for `tab`. */
      run: (e: KeyboardEvent, index: number) => void;
      /** Off without changing the shape of the bindings; per press for a row with several. */
      when?: boolean | ((index: number) => boolean);
    };

/**
 * Bind rows of the table for as long as the component is mounted.
 *
 * Keyed by row id, so the caller says WHAT it answers and the table says with
 * which key. A kit row is refused (see the file's doc), and so is an id the
 * table does not declare — a typo would otherwise be a key that silently
 * never fires.
 */
export function useKeymap(handlers: Record<string, KeyHandler>): void {
  const chords: (Chord | Sequence)[] = [];
  for (const [id, handler] of Object.entries(handlers)) {
    const row = keyRow(id);
    if (row.by === "kit") {
      throw new Error(`keymap: "${id}" is bound by the design system, not by the dashboard`);
    }
    const run = typeof handler === "function" ? handler : handler.run;
    const when = typeof handler === "function" ? undefined : handler.when;
    row.presses.forEach((press, index) => {
      if (press.alt) {
        // Alt is read from inside a surface by `matchesRow`: a window chord
        // compares no Alt and would fire for the plain letter.
        throw new Error(`keymap: "${id}" names Alt and cannot be a window chord`);
      }
      if (press.shift) {
        // A shifted letter is matched by `matchesRow`, never by a window
        // chord, which compares no Shift and would fire for the plain letter.
        throw new Error(`keymap: "${id}" names Shift and cannot be a window chord`);
      }
      const chord: Chord = {
        key: press.key,
        run: (e) => run(e, index),
        meta: press.mod,
        whileTyping: row.whileTyping,
        when: typeof when === "function" ? when(index) : when,
      };
      chords.push(press.after ? { ...chord, after: press.after } : chord);
    });
  }
  useKeyChords(chords);
}

/** Names `@crewlethq/ui`'s `Kbd` knows, for the keys this table lowercases. */
const CAP_NAMES: Record<string, string> = {
  escape: "Escape",
  enter: "Enter",
  tab: "Tab",
  backspace: "Backspace",
};

/**
 * A two-key sequence in words, as a tooltip or a hint says it: `g` then `p`
 * is "G then P". Read off the table, so the words cannot drift from the key.
 */
export function sequenceWords(id: string): string {
  const press = keyRow(id).presses[0]!;
  return [press.after, press.key]
    .filter((k): k is string => !!k)
    .map((k) => k.toUpperCase())
    .join(" then ");
}

/**
 * One press as the keys of a kit `Kbd` — `["Mod", "k"]` — so a hint drawn
 * anywhere (the legend, the sidebar's search trigger) is the table's own
 * press in the platform's own notation, never a hand-written "⌘K". A
 * sequence's prefix is not part of it: `g` then `h` is two caps.
 */
export function capsOf(press: Press): string[] {
  const caps: string[] = [];
  if (press.mod) caps.push("Mod");
  if (press.alt) caps.push("Alt");
  if (press.shift) caps.push("Shift");
  caps.push(CAP_NAMES[press.key] ?? press.key);
  return caps;
}
