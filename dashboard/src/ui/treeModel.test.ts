// @vitest-environment node
/**
 * One answer to "where does this key go" for both the canvas tree and the
 * outline treegrid.
 *
 * The fixture is the shape of a real company: a root seat, units nested two
 * deep, and labels that collide on their first letter and start with a digit,
 * because those are the cases type-ahead and the zoom keys fight over.
 */

import { describe, expect, test } from "vitest";
import {
  allExpandable,
  ancestors,
  collapseOrAscend,
  createTree,
  expandOrDescend,
  expandTo,
  firstVisible,
  isTypeAheadKey,
  lastVisible,
  level,
  nextVisible,
  posInSet,
  previousVisible,
  setSize,
  typeAhead,
  typeAheadBuffer,
  visible,
  TYPE_AHEAD_RESET_MS,
  type TreeInput,
} from "./treeModel.ts";

const FOREST: TreeInput[] = [
  {
    id: "company",
    label: "Nimbus",
    children: [
      { id: "ceo", label: "Chief Executive" },
      {
        id: "eng",
        label: "Engineering",
        children: [
          {
            id: "sre",
            label: "Reliability",
            children: [{ id: "oncall", label: "Engineer on call" }],
          },
          { id: "swe", label: "Software Engineer" },
        ],
      },
      { id: "zero", label: "0 to 1" },
      { id: "gtm", label: "Go to Market" },
    ],
  },
  { id: "orphan", label: "External Advisor" },
];

const tree = createTree(FOREST);
const none = new Set<string>();
const all = allExpandable(tree);

describe("visible order", () => {
  test("is pre-order, skipping what a collapsed node holds", () => {
    expect(visible(tree, none)).toEqual(["company", "orphan"]);
    expect(visible(tree, new Set(["company"]))).toEqual([
      "company",
      "ceo",
      "eng",
      "zero",
      "gtm",
      "orphan",
    ]);
    expect(visible(tree, all)).toEqual([
      "company",
      "ceo",
      "eng",
      "sre",
      "oncall",
      "swe",
      "zero",
      "gtm",
      "orphan",
    ]);
  });

  test("next and previous walk exactly the visible order, in both directions", () => {
    for (const expanded of [none, new Set(["company", "eng"]), all]) {
      const rows = visible(tree, expanded);
      rows.forEach((id, i) => {
        expect(nextVisible(tree, expanded, id)).toBe(rows[i + 1] ?? null);
        expect(previousVisible(tree, expanded, id)).toBe(rows[i - 1] ?? null);
      });
      expect(firstVisible(tree)).toBe(rows[0]);
      expect(lastVisible(tree, expanded)).toBe(rows[rows.length - 1]);
    }
  });

  test("an expanded leaf is still a leaf", () => {
    expect(nextVisible(tree, new Set(["company", "ceo"]), "ceo")).toBe("eng");
  });
});

describe("Right and Left", () => {
  const expanded = new Set(["company"]);

  test("Right expands a closed node, then moves to its first child, and does nothing on a leaf", () => {
    expect(expandOrDescend(tree, expanded, "eng")).toEqual({ expand: "eng" });
    expect(expandOrDescend(tree, new Set(["company", "eng"]), "eng")).toEqual({ focus: "sre" });
    expect(expandOrDescend(tree, expanded, "ceo")).toBeNull();
  });

  test("Left collapses an open node, otherwise moves to the parent, and does nothing on a closed root", () => {
    expect(collapseOrAscend(tree, expanded, "company")).toEqual({ collapse: "company" });
    expect(collapseOrAscend(tree, expanded, "eng")).toEqual({ focus: "company" });
    expect(collapseOrAscend(tree, none, "orphan")).toBeNull();
  });
});

test("level, set size and position are what aria-level, aria-setsize and aria-posinset need", () => {
  expect([level(tree, "company"), setSize(tree, "company"), posInSet(tree, "company")]).toEqual([
    1, 2, 1,
  ]);
  expect([level(tree, "gtm"), setSize(tree, "gtm"), posInSet(tree, "gtm")]).toEqual([2, 4, 4]);
  expect([level(tree, "oncall"), setSize(tree, "oncall"), posInSet(tree, "oncall")]).toEqual([
    4, 1, 1,
  ]);
});

test("revealing a deep node opens exactly its ancestors, and keeps what was already open", () => {
  expect(ancestors(tree, "oncall")).toEqual(["company", "eng", "sre"]);
  expect([...expandTo(tree, new Set(["gtm"]), "oncall")].sort()).toEqual(
    ["company", "eng", "gtm", "sre"].sort(),
  );
});

describe("type-ahead", () => {
  const open = new Set(["company", "eng"]);

  test("matches the start of a visible label, case-insensitively, after the current row", () => {
    expect(typeAhead(tree, open, "company", "s")).toBe("swe");
    expect(typeAhead(tree, open, "company", "SOFT")).toBe("swe");
    // Reliability's child is not visible, so "engineer on call" is not a match.
    expect(typeAhead(tree, open, "swe", "engineer o")).toBeNull();
  });

  test("with nothing focused it starts at the top", () => {
    expect(typeAhead(tree, open, null, "n")).toBe("company");
    expect(typeAhead(tree, open, null, "ni")).toBe("company");
  });

  test("the same letter again cycles through the rows it starts, wrapping", () => {
    expect(typeAhead(tree, all, "company", "e")).toBe("eng");
    expect(typeAhead(tree, all, "eng", "ee")).toBe("oncall");
    expect(typeAhead(tree, all, "oncall", "eee")).toBe("orphan");
    expect(typeAhead(tree, all, "orphan", "eeee")).toBe("eng");
  });

  test("a longer word stays on the row it already matches", () => {
    expect(typeAhead(tree, open, "eng", "en")).toBe("eng");
  });

  test("never starts a word with a zoom or fit key, a modified key or a space", () => {
    for (const key of ["+", "-", "0"]) expect(isTypeAheadKey({ key })).toBe(false);
    expect(isTypeAheadKey({ key: "z", ctrlKey: true })).toBe(false);
    expect(isTypeAheadKey({ key: "ArrowDown" })).toBe(false);
    expect(isTypeAheadKey({ key: " " })).toBe(false);
    expect(isTypeAheadKey({ key: " " }, "go")).toBe(true);
    expect(isTypeAheadKey({ key: "1" })).toBe(true);
    // Inside a word they are ordinary characters: "Q3 2026" is typed whole.
    expect(isTypeAheadKey({ key: "0" }, "q3 2")).toBe(true);
    expect(isTypeAheadKey({ key: "-" }, "on")).toBe(true);
  });

  test("a pause longer than the reset starts a new word", () => {
    let state = typeAheadBuffer({ text: "", at: 0 }, "g", 1000);
    state = typeAheadBuffer(state, "o", 1000 + TYPE_AHEAD_RESET_MS - 1);
    expect(state.text).toBe("go");
    state = typeAheadBuffer(state, "s", 1000 + 2 * TYPE_AHEAD_RESET_MS + 1);
    expect(state.text).toBe("s");
  });
});

test("a duplicated id is refused by name", () => {
  expect(() =>
    createTree([
      { id: "a", label: "A" },
      { id: "a", label: "A again" },
    ]),
  ).toThrow(/"a" appears twice/);
});
