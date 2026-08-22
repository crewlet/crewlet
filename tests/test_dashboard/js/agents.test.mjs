// The Agents room: who is working, who is not, and why.
//
// This room exists because the answer was buried — Company → Org → Seats
// lens → a seat → the Now tab — and because the most prominent list of
// agents in the product, the pulse strip, was rendered clickable and
// handled nowhere.
//
// What it must not do is invent confidence. A page that cannot see the
// engine has to say so rather than report a calm zero, and "idle" must
// not be dressed as a status: an operator who learns that colour means
// nothing stops reading colour.

import assert from "node:assert";
import { readFileSync } from "node:fs";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

const { document } = installDom();

function parse(markup) {
  const el = document.createElement("div");
  el.innerHTML = markup;
  return el;
}
function walk(el, visit) {
  for (const kid of el.childNodes) {
    if (kid.nodeType !== 1) continue;
    visit(kid);
    walk(kid, visit);
  }
}
function all(el, predicate) {
  const out = [];
  walk(el, (node) => {
    if (predicate(node)) out.push(node);
  });
  return out;
}
function hasClass(node, name) {
  return (node.getAttribute("class") || "").split(/\s+/).includes(name);
}
const text = (node) => node.textContent || "";
const AGENTS_CSS = new URL(
  "../../../src/crewlet/static/dashboard/styles/rooms/agents.css",
  import.meta.url,
);
const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { createAgentsView, classify } = await import(new URL("views/agents.js", base));

const ORG = {
  name: "Nimbus",
  units: [
    {
      name: "Leadership",
      roles: [
        { name: "CEO", handle: "ceo", kind: "agent" },
        { name: "Founder", handle: "founder", kind: "human" },
      ],
      children: [
        {
          name: "Engineering",
          roles: [
            { name: "CTO", handle: "cto", kind: "agent" },
            { name: "Backend Engineer", handle: "backend", kind: "agent" },
          ],
        },
      ],
    },
  ],
};

const AGENTS = [
  { id: "a1", role: "CEO", handle: "ceo", state: "idle", updated_at: "2026-08-22T10:00:00Z" },
  {
    id: "a2",
    role: "CTO",
    handle: "cto",
    state: "working",
    current_phase: "execute",
    updated_at: "2026-08-22T10:05:00Z",
  },
  {
    id: "a3",
    role: "Backend Engineer",
    handle: "backend",
    state: "afk",
    afk_reason: "stall",
    updated_at: "2026-08-22T09:00:00Z",
  },
];

function state(over = {}) {
  return {
    org: ORG,
    agents: AGENTS,
    sandboxes: [],
    events: [],
    tokens: [],
    budget: {},
    connected: true,
    health: { status: "ok", configured: true },
    ...over,
  };
}

function view(params = {}) {
  const navigated = [];
  const v = createAgentsView({
    store: { state: state(), subscribe: () => () => {} },
    navigate: (to) => navigated.push(to),
    refresh: () => {},
    params,
  });
  v.__navigated = navigated;
  return v;
}

// ---------------------------------------------------------------------
// The grouping, which is the room's whole argument
// ---------------------------------------------------------------------

test("a seat parked on a question needs a person but is not broken", () => {
  // Both go in the same group and only one is red. The state enum cannot
  // express that difference, which is why the grouping does.
  const v = classify(
    { name: "CTO" },
    {
      agent: { id: "a2", state: "working" },
      sandbox: { status: "awaiting_input" },
      sandboxes: [{ role: "CTO", status: "awaiting_input" }],
    },
  );
  assert.equal(v.group, "needs");
  assert.equal(v.broken, false);
});

test("a stopped seat is broken", () => {
  const v = classify({ name: "X" }, { agent: { id: "a", state: "afk" }, sandboxes: [] });
  assert.equal(v.group, "needs");
  assert.equal(v.broken, true);
});

test("an error outranks whatever state the seat claims", () => {
  // A seat can report `idle` and still be holding a failure; the failure
  // is the fact worth surfacing.
  const v = classify(
    { name: "X" },
    { agent: { id: "a", state: "idle", last_error: "provider refused" }, sandboxes: [] },
  );
  assert.equal(v.group, "needs");
  assert.equal(v.broken, true);
});

test("a seat with no live agent row is 'not running', not idle", () => {
  // Idle means a running process with an empty inbox. No process at all
  // is a different fact and on a fleet it means no node claimed the seat.
  const v = classify({ name: "X" }, { agent: undefined, sandboxes: [] });
  assert.equal(v.group, "off");
  assert.equal(v.broken, false);
});

test("human seats never appear — nothing runs them", () => {
  const el = parse(view().render(state()));
  assert.ok(!text(el).includes("Founder"), "a human seat was listed as an agent");
});

// ---------------------------------------------------------------------
// What the page shows
// ---------------------------------------------------------------------

test("the groups are ordered by urgency, not by config order", () => {
  const el = parse(view().render(state()));
  const heads = all(el, (n) => hasClass(n, "ag-group")).map((n) =>
    n.getAttribute("data-k"),
  );
  assert.deepEqual(heads, ["g:needs", "g:working", "g:idle"]);
});

test("inside a group the oldest is first", () => {
  // A question asked on Friday outranks one asked a minute ago: it is
  // the one nobody has looked at.
  const agents = [
    { id: "a1", role: "CEO", handle: "ceo", state: "afk", updated_at: "2026-08-22T10:00:00Z" },
    { id: "a2", role: "CTO", handle: "cto", state: "afk", updated_at: "2026-08-20T08:00:00Z" },
  ];
  const el = parse(view().render(state({ agents })));
  const rows = all(el, (n) => hasClass(n, "ag-row")).map((n) => n.getAttribute("data-k"));
  assert.deepEqual(rows.slice(0, 2), ["ag:cto", "ag:ceo"]);
});

test("every row offers a direct route to the LLM calls", () => {
  // The whole point of the room: the calls were behind a lens, a seat and
  // a tab, and nobody found them.
  const el = parse(view().render(state()));
  const calls = all(el, (n) => n.getAttribute("data-action") === "seat-calls");
  assert.equal(calls.length, 3, "one per agent seat");
});

test("the calls button lands on the turns, not the seat's default tab", () => {
  const v = view();
  v.onAction("seat-calls", { dataset: { seat: "cto" } });
  assert.deepEqual(v.__navigated, ["/seats/cto?tab=now"]);
});

test("nothing helper-generated is escaped into visible markup", () => {
  // `avatarFor` returns MARKUP, not a letter. Wrapping it in `esc` printed
  // `<span class="avatar" style=...>` across the face of every row, and
  // every assertion here still passed — because escaped markup IS text
  // content, which is all the other tests look at. So this looks for the
  // shape of the mistake rather than for one helper.
  const html = view().render(state());
  assert.ok(!/&lt;(span|svg|div|button)\b/i.test(html), "markup was escaped into text");
});

test("a working seat says what it is working on", () => {
  const el = parse(view().render(state()));
  assert.ok(text(el).includes("execute"), "the phase is not shown");
});

// ---------------------------------------------------------------------
// Honesty
// ---------------------------------------------------------------------

test("a disconnected page reports no counts at all", () => {
  // Losing the socket freezes every slice this room reads. A confident
  // "0 stopped" drawn at that moment is the single most dangerous number
  // the page could show.
  const el = parse(view().render(state({ connected: false })));
  const counts = all(el, (n) => hasClass(n, "ag-chip-n")).map((n) => text(n).trim());
  assert.deepEqual(counts, ["–", "–", "–", "–"]);
  assert.match(text(el), /not live/i);
});

test("a connected page does report them", () => {
  const el = parse(view().render(state()));
  const counts = all(el, (n) => hasClass(n, "ag-chip-n")).map((n) => text(n).trim());
  assert.deepEqual(counts, ["1", "1", "1", "0"]);
});

test("idle is given no hue — quiet is not a status", () => {
  // Colouring a healthy company makes an operator stop reading colour,
  // which is what the red is for.
  const src = readFileSync(AGENTS_CSS, "utf8");
  assert.ok(
    !/\.ag-chip\.tone-idle[^}]*color:/.test(src),
    "idle was given a colour",
  );
  assert.ok(
    !/\.ag-chip\.tone-off[^}]*color:/.test(src),
    "not-running was given a colour",
  );
});

test("red is reserved for something that broke", () => {
  // Not "the file mentions red somewhere": every RULE that reaches for it
  // has to be selecting a broken state. A status hue that leaks onto an
  // ordinary row is how an operator learns to stop trusting it.
  const src = readFileSync(AGENTS_CSS, "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
  const rules = [...src.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
    selector: m[1].trim(),
    body: m[2],
  }));
  const reds = rules.filter((r) => /var\(--red/.test(r.body));
  assert.ok(reds.length, "the room never uses red at all");
  for (const rule of reds) {
    assert.match(
      rule.selector,
      /is-broken/,
      `red outside a broken state: ${rule.selector}`,
    );
  }
});

test("the room defines no colour of its own", () => {
  const src = readFileSync(AGENTS_CSS, "utf8");
  const hex = src.replace(/\/\*[\s\S]*?\*\//g, "").match(/#[0-9a-fA-F]{3,8}\b/g);
  assert.equal(hex, null, `literal colours in the room stylesheet: ${hex}`);
});

test("no seats says so rather than rendering an empty frame", () => {
  const el = parse(
    view().render(state({ org: { name: "Empty", units: [] }, agents: [] })),
  );
  assert.match(text(el), /No agent seats|nothing received|No company/i);
});

await run();
