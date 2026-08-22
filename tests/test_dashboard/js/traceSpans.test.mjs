// Arranging a trace's events by their span graph.
//
// `arrangeSpans` turns a flat list of events into an indented tree, and
// three surfaces render the result — the Activity feed's trace groups,
// the trace screen, and event detail — so it moved into `traceNodes.js`
// when they had begun to draw it three ways.
//
// Its failure modes are all silent. A row whose parent is missing, or
// whose parents form a cycle, must still appear: a trace can be
// truncated at the store's cap, and a parent can have been written by a
// process whose events did not survive. Dropping such a row loses part
// of a turn from a screen whose whole job is to show the whole turn.
//
// The paging and filtering that used to be tested here moved to
// activity.test.mjs with the view.

import assert from "node:assert";
import { test, run } from "./harness.mjs";

const { arrangeSpans } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/traceNodes.js", import.meta.url)
);

test("a trace nests children under their parent span", () => {
  const rows = [
    { id: "root", span_id: "s1", parent_span_id: "" },
    { id: "child", span_id: "s2", parent_span_id: "s1" },
    { id: "grandchild", span_id: "s3", parent_span_id: "s2" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => [n.event.id, n.depth]),
    [
      ["root", 0],
      ["child", 1],
      ["grandchild", 2],
    ],
  );
});

test("an orphan renders at the root rather than vanishing", () => {
  // A trace can be truncated at the cap, or a parent can have been
  // written by a process whose events did not survive.
  const rows = [
    { id: "a", span_id: "s1", parent_span_id: "" },
    { id: "orphan", span_id: "s9", parent_span_id: "missing" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => n.event.id),
    ["a", "orphan"],
  );
});

test("sibling order is the order given, which is causal", () => {
  const rows = [
    { id: "root", span_id: "s1", parent_span_id: "" },
    { id: "first", span_id: "s2", parent_span_id: "s1" },
    { id: "second", span_id: "s3", parent_span_id: "s1" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => n.event.id),
    ["root", "first", "second"],
  );
});

test("a cycle in the span graph cannot lose a row or hang", () => {
  const rows = [
    { id: "a", span_id: "s1", parent_span_id: "s2" },
    { id: "b", span_id: "s2", parent_span_id: "s1" },
  ];
  const out = arrangeSpans(rows);
  assert.strictEqual(out.length, 2);
});

test("events with no spans at all still render, in order", () => {
  const rows = [
    { id: "a", span_id: "", parent_span_id: "" },
    { id: "b", span_id: "", parent_span_id: "" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => [n.event.id, n.depth]),
    [
      ["a", 0],
      ["b", 0],
    ],
  );
});

await run();
