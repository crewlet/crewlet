/**
 * The Home kit's one rule about what a widget may claim: never "nothing" over
 * an answer that could not account for everything.
 *
 *  - `gapsOf` says what the answers a widget draws from could not account
 *    for, once per kind of gap: a record this build cannot decode, a node that
 *    did not answer a fleet read, a roster nobody could read.
 *  - `WidgetQuery` draws a widget's answer: its claim only over an answer with
 *    no gap, "Nothing to list" and the gap over one with no rows, and the gap
 *    under the rows over one with rows.
 *  - THE SOURCE GATE: every widget module, and every card Home's own widgets
 *    draw, goes through `WidgetQuery` — never `QueryState` or `EmptyState`
 *    directly, which is how a widget would make its claim around the rule.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { INCOMPLETE_NOTE } from "~/components/work.tsx";
import { isLocalSource, modules, parse, type Module, type Node as AstNode } from "~/test/source.ts";
import { NOTHING_TO_LIST, WidgetQuery, gapsOf } from "./kit.tsx";

afterEach(cleanup);

const node = (id: string, answered: boolean) => ({
  id,
  answered,
  error: answered ? "" : "timeout",
});

describe("what the answers could not account for", () => {
  test("an answer that said nothing about its coverage has no gap", () => {
    expect(gapsOf([{}, null, undefined, { complete: true }])).toEqual([]);
    expect(gapsOf([{ coverage: { complete: true, nodes: [node("node-1", true)] } }])).toEqual([]);
  });

  test("a record this build cannot decode is said once, however many answers met one", () => {
    expect(gapsOf([{ complete: false }, { complete: false }, { complete: true }])).toEqual([
      INCOMPLETE_NOTE,
    ]);
  });

  test("a node that did not answer is named, and several are counted", () => {
    const one = { complete: false, nodes: [node("node-1", true), node("node-2", false)] };
    expect(gapsOf([{ coverage: one }])).toEqual([
      "Node node-2 didn’t answer, so this may be short.",
    ]);
    const two = { complete: false, nodes: [node("node-2", false), node("node-3", false)] };
    // THE SAME NODE IN TWO ANSWERS IS ONE NODE.
    expect(gapsOf([{ coverage: one }, { coverage: two }])).toEqual([
      "2 nodes didn’t answer, so this may be short.",
    ]);
  });

  test("a roster nobody could read is a gap with no node to name", () => {
    expect(gapsOf([{ coverage: { complete: false, nodes: [node("node-1", true)] } }])).toEqual([
      "The list of nodes couldn’t be read, so this may be short.",
    ]);
  });

  test("both kinds of gap are said, each once", () => {
    expect(
      gapsOf([
        { complete: false },
        { coverage: { complete: false, nodes: [node("node-2", false)] } },
      ]),
    ).toEqual([INCOMPLETE_NOTE, "Node node-2 didn’t answer, so this may be short."]);
  });
});

describe("a widget's answer, drawn", () => {
  const CLAIM = { title: "You’re caught up", hint: "New updates land here." };

  test("with nothing to list and no gap, the widget makes its claim", () => {
    render(<WidgetQuery error={null} loading={false} gaps={[]} empty={CLAIM} />);
    expect(screen.getByText("You’re caught up")).toBeTruthy();
  });

  test("with nothing to list and a gap, it says why it may be wrong instead", () => {
    render(<WidgetQuery error={null} loading={false} gaps={[INCOMPLETE_NOTE]} empty={CLAIM} />);
    expect(screen.getByText(NOTHING_TO_LIST)).toBeTruthy();
    expect(screen.getByText(INCOMPLETE_NOTE)).toBeTruthy();
    expect(screen.queryByText("You’re caught up")).toBeNull();
  });

  test("with rows and a gap, the rows are drawn and the gap is said under them", () => {
    const { container } = render(
      <WidgetQuery error={null} loading={false} gaps={[INCOMPLETE_NOTE]}>
        <ul>
          <li>ENG-42</li>
        </ul>
      </WidgetQuery>,
    );
    expect(screen.getByText("ENG-42")).toBeTruthy();
    const note = container.querySelector(".home-card-note");
    expect(note?.textContent).toBe(INCOMPLETE_NOTE);
    // UNDER THE ROWS, where a reader of the list meets it last.
    expect(container.querySelector("ul")!.compareDocumentPosition(note!)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  test("a refusal is the page's own, and neither the claim nor the gap is drawn", () => {
    render(<WidgetQuery error="timeout" loading={false} gaps={[INCOMPLETE_NOTE]} empty={CLAIM} />);
    expect(screen.queryByText("You’re caught up")).toBeNull();
    expect(screen.queryByText(NOTHING_TO_LIST)).toBeNull();
    expect(screen.queryByText(INCOMPLETE_NOTE)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The source gate
// ---------------------------------------------------------------------------

/** The two ways a widget could draw "nothing" without the kit deciding. */
const AROUND_THE_KIT = new Set(["QueryState", "EmptyState"]);

/** The names a module imports, value imports only. */
function importedNames(module: Module): string[] {
  const out: string[] = [];
  for (const statement of parse(module.text, module.lang).body as AstNode[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    for (const spec of statement.specifiers as AstNode[]) {
      if (spec.type !== "ImportSpecifier" || spec.importKind === "type") continue;
      const imported = spec.imported as AstNode;
      out.push(String(imported.type === "Identifier" ? imported.name : imported.value));
    }
  }
  return out;
}

/**
 * Every module a Home widget is drawn by: each section's `home.widgets.tsx`,
 * and the cards Home's own widgets render — read off `sections.tsx`'s own
 * imports, so a card added there is held here without a list to keep.
 */
function widgetModules(): Module[] {
  const all = modules();
  const sections = all.find((m) => m.path === "routes/home/sections.tsx");
  if (!sections) throw new Error("routes/home/sections.tsx is not in the tree");
  const cards = new Set<string>();
  for (const statement of parse(sections.text, sections.lang).body as AstNode[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    const source = String((statement.source as AstNode).value);
    if (isLocalSource(source) && source.startsWith("./") && source.endsWith(".tsx")) {
      cards.add(`routes/home/${source.slice(2)}`);
    }
  }
  return all.filter((m) => /^routes\/[^/]+\/home\.widgets\.tsx$/.test(m.path) || cards.has(m.path));
}

test("every widget draws its answer through the kit, never around it", () => {
  const read = widgetModules();
  // A FLOOR: ten sections offer widgets and Home draws seven cards, so a walk
  // that found fewer is reading the wrong tree.
  expect(read.length, "the gate read too few widget modules").toBeGreaterThan(15);
  const offenders = read.flatMap((m) =>
    importedNames(m)
      .filter((name) => AROUND_THE_KIT.has(name))
      .map((name) => `${m.path} imports ${name}`),
  );
  expect(offenders, "draw the answer through WidgetQuery, with its gaps").toEqual([]);
});
