// The Org room — four lenses over one company.
//
// The room replaced four screens, and the two things that were never on
// any of them are what this suite is mostly about:
//
//   - the reporting relationship, DRAWN. The old org screen was an
//     indented list, which shows nesting in the unit tree and says
//     nothing about who answers to whom — an explicit `manages[]` entry
//     crosses units, and an indent cannot express that at all.
//   - the lens in the URL, so a view of the company is a link. The old
//     screens were four routes; collapsing them into one room without
//     round-tripping the lens would have turned three bookmarks into one.
//
// `manages` is free text in a hand-written config, so the tree it
// describes is not guaranteed to be a tree: it can name a seat that no
// longer exists, and it can close a cycle. Both are tested here, because
// the failure mode of the second is an infinite recursion in a render.

import assert from "node:assert";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { DASHBOARD_DIR, JS_URL } from "./dashboardRoot.mjs";

const { document } = installDom();
const DASH = DASHBOARD_DIR;
const base = JS_URL;
const { createOrgRoomView, buildReporting } = await import(
  new URL("views/orgRoom.js", base)
);
const { flattenSeats } = await import(new URL("org.js", base));

// The location bar the view writes its lens back into. `replaceParams`
// reads `location.hash` and writes through `history.replaceState`, so the
// round-trip is testable without a browser.
globalThis.location = { origin: "http://localhost", hash: "#/org" };
globalThis.history = {
  replaceState(_state, _title, url) {
    globalThis.location.hash = String(url);
  },
};

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

// A company whose reporting lines are NOT its unit nesting: the CEO sits
// at the root and manages the CTO explicitly, across a unit boundary.
function nimbus() {
  return {
    name: "Nimbus",
    mission: "Ship weather data people trust.",
    vision: "A forecast for every rooftop.",
    policies: ["No customer data in prompts"],
    roles: [
      { name: "CEO", handle: "ceo", goal: "Run the company", manages: ["CTO"] },
    ],
    units: [
      {
        name: "Engineering",
        type: "department",
        purpose: "Build the platform",
        lead: "CTO",
        roles: [
          { name: "CTO", handle: "cto", goal: "Own the architecture" },
          { name: "Backend Engineer", handle: "backend", goal: "Write services" },
          {
            name: "Ops Lead",
            handle: "ops",
            kind: "human",
            email: "ops@example.com",
            availability: "Mon–Thu, EU hours",
            contact: { slack_user_id: "U0FOUNDER" },
          },
        ],
      },
    ],
  };
}

const AGENTS = [
  { id: "a1", role: "CEO", handle: "ceo", state: "idle" },
  { id: "a2", role: "CTO", handle: "cto", state: "working", current_phase: "plan" },
  {
    id: "a3",
    role: "Backend Engineer",
    handle: "backend",
    state: "afk",
    afk_reason: "stall",
  },
];

// The CTO has a detached coding run in flight, so its effective state is
// `awaiting_sandbox` even though its kick-off turn already completed.
const SANDBOXES = [{ role: "CTO", status: "running", coding_agent: "claude" }];

function state(over = {}) {
  return {
    connected: true,
    health: { status: "ok", configured: true },
    org: nimbus(),
    agents: AGENTS,
    sandboxes: SANDBOXES,
    events: [],
    tokens: null,
    budget: {},
    ...over,
  };
}

function view(params = {}, hooks = {}) {
  return createOrgRoomView({
    params,
    navigate: hooks.navigate || (() => {}),
    refresh: hooks.refresh || (() => {}),
  });
}

// ---------------------------------------------------------------------
// Markup helpers
// ---------------------------------------------------------------------

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

function findByKey(el, key) {
  let hit = null;
  walk(el, (node) => {
    if (!hit && node.getAttribute("data-k") === key) hit = node;
  });
  return hit;
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

/** The seats reporting directly to the branch keyed `node:<name>`. */
function reportsOf(el, name) {
  const branch = findByKey(el, `node:${name}`);
  if (!branch) return null;
  const kids = branch.children.find((c) => hasClass(c, "org-kids"));
  if (!kids) return [];
  return kids.children
    .filter((c) => hasClass(c, "org-branch"))
    .map((c) => (c.getAttribute("data-k") || "").replace(/^node:/, ""));
}

// ---------------------------------------------------------------------
// The chart
// ---------------------------------------------------------------------

test("the chart is the default lens and its tab is the selected one", () => {
  const el = parse(view().render(state()));
  const tabs = all(el, (n) => hasClass(n, "org-lens-tab"));
  assert.equal(tabs.length, 3, "three lenses");
  const on = tabs.filter((t) => hasClass(t, "on"));
  assert.equal(on.length, 1, "exactly one lens is selected");
  assert.equal(on[0].getAttribute("data-k"), "chart");
  assert.equal(on[0].getAttribute("aria-selected"), "true");
});

test("an unknown lens falls back to the chart rather than an empty screen", () => {
  const el = parse(view({ lens: "wat" }).render(state()));
  const on = all(el, (n) => hasClass(n, "org-lens-tab") && hasClass(n, "on"));
  assert.equal(on[0].getAttribute("data-k"), "chart");
});

test("the chart draws the real reporting lines, not the unit nesting", () => {
  const el = parse(view().render(state()));
  // One root: the CEO. Everyone else hangs off somebody.
  const tree = all(el, (n) => hasClass(n, "org-tree"))[0];
  const roots = tree.children.filter((c) => hasClass(c, "org-branch"));
  assert.deepEqual(
    roots.map((r) => r.getAttribute("data-k")),
    ["node:CEO"],
  );
  // The explicit `manages: ["CTO"]` crosses a unit boundary — the CTO
  // sits inside Engineering and reports to a seat at the root.
  assert.deepEqual(reportsOf(el, "CEO"), ["CTO"]);
  // And the unit's lead picks up everybody else in the unit.
  assert.deepEqual(reportsOf(el, "CTO"), ["Backend Engineer", "Ops Lead"]);
  assert.deepEqual(reportsOf(el, "Backend Engineer"), []);
});

test("a lead never reports to itself", () => {
  // The CTO leads Engineering and sits in it. Taking the unit lead
  // literally would parent the CTO to the CTO — a subtree that contains
  // itself, which is an infinite render, not a chart.
  const seats = flattenSeats(nimbus());
  const { kids } = buildReporting(seats);
  assert.ok(!(kids.get("CTO") || []).some((s) => s.name === "CTO"));
});

test("a manages cycle still renders every seat", () => {
  // `manages` is free text: nothing stops two seats from naming each
  // other, and neither is then reachable from a root.
  const org = {
    name: "Loop",
    roles: [
      { name: "A", handle: "a", manages: ["B"] },
      { name: "B", handle: "b", manages: ["A"] },
    ],
  };
  const { roots } = buildReporting(flattenSeats(org));
  assert.equal(roots.length, 0, "the fixture no longer models a cycle");

  const el = parse(view().render(state({ org, agents: [], sandboxes: [] })));
  assert.ok(findByKey(el, "node:A"), "A vanished from the chart");
  assert.ok(findByKey(el, "node:B"), "B vanished from the chart");
  // Drawn once each: the cycle is broken, not walked twice.
  assert.equal(all(el, (n) => hasClass(n, "org-branch")).length, 2);
});

test("a seat whose manager is not in the org is planted as a root", () => {
  // A lead removed from the config leaves its reports pointing at a name
  // that resolves to nothing. They are still in the company.
  const org = {
    name: "Orphans",
    units: [
      {
        name: "Support",
        lead: "Somebody Who Left",
        roles: [{ name: "Agent One", handle: "one" }],
      },
    ],
  };
  const el = parse(view().render(state({ org, agents: [], sandboxes: [] })));
  const tree = all(el, (n) => hasClass(n, "org-tree"))[0];
  assert.deepEqual(
    tree.children.filter((c) => hasClass(c, "org-branch")).map((c) => c.getAttribute("data-k")),
    ["node:Agent One"],
  );
});

test("every node is keyed, and an agent node links to its seat page", () => {
  const el = parse(view().render(state()));
  const branches = all(el, (n) => hasClass(n, "org-branch"));
  assert.equal(branches.length, 4);
  for (const b of branches) {
    assert.match(b.getAttribute("data-k") || "", /^node:.+/, "an unkeyed branch");
  }
  const cto = findByKey(el, "node:CTO").children.find((c) => hasClass(c, "org-node"));
  assert.equal(cto.getAttribute("data-action"), "seat");
  assert.equal(cto.getAttribute("data-seat"), "cto", "addressed by handle");
  assert.equal(cto.getAttribute("tabindex"), "0", "a link answers the keyboard");
});

test("a node's dot is the seat's EFFECTIVE state", () => {
  const el = parse(view().render(state()));
  const dotOf = (name) => {
    const node = findByKey(el, `node:${name}`).children.find((c) =>
      hasClass(c, "org-node"),
    );
    return all(node, (n) => hasClass(n, "org-node-dot"))[0].getAttribute("data-state");
  };
  // The CTO's turn completed; its detached sandbox run has not.
  assert.equal(dotOf("CTO"), "awaiting_sandbox");
  assert.equal(dotOf("CEO"), "idle");
  assert.equal(dotOf("Backend Engineer"), "afk");
});

test("a human seat is marked human and given no runtime", () => {
  const el = parse(view().render(state()));
  const node = findByKey(el, "node:Ops Lead").children.find((c) =>
    hasClass(c, "org-node"),
  );
  assert.equal(node.getAttribute("data-action"), null, "a human was made clickable");
  assert.ok(hasClass(node, "is-human"));
  assert.equal(
    all(node, (n) => hasClass(n, "org-node-dot"))[0].getAttribute("data-state"),
    "human",
  );
  assert.match(node.innerHTML, /human/);
  assert.doesNotMatch(node.innerHTML, /offline/, "a human was given a run state");
});

test("a seat with no live agent row reads offline, not blank", () => {
  const el = parse(view().render(state({ agents: [], sandboxes: [] })));
  const node = findByKey(el, "node:CEO").children.find((c) => hasClass(c, "org-node"));
  assert.equal(
    all(node, (n) => hasClass(n, "org-node-dot"))[0].getAttribute("data-state"),
    "offline",
  );
});

// ---------------------------------------------------------------------
// The lens is a link
// ---------------------------------------------------------------------

test("the lens comes from the URL and switching one writes it back", () => {
  let refreshed = 0;
  globalThis.location.hash = "#/org";
  const v = view({ lens: "directory" }, { refresh: () => refreshed++ });
  assert.ok(
    all(parse(v.render(state())), (n) => hasClass(n, "org-lens-tab") && hasClass(n, "on"))[0]
      .getAttribute("data-k") === "directory",
  );

  v.onAction("org-lens", { dataset: { k: "charter" } });
  assert.equal(globalThis.location.hash, "#/org?lens=charter");
  assert.equal(refreshed, 1);
  const el = parse(v.render(state()));
  assert.match(el.innerHTML, /Ship weather data people trust/);
});

test("re-picking the lens already showing does nothing", () => {
  let refreshed = 0;
  const v = view({ lens: "seats" }, { refresh: () => refreshed++ });
  v.onAction("org-lens", { dataset: { k: "seats" } });
  v.onAction("org-lens", { dataset: { k: "not-a-lens" } });
  assert.equal(refreshed, 0);
});

test("the directory's filter is in the URL too, and defaults out of it", () => {
  let refreshed = 0;
  globalThis.location.hash = "#/org?lens=directory";
  const v = view({ lens: "directory" }, { refresh: () => refreshed++ });

  v.onAction("org-kind", { dataset: { k: "human" } });
  assert.equal(globalThis.location.hash, "#/org?lens=directory&kind=human");
  assert.equal(refreshed, 1);

  v.onAction("org-kind", { dataset: { k: "all" } });
  assert.equal(
    globalThis.location.hash,
    "#/org?lens=directory",
    "the default filter was written into the URL",
  );
});

test("a seat card's runtime-id click still resolves", () => {
  // The shared seat card addresses a seat by its runtime id and the shell
  // routes handles, so the room has to resolve the id form itself or its
  // own cards are inert.
  let went = "";
  const v = view({ lens: "seats" }, { navigate: (to) => (went = to) });
  v.onAction("agent", { dataset: { id: "a2" } });
  assert.equal(went, "/seats/a2");
});

// ---------------------------------------------------------------------
// Directory
// ---------------------------------------------------------------------

test("the directory says where a seat sits, who it answers to, and how to reach it", () => {
  const el = parse(view({ lens: "directory" }).render(state()));
  const row = findByKey(el, "seat:Backend Engineer");
  assert.ok(row, "the seat is missing from the directory");
  assert.match(row.textContent, /Engineering/);
  assert.match(row.textContent, /reports to CTO/);
  assert.match(row.textContent, /@backend/);
  assert.match(row.textContent, /Write services/);

  const human = findByKey(el, "seat:Ops Lead");
  assert.match(human.textContent, /U0FOUNDER/, "the contact chip is missing");
  assert.match(human.textContent, /ops@example.com/);
  assert.match(human.textContent, /Mon–Thu/);
  assert.equal(human.getAttribute("data-action"), null);
});

test("the Everyone / Agents / Humans filter narrows the list", () => {
  const el = parse(view({ lens: "directory", kind: "human" }).render(state()));
  assert.ok(findByKey(el, "seat:Ops Lead"));
  assert.equal(findByKey(el, "seat:CEO"), null, "an agent survived the human filter");
  // The counts are of the whole directory, not of what is shown, so the
  // pills say what picking one would give you.
  const pills = all(el, (n) => hasClass(n, "pill"));
  assert.deepEqual(
    pills.map((p) => p.getAttribute("data-k")),
    ["all", "agent", "human"],
  );
  assert.equal(pills.filter((p) => hasClass(p, "active")).length, 1);
  assert.match(pills[0].textContent, /4/);
  assert.match(pills[2].textContent, /1/);
});

test("a filter that matches nothing says so instead of showing a blank list", () => {
  const org = nimbus();
  // No human seats at all.
  org.units[0].roles = org.units[0].roles.filter((r) => r.kind !== "human");
  const el = parse(view({ lens: "directory", kind: "human" }).render(state({ org })));
  assert.match(el.textContent, /No human seats/);
  assert.doesNotMatch(
    el.textContent,
    /No company configuration is active/,
    "an empty filter was reported as an unconfigured engine",
  );
});

// ---------------------------------------------------------------------
// Charter
// ---------------------------------------------------------------------

test("the charter is the company's own account of itself", () => {
  const el = parse(view({ lens: "charter" }).render(state()));
  assert.match(el.textContent, /Nimbus/);
  assert.match(el.textContent, /Ship weather data people trust/);
  assert.match(el.textContent, /A forecast for every rooftop/);
  assert.match(el.textContent, /No customer data in prompts/);

  const unit = findByKey(el, "unit:Engineering");
  assert.ok(unit, "the unit list is missing");
  assert.match(unit.textContent, /Build the platform/);
  assert.match(unit.textContent, /lead · CTO/);
  assert.match(unit.textContent, /3 seats/);

  // Every policy and unit row is keyed.
  assert.ok(findByKey(el, "policy:No customer data in prompts"));
  // The seat count separates the two kinds rather than reporting one number.
  assert.match(el.textContent, /3 agent · 1 human/);
});

test("the charter's unit list links into the chart", () => {
  const el = parse(view({ lens: "charter" }).render(state()));
  const link = all(el, (n) => n.getAttribute("data-action") === "go")[0];
  assert.equal(link.getAttribute("data-route"), "/org?lens=chart");
});

// ---------------------------------------------------------------------
// Seats
// ---------------------------------------------------------------------



// ---------------------------------------------------------------------
// Honesty
// ---------------------------------------------------------------------

test("a disconnected page shows a skeleton, never an empty company", () => {
  const el = parse(
    view().render(state({ connected: false, org: {}, health: { status: "unknown" } })),
  );
  assert.ok(all(el, (n) => hasClass(n, "skel")).length, "no skeleton while blind");
  assert.doesNotMatch(el.textContent, /No company configured/);
});

test("an engine with no active configuration says that, not 'no seats'", () => {
  const el = parse(
    view().render(state({ org: {}, health: { status: "ok", configured: false } })),
  );
  assert.match(el.textContent, /No company configuration is active/);
});

test("a configured company with no seats keeps the lens bar", () => {
  const el = parse(view().render(state({ org: { name: "Empty", units: [] } })));
  assert.equal(all(el, (n) => hasClass(n, "org-lens-tab")).length, 3);
  assert.match(el.textContent, /No seats configured/);
});

test("markup carries no literal colour and escapes what it interpolates", () => {
  const org = nimbus();
  org.name = '<script>alert("x")</script>';
  org.roles[0].goal = "a & b < c";
  for (const lens of ["chart", "directory", "charter", "seats"]) {
    const markup = view({ lens }).render(state({ org }));
    assert.doesNotMatch(markup, /#[0-9a-fA-F]{3,8}\b/, `${lens} wrote a hex colour`);
    assert.doesNotMatch(markup, /<script>/, `${lens} did not escape`);
  }
});

// ---------------------------------------------------------------------
// The room's stylesheet
// ---------------------------------------------------------------------

test("the chart scrolls in its own container, and defines no colour", () => {
  const css = readFileSync(join(DASH, "styles/rooms/org.css"), "utf8");
  const chart = /\.org-chart \{([\s\S]*?)\}/.exec(css);
  assert.ok(chart, "org.css no longer styles .org-chart");
  assert.match(chart[1], /overflow-x:\s*auto/, "a wide chart would scroll the page");
  // Every colour is a token, and every space and type step comes off the
  // ramp — no literal px font sizes, no hand-written hex.
  assert.doesNotMatch(css, /#[0-9a-fA-F]{3,8}\b/, "org.css contains a hex colour");
  assert.doesNotMatch(css, /font-size:\s*\d/, "org.css hard-codes a font size");
  // The reporting lines are actually drawn.
  assert.match(css, /\.org-kids::before/);
  assert.match(css, /\.org-kids > \.org-branch::after/);
});

await run();
