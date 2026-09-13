/**
 * A keyboard shortcut, drawn the way the reader's own keyboard labels it.
 *
 * WHY IT EXISTS. A hint that says "Ctrl+Z" to somebody on a Mac is a hint for
 * a key they do not press, and one that says "⌘Z" is read aloud by a screen
 * reader as "place of interest sign Z". The builder shows shortcuts in its
 * toolbar and menus, so the mapping lives here once.
 *
 * THE RULES IT KEEPS.
 *
 * - `Mod` IS THE PLATFORM'S COMMAND KEY: Command on Apple platforms, Control
 *   everywhere else, matching handlers that accept either `metaKey` or
 *   `ctrlKey`. Option, Shift, Backspace and the rest get the glyph the
 *   platform prints on the key.
 * - THE GLYPHS ARE DECORATION; THE WORDS ARE THE HINT. Each key is drawn as a
 *   `<kbd>` hidden from assistive technology, beside one sentence it reads
 *   instead: "Command plus Shift plus Z".
 */

import type { ReactNode } from "react";

/** Whether the reader is on an Apple platform, where Command is the command key. */
export function isApplePlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  const hinted = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData
    ?.platform;
  return /mac|iphone|ipad|ipod/i.test(hinted || navigator.platform || navigator.userAgent);
}

const NAMED: Record<string, { apple: [string, string]; other: [string, string] }> = {
  Mod: { apple: ["⌘", "Command"], other: ["Ctrl", "Control"] },
  Ctrl: { apple: ["⌃", "Control"], other: ["Ctrl", "Control"] },
  Alt: { apple: ["⌥", "Option"], other: ["Alt", "Alt"] },
  Shift: { apple: ["⇧", "Shift"], other: ["Shift", "Shift"] },
  Enter: { apple: ["↵", "Return"], other: ["Enter", "Enter"] },
  Escape: { apple: ["Esc", "Escape"], other: ["Esc", "Escape"] },
  Backspace: { apple: ["⌫", "Delete"], other: ["Backspace", "Backspace"] },
  Delete: { apple: ["⌦", "Forward delete"], other: ["Del", "Delete"] },
  Tab: { apple: ["⇥", "Tab"], other: ["Tab", "Tab"] },
  Space: { apple: ["Space", "Space"], other: ["Space", "Space"] },
  ArrowUp: { apple: ["↑", "Up arrow"], other: ["↑", "Up arrow"] },
  ArrowDown: { apple: ["↓", "Down arrow"], other: ["↓", "Down arrow"] },
  ArrowLeft: { apple: ["←", "Left arrow"], other: ["←", "Left arrow"] },
  ArrowRight: { apple: ["→", "Right arrow"], other: ["→", "Right arrow"] },
  ContextMenu: { apple: ["Menu", "Menu key"], other: ["Menu", "Menu key"] },
  "+": { apple: ["+", "Plus"], other: ["+", "Plus"] },
  "-": { apple: ["-", "Minus"], other: ["-", "Minus"] },
  "=": { apple: ["=", "Equals"], other: ["=", "Equals"] },
};

/** How one key is drawn and how it is read. */
export function keyGlyph(key: string, apple: boolean): { glyph: string; spoken: string } {
  const named = NAMED[key];
  if (named) {
    const [glyph, spoken] = apple ? named.apple : named.other;
    return { glyph, spoken };
  }
  // A letter is printed in capitals on every keyboard.
  const shown = [...key].length === 1 ? key.toLocaleUpperCase() : key;
  return { glyph: shown, spoken: shown };
}

export function Kbd({
  keys,
  apple = isApplePlatform(),
}: {
  /** Pressed together, in order: `["Mod", "Shift", "z"]`. */
  keys: readonly string[];
  /** Overrides platform detection, for a suite or a screenshot. */
  apple?: boolean;
}): ReactNode {
  const mapped = keys.map((key) => keyGlyph(key, apple));
  return (
    <span className="kbd-combo">
      <span className="kbd-keys" aria-hidden="true">
        {mapped.map((k, i) => (
          <kbd key={i}>{k.glyph}</kbd>
        ))}
      </span>
      <span className="sr-only">{mapped.map((k) => k.spoken).join(" plus ")}</span>
    </span>
  );
}
