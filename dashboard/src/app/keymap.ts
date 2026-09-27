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
import { WORKSPACES } from "./nav.ts";

/** Where a row is live. See the file's doc. */
export type KeyScope =
  "frame" | "list" | "object" | "peek" | "canvas" | "builder" | "compose" | "search" | "layer";

/** The scopes in the order the legend draws them, with what each heading says. */
export const KEY_SCOPES: readonly { scope: KeyScope; title: string }[] = [
  { scope: "frame", title: "Anywhere" },
  { scope: "list", title: "In a list" },
  { scope: "peek", title: "With a peek open" },
  { scope: "object", title: "On a page with tabs" },
  { scope: "canvas", title: "On a chart" },
  { scope: "builder", title: "In the org builder" },
  { scope: "compose", title: "Writing a reply" },
  { scope: "search", title: "In search" },
  { scope: "layer", title: "In a dialog, a menu or the navigation drawer" },
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
    does: "Search this screen — or everything, where it has no search box",
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
  })),
  {
    id: "drawer",
    scope: "frame",
    presses: [{ key: "\\", mod: true }],
    does: "Open the navigation, on a window too narrow to show it",
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
    does: "Next row — or, on a task opened from a list, the next task",
    by: "dashboard",
  },
  {
    id: "list.previous",
    scope: "list",
    presses: [{ key: "k" }],
    does: "Previous row — or, on a task opened from a list, the previous task",
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
    // reader who has focused one and wants out means the rail.
    id: "peek.close",
    scope: "peek",
    presses: [{ key: "escape" }],
    does: "Close the peek",
    by: "dashboard",
    whileTyping: true,
  },
  { id: "tab", scope: "object", presses: DIGITS, does: "Go to that tab", by: "dashboard" },
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
    // The builder's undo is its own listener rather than [useKeymap]'s — it
    // is scoped to presses from inside the builder, which a window chord
    // cannot see — so it reads its presses from here through [matchesRow].
    // A field keeps the key: the same press undoes typing there.
    id: "builder.undo",
    scope: "builder",
    // `shift: false` rather than absent: Mod+Shift+Z is redo, so undo has
    // to refuse Shift rather than ignore it.
    presses: [{ key: "z", mod: true, shift: false }],
    does: "Undo the last change to the draft",
    by: "dashboard",
  },
  {
    id: "builder.redo",
    scope: "builder",
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
 * THE PALETTE'S OWN KEYS, read by it from its field through [matchesRow]. They
 * are live while it is the top layer and never beside a page key, so they
 * share a namespace with the layer's Escape and with nothing else.
 */
const SEARCH: KeyRow[] = [
  {
    id: "search.scope",
    scope: "search",
    presses: [{ key: "tab" }],
    does: "Next scope — All, Tasks, Pages, Agents, Actions; Shift+Tab goes back",
    by: "kit",
    whileTyping: true,
  },
  {
    id: "search.ask",
    scope: "search",
    presses: [{ key: "enter", mod: true }],
    does: "Ask an agent about what you typed — the answer lands in your inbox",
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
    does: "Close whatever is on top — a dialog, a menu, search or the drawer",
    by: "kit",
    whileTyping: true,
  },
];

/** The table. */
export const KEYMAP: readonly KeyRow[] = [...FRAME, ...PAGE, ...COMPOSE, ...SEARCH, ...LAYER];

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
  if (!row) throw new Error(`keymap: no row "${id}" — declare it in app/keymap.ts`);
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
