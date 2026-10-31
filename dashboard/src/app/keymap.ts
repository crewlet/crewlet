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

import { isApplePlatform, keyGlyph } from "@crewlethq/ui";
import { useKeyChords, type Chord, type Sequence } from "~/lib/keys.ts";
import { WORKSPACES, type Service } from "./nav.ts";

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
  | "chat"
  | "create"
  | "charter"
  | "search"
  | "assistant"
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
  { scope: "compose", title: "Writing a reply or a message" },
  { scope: "chat", title: "In Chatlet" },
  { scope: "create", title: "Making a new artifact" },
  { scope: "charter", title: "Writing the company's principles" },
  { scope: "search", title: "In search" },
  { scope: "assistant", title: "In Ask Crewlet" },
  {
    scope: "layer",
    title: "In a dialog, a menu, the navigation drawer, an artifact's panel or Ask Crewlet",
  },
];

/** One press: a key, its modifiers, and the prefix it follows if it is a sequence. */
export interface Press {
  /** `KeyboardEvent.key`, lowercased: `"k"`, `"escape"`, `"["`, `"?"`. */
  key: string;
  /** The platform's command key: Command on a Mac, Control elsewhere. */
  mod?: boolean;
  /**
   * Shift, for a LETTER or a key such as an arrow whose shifted press is
   * another row's. A symbol that needs Shift to type (`?`, `+`) is written as
   * the symbol, because that is what `KeyboardEvent.key` reports and what a
   * reader's keycap says. `false` says the row fires only WITHOUT it.
   */
  shift?: boolean;
  /**
   * Option on a Mac, Alt elsewhere. On a LETTER, only for a row a surface
   * reads through [matchesRow] from inside itself (the palette's
   * accelerators, pressed in its field, where a bare letter is a letter of
   * the query and Alt is what turns it into a key), matched by the physical
   * key because Option+A on a Mac types `å`; [useKeymap] refuses one. On any
   * other key, such as an arrow, the key is reported as itself and a window
   * chord compares the Alt.
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
   * to Worklet is no key at all where the company runs no Worklet, so the
   * legend leaves it out and the frame binds nothing to it.
   */
  service?: Service;
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
    ...(ws.service ? { service: ws.service } : {}),
  })),
  // THE ONE `g` SEQUENCE THAT IS NOT A WORKSPACE: a running turn is an object,
  // and reaching it was a workspace, a card, a profile and a tab away. Not a
  // `go.` row, which is the family derived from `nav.ts`.
  {
    id: "running",
    scope: "frame",
    presses: [{ after: "g", key: "r" }],
    does: "Watch running turns",
    by: "dashboard",
  },
  // AND THE SCOPE OF WORKLET: the switcher under Worklet's trail, or centred over
  // any other page, where nothing moves until a project is chosen.
  {
    id: "project",
    scope: "frame",
    presses: [{ after: "g", key: "p" }],
    does: "Switch project",
    by: "dashboard",
    // A PROJECT IS WORKLET'S, so where the company runs none there is
    // nothing to switch between.
    service: "tracker",
  },
  // THE ASSISTANT, from anywhere and from inside a field: the command key
  // with a full stop, which neither this table nor a browser's common
  // shortcuts take (the command key with J is a browser's Downloads). Pressed
  // outside the panel it opens it, or puts focus in its composer; pressed
  // inside, it closes it and hands focus back.
  {
    id: "assistant.open",
    scope: "frame",
    presses: [{ key: ".", mod: true }],
    does: "Open Ask Crewlet, or close it",
    by: "dashboard",
    whileTyping: true,
  },
  // ONE PRESS FOR THE SIDEBAR IN EITHER SHAPE OF THE FRAME: it folds the
  // sidebar to its compact rail and back where the sidebar stands beside the
  // page, and below the shell breakpoint, where the sidebar is a drawer with
  // nothing to fold, it opens the drawer (`sidebar/Sidebar.tsx`).
  {
    id: "sidebar",
    scope: "frame",
    presses: [{ key: "\\", mod: true }],
    does: "Fold or unfold the sidebar, or open it on a narrow window",
    by: "dashboard",
    whileTyping: true,
  },
];

const PAGE: KeyRow[] = [
  // ON A TASK OPENED FROM A LIST the task is that list's row, so the same two
  // keys step to the task before and after it (`routes/worklet/ListPosition.tsx`).
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
 * THE SEND KEY FOR WHAT A PERSON SAYS TO SOMEBODY, read from inside the field
 * through [matchesRow] by every box that says it: a reply in the Inbox and a
 * comment on a task (`components/Composer.tsx`), and a message in a chat room
 * or a thread (`routes/chatlet/Composer.tsx`). Enter alone is a new line there,
 * because what is said to somebody is often several lines: a list, a pasted
 * stack trace.
 *
 * THE ASSISTANT'S QUESTION BOX IS THE ONE EXCEPTION, and on purpose: it sends
 * on Enter ([ASSISTANT]'s `assistant.send`), the design system's own rule for
 * a question, which is one line far more often than not. Two kinds of box,
 * two keys, each listed in the legend under its own heading.
 */
const COMPOSE: KeyRow[] = [
  {
    id: "compose.send",
    scope: "compose",
    presses: [{ key: "enter", mod: true }],
    does: "Send it",
    by: "dashboard",
    whileTyping: true,
  },
];

/**
 * CHATLET'S KEYS, bound by its screen (`routes/chatlet/shortcuts.ts`) for as
 * long as it is open. Moving between rooms fires from inside the message box
 * too, since a reader answering in one room is who moves to the next; marking
 * a room read does not, because Escape in a box is the box's own.
 */
const CHAT: KeyRow[] = [
  {
    id: "chat.room",
    scope: "chat",
    presses: [
      { key: "arrowup", alt: true, shift: false },
      { key: "arrowdown", alt: true, shift: false },
    ],
    does: "Previous or next room",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "chat.unread",
    scope: "chat",
    presses: [
      { key: "arrowup", alt: true, shift: true },
      { key: "arrowdown", alt: true, shift: true },
    ],
    does: "Previous or next room with unread messages",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "chat.search",
    scope: "chat",
    presses: [{ key: "f", mod: true, shift: false }],
    does: "Search the open room, or all of Chatlet",
    by: "dashboard",
    whileTyping: true,
  },
  {
    id: "chat.read",
    scope: "chat",
    presses: [{ key: "escape" }],
    does: "Mark the open room as read",
    by: "dashboard",
  },
  {
    id: "chat.keys",
    scope: "chat",
    presses: [{ key: "/", mod: true }],
    does: "Show Chatlet's keys",
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
 * Scopes whose rows never answer one press together, which may therefore
 * share it: two never live on one screen, or one whose row stands aside
 * while the other is live (Chatlet's Escape while a peek is open). A
 * reply's composer is the Inbox's and a task's, and a create page is
 * neither, so Send and a create page's main action are both the command key
 * with Enter. Company › Principles holds no composer, no create page and no
 * peek, so its editor's Save and Escape are free there.
 */
export const SCOPES_APART: readonly (readonly [KeyScope, KeyScope])[] = [
  ["compose", "create"],
  // CHATLET DRAWS NO CREATE PAGE AND NO CHARTER. A PEEK can be open over it
  // (any address may carry one), and Chatlet's Escape is bound only while
  // none is (`routes/chatlet/shortcuts.ts`), so the two never answer one
  // press: Escape with a peek open closes the peek and marks nothing read.
  ["chat", "peek"],
  ["chat", "create"],
  ["chat", "charter"],
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

/**
 * THE ASSISTANT'S OWN KEY, answered by the kit's `Composer` from inside its
 * field and claimed there, so it shares the layer's namespace rather than the
 * page's: Enter in the composer is never a list's Enter. Escape inside the
 * panel is the layer's row below, which the composer answers first while an
 * answer is running (it stops it) and the panel answers otherwise (it
 * closes), so one press never closes a peek under it.
 */
const ASSISTANT: KeyRow[] = [
  {
    id: "assistant.send",
    scope: "assistant",
    presses: [{ key: "enter" }],
    does: "Send the question; with Shift, a new line",
    by: "kit",
    whileTyping: true,
  },
];

const LAYER: KeyRow[] = [
  {
    id: "layer.close",
    scope: "layer",
    presses: [{ key: "escape" }],
    does: "Close what is on top; in Ask Crewlet, stop a running answer first",
    by: "kit",
    whileTyping: true,
  },
];

/** The table. */
export const KEYMAP: readonly KeyRow[] = [
  ...FRAME,
  ...PAGE,
  ...COMPOSE,
  ...CHAT,
  ...CREATE,
  ...CHARTER,
  ...SEARCH,
  ...ASSISTANT,
  ...LAYER,
];

/**
 * Whether a row is live only while a layer holds the keyboard — its own
 * namespace, since every page key stands aside then (`lib/keys.ts`). The
 * assistant's keys are in it too: they are answered inside its panel and
 * claimed there, so no page key ever hears them.
 */
export function layerScoped(scope: KeyScope): boolean {
  return scope === "layer" || scope === "search" || scope === "assistant";
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
      if (press.alt && /^[a-z]$/.test(press.key)) {
        // ALT ON A LETTER is read from inside a surface by `matchesRow`, by
        // the physical key: Option composes the letter into another
        // character on a Mac (`å` for A), so a window chord matched by `key`
        // would never fire there. Alt on any other key (an arrow) reports
        // the key itself and binds like any press.
        throw new Error(`keymap: "${id}" names Alt on a letter and cannot be a window chord`);
      }
      const chord: Chord = {
        key: press.key,
        run: (e) => run(e, index),
        meta: press.mod,
        // EXACT WHERE THE ROW SAYS: a row naming Alt or Shift fires only with
        // it held, and one naming `shift: false` only without, so Alt with an
        // arrow and Alt and Shift with the same arrow are two rows.
        ...(press.alt !== undefined ? { alt: press.alt } : {}),
        ...(press.shift !== undefined ? { shift: press.shift } : {}),
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

/**
 * One press as `aria-keyshortcuts` names it: a command-key press is both
 * Meta and Control, because the attribute is read on every platform and the
 * table's `mod` is Command on a Mac and Control elsewhere.
 */
export function ariaKeysOf(press: Press): string {
  const rest = [
    press.alt ? "Alt" : null,
    press.shift ? "Shift" : null,
    press.key.length === 1 ? press.key.toUpperCase() : (CAP_NAMES[press.key] ?? press.key),
  ]
    .filter((k): k is string => k !== null)
    .join("+");
  return press.mod ? `Meta+${rest} Control+${rest}` : rest;
}

/**
 * One press as a tooltip prints it, in the platform's notation, from the
 * kit's own key names: "⌘." on a Mac, "Ctrl+." elsewhere.
 */
export function pressText(press: Press, apple: boolean = isApplePlatform()): string {
  return capsOf(press)
    .map((key) => keyGlyph(key, apple).glyph)
    .join(apple ? "" : "+");
}
