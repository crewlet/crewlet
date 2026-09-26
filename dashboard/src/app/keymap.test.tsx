/**
 * The one keymap: every key the dashboard answers is a row of `keymap.ts`,
 * no two rows answer one press where both can be live, the legend is the
 * table, and a key waits while the reader is typing or a modal is open.
 */

import { useRef, type ReactNode } from "react";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { LayerHost, Modal } from "@crewlethq/ui";
import { KEYMAP, KEY_SCOPES, keyRow, matchesRow, useKeymap, type KeyHandler } from "./keymap.ts";
import { KeyLegend } from "./KeyLegend.tsx";
import { WORKSPACES } from "./nav.ts";
import { focusSearchTarget, useSearchTarget } from "./searchTarget.ts";
import { modules, parse, stringValue, walk, type Node } from "~/test/source.ts";

afterEach(cleanup);

function press(key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const e = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init });
  act(() => {
    window.dispatchEvent(e);
  });
  return e;
}

/** A press's identity: what `lib/keys.ts` compares. */
function signature(p: { key: string; mod?: boolean; shift?: boolean; after?: string }): string {
  return `${p.after ? `${p.after} then ` : ""}${p.mod ? "Mod+" : ""}${p.shift ? "Shift+" : ""}${p.key}`;
}

describe("the table", () => {
  test("every row has one id", () => {
    const ids = KEYMAP.map((r) => r.id);
    expect(ids.filter((id, i) => ids.indexOf(id) !== i)).toEqual([]);
  });

  // THE PAGE'S SCOPES ARE LIVE TOGETHER — a list, with a peek open, on an
  // object with tabs, beside a chart — so a press two of them answer is a
  // press that does two things. The layer's rows are live only while every
  // page key stands aside, so they are their own namespace.
  test("no press is answered twice where both rows can be live", () => {
    for (const layer of [false, true]) {
      const seen = new Map<string, string>();
      const clashes: string[] = [];
      for (const row of KEYMAP.filter((r) => (r.scope === "layer") === layer)) {
        for (const p of row.presses) {
          const sig = signature(p);
          const other = seen.get(sig);
          if (other) clashes.push(`${sig}: ${other} and ${row.id}`);
          seen.set(sig, row.id);
        }
      }
      expect(clashes).toEqual([]);
    }
  });

  test("a sequence's prefix is no key of its own", () => {
    // `g` arms a prefix and swallows the next key, so a row bound to a bare
    // `g` would never fire and the sequences would never start.
    const prefixes = new Set(KEYMAP.flatMap((r) => r.presses.map((p) => p.after).filter(Boolean)));
    const bare = KEYMAP.flatMap((r) =>
      r.presses.filter((p) => !p.after && !p.mod && prefixes.has(p.key)).map(() => r.id),
    );
    expect(bare).toEqual([]);
  });

  test("every workspace is a `g` sequence, derived from the one navigation table", () => {
    for (const ws of WORKSPACES) {
      expect(keyRow(`go.${ws.key}`).presses).toEqual([{ after: "g", key: ws.chord }]);
    }
  });

  test("every scope a row names has a heading in the legend", () => {
    const scopes = new Set(KEY_SCOPES.map((s) => s.scope));
    expect(KEYMAP.filter((r) => !scopes.has(r.scope)).map((r) => r.id)).toEqual([]);
  });

  test("the digits are one row that says which was pressed", () => {
    const fired = vi.fn();
    function Tabs() {
      useKeymap({ tab: { run: (_, i) => fired(i), when: (i) => i < 3 } });
      return null;
    }
    render(<Tabs />);
    press("2");
    expect(fired).toHaveBeenLastCalledWith(1);
    // Past the end of the strip, the digit is left for whatever else wants it.
    const e = press("7");
    expect(fired).toHaveBeenCalledTimes(1);
    expect(e.defaultPrevented).toBe(false);
  });
});

describe("the legend", () => {
  test("renders every row of the table, grouped by where it is live", () => {
    render(
      <LayerHost>
        <KeyLegend onClose={() => {}} />
      </LayerHost>,
    );
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    const drawn = [...dialog.querySelectorAll("[data-key-row]")].map((el) =>
      el.getAttribute("data-key-row"),
    );
    expect(drawn.sort()).toEqual(KEYMAP.map((r) => r.id).sort());
    for (const row of KEYMAP) {
      const el = dialog.querySelector(`[data-key-row="${CSS.escape(row.id)}"]`)!;
      expect(el.textContent).toContain(row.does);
      // Under its scope's heading, which is what says where it works.
      const title = KEY_SCOPES.find((s) => s.scope === row.scope)!.title;
      const group = el.closest("section")!;
      expect(within(group).getByRole("heading").textContent).toBe(title);
    }
  });

  test("draws a key in words a screen reader can say", () => {
    render(
      <LayerHost>
        <KeyLegend onClose={() => {}} />
      </LayerHost>,
    );
    const palette = document.querySelector('[data-key-row="palette"]')!;
    expect(palette.textContent).toMatch(/(Command|Control) plus K/);
    const go = document.querySelector('[data-key-row="go.home"]')!;
    expect(go.textContent).toMatch(/G.*then.*H/);
  });
});

describe("binding a row", () => {
  function Bound({
    handlers,
    children,
  }: {
    handlers: Record<string, KeyHandler>;
    children?: ReactNode;
  }) {
    useKeymap(handlers);
    return (
      <>
        <input aria-label="field" />
        {children}
      </>
    );
  }

  test("fires on the table's key, and not on another", () => {
    const next = vi.fn();
    render(<Bound handlers={{ "list.next": next }} />);
    press("k");
    expect(next).not.toHaveBeenCalled();
    press("j");
    expect(next).toHaveBeenCalledTimes(1);
  });

  test("a bare key waits while the reader is typing; a row that says so does not", () => {
    const next = vi.fn();
    const close = vi.fn();
    render(<Bound handlers={{ "list.next": next, "peek.close": close }} />);
    act(() => screen.getByLabelText("field").focus());
    press("j");
    expect(next).not.toHaveBeenCalled();
    press("Escape");
    expect(close).toHaveBeenCalledTimes(1);
  });

  test("every page key waits while a modal is open", () => {
    const next = vi.fn();
    const close = vi.fn();
    render(
      <LayerHost>
        <Bound handlers={{ "list.next": next, "peek.close": close }}>
          <Modal open title="Over the page" onClose={() => {}}>
            body
          </Modal>
        </Bound>
      </LayerHost>,
    );
    press("j");
    press("Escape");
    expect(next).not.toHaveBeenCalled();
    expect(close).not.toHaveBeenCalled();
  });

  test("a press somebody already answered, or one an input method is composing, is not a key", () => {
    const open = vi.fn();
    render(<Bound handlers={{ "list.open": open }} />);
    const answered = new KeyboardEvent("keydown", {
      key: "Enter",
      bubbles: true,
      cancelable: true,
    });
    answered.preventDefault();
    act(() => {
      window.dispatchEvent(answered);
    });
    press("Enter", { isComposing: true });
    expect(open).not.toHaveBeenCalled();
    press("Enter");
    expect(open).toHaveBeenCalledTimes(1);
  });

  test("a row the design system binds is refused, and so is one nobody declared", () => {
    expect(() => render(<Bound handlers={{ "canvas.fit": () => {} }} />)).toThrow(/design system/);
    expect(() => render(<Bound handlers={{ "list.nxet": () => {} }} />)).toThrow(/no row/);
  });

  test("the builder's undo reads its presses from the table, Shift and a non-Latin layout included", () => {
    const undo = new KeyboardEvent("keydown", { key: "z", ctrlKey: true });
    const redo = new KeyboardEvent("keydown", { key: "Z", ctrlKey: true, shiftKey: true });
    const russian = new KeyboardEvent("keydown", { key: "я", code: "KeyZ", metaKey: true });
    expect(matchesRow("builder.undo", undo)).toBe(true);
    expect(matchesRow("builder.redo", undo)).toBe(false);
    expect(matchesRow("builder.redo", redo)).toBe(true);
    expect(matchesRow("builder.undo", redo)).toBe(false);
    expect(matchesRow("builder.undo", russian)).toBe(true);
    expect(matchesRow("builder.undo", new KeyboardEvent("keydown", { key: "z" }))).toBe(false);
  });
});

describe("a screen's search", () => {
  function Screen({ enabled = true }: { enabled?: boolean }) {
    const box = useRef<HTMLInputElement>(null);
    useSearchTarget(box, enabled);
    return <input aria-label="screen search" ref={box} defaultValue="old" />;
  }

  test("is focused, its text selected to type over", () => {
    render(<Screen />);
    expect(focusSearchTarget()).toBe(true);
    const box = screen.getByLabelText<HTMLInputElement>("screen search");
    expect(document.activeElement).toBe(box);
    expect(box.selectionStart).toBe(0);
    expect(box.selectionEnd).toBe(3);
  });

  test("is not there once the screen is gone, or while it is withdrawn", () => {
    const view = render(<Screen />);
    view.unmount();
    expect(focusSearchTarget()).toBe(false);
    render(<Screen enabled={false} />);
    expect(focusSearchTarget()).toBe(false);
  });

  test("the one mounted last wins", () => {
    render(<Screen />);
    const opened = vi.fn();
    function Later() {
      useSearchTarget(opened);
      return null;
    }
    render(<Later />);
    expect(focusSearchTarget()).toBe(true);
    expect(opened).toHaveBeenCalledTimes(1);
    expect(document.activeElement).not.toBe(screen.getByLabelText("screen search"));
  });
});

// ONE TABLE HOLDS ONLY IF NOTHING BINDS A KEY BESIDE IT. The mechanism in
// `lib/keys.ts` is called from the table's `useKeymap` alone, and a raw
// `keydown` listener is a binding the legend and the collision check cannot
// see — except the two that read the table themselves.
describe("the table is the only way a key is bound", () => {
  const LISTENERS: Record<string, string> = {
    "lib/keys.ts": "the mechanism useKeymap is built on",
    "routes/org/builder/Builder.tsx":
      "undo and redo, scoped to presses from inside the builder; it reads its keys with matchesRow",
  };

  test("nothing but the table calls the mechanism", () => {
    const callers: string[] = [];
    for (const mod of modules()) {
      if (mod.path === "lib/keys.ts") continue;
      walk(parse(mod.text, mod.lang), (node) => {
        if (node.type === "CallExpression") {
          const callee = node.callee as Node & { name?: string };
          if (callee.type === "Identifier" && callee.name === "useKeyChords")
            callers.push(mod.path);
        }
      });
    }
    expect(callers).toEqual(["app/keymap.ts"]);
  });

  test("no module listens for keys at the window or the document but those that read the table", () => {
    const listening = new Set<string>();
    for (const mod of modules()) {
      walk(parse(mod.text, mod.lang), (node) => {
        if (node.type !== "CallExpression") return;
        const callee = node.callee as Node & { property?: Node & { name?: string } };
        if (callee.type !== "MemberExpression" || callee.property?.name !== "addEventListener")
          return;
        const first = (node.arguments as Node[])[0];
        if (stringValue(first) === "keydown") listening.add(mod.path);
      });
    }
    expect([...listening].sort()).toEqual(Object.keys(LISTENERS).sort());
    const builder = modules().find((m) => m.path === "routes/org/builder/Builder.tsx")!;
    expect(builder.text).toContain('matchesRow("builder.undo"');
  });
});
