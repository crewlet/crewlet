// Dashboard entry point: store + socket + router + chrome.
//
// The render contract every view implements:
//
//   { slices, mount(root), render(state) -> markup, onAction(), destroy() }
//
// `slices` names the parts of the store the view actually reads, so a
// health tick wakes only the header and a progress round wakes only the
// views showing agents. `render` returns markup and touches no DOM; the
// shell patches it in on the next animation frame. A view is therefore
// incapable of causing the full-page rebuild this dashboard used to do
// on every envelope.

import { Store } from "./store.js";
import { LiveSocket } from "./socket.js";
import { parseRoute, navigate, onRouteChange } from "./router.js";
import { patch } from "./patch.js";
import { schedule, cancel } from "./scheduler.js";
import { $, delegate } from "./dom.js";
import { esc, relTime } from "./format.js";
import { icon } from "./icons.js";
import { apiToken } from "./authToken.js";

import { createDashboardView } from "./views/dashboard.js";
import { createEventsView } from "./views/events.js";
import { createTokensView } from "./views/tokens.js";
import { createToolsView } from "./views/tools.js";
import { createOrgView } from "./views/org.js";
import { createCompanyView } from "./views/company.js";
import { createPeopleView } from "./views/people.js";
import { createAuditView } from "./views/audit.js";
import { createAgentsView } from "./views/agents.js";
import { createSchedulesView } from "./views/schedules.js";
import { createConfigView } from "./views/config.js";
import { createAgentView } from "./views/agent.js";
import { createEventDetailView } from "./views/eventDetail.js";
import { createTraceView } from "./views/trace.js";

const store = new Store();
const socket = new LiveSocket(store);
socket.setToken(apiToken());

const VIEWS = {
  dashboard: createDashboardView,
  company: createCompanyView,
  people: createPeopleView,
  org: createOrgView,
  audit: createAuditView,
  agents: createAgentsView,
  events: createEventsView,
  tokens: createTokensView,
  tools: createToolsView,
  schedules: createSchedulesView,
  config: createConfigView,
  agent: createAgentView,
  llm: createAgentView, // the agent view owns the llm sub-route
  eventDetail: createEventDetailView,
  trace: createTraceView,
};

const TITLES = {
  dashboard: "Dashboard",
  company: "Overview",
  people: "People Directory",
  org: "Org Chart",
  audit: "Audit log",
  agents: "Agents",
  events: "Activity",
  tokens: "Tokens",
  tools: "Tools",
  schedules: "Schedules",
  config: "Configuration",
  eventDetail: "Event",
  trace: "Trace",
};

let active = null;
let activeRoute = null;
let unsubscribeView = null;
let unsubscribeEvents = null;

// Context handed to every view. `query` is the only way a view reaches
// the server, and it goes over the same socket the pushes arrive on.
const ctx = {
  store,
  navigate,
  query: (what, params) => socket.query(what, params),
  setToken: (token) => socket.setToken(token),
  refresh: () => renderView(),
};

// ---- view mounting ----

function renderView() {
  if (!active) return;
  schedule("view", () => {
    if (!active) return;
    patch($("#view"), active.render(store.state));
  });
}

function mountRoute() {
  const route = parseRoute();
  const root = $("#view");

  if (active && active.destroy) active.destroy();
  if (unsubscribeView) unsubscribeView();
  if (unsubscribeEvents) unsubscribeEvents();
  cancel("view");
  root.innerHTML = "";

  const factory = VIEWS[route.name] || VIEWS.dashboard;
  active = factory({ ...ctx, params: route.params });
  activeRoute = route;
  if (active.mount) active.mount(root);

  unsubscribeView = store.subscribe(active.slices || [], renderView);
  unsubscribeEvents = active.onEvent
    ? store.onEvent((ev) => active.onEvent(ev))
    : null;

  // First paint is synchronous: the frame budget matters for updates,
  // not for the one render the reader is waiting on.
  patch(root, active.render(store.state));

  setTitle(route);
  renderNav();
  window.scrollTo(0, 0);
  $("#app").classList.remove("nav-open");
}

function setTitle(route) {
  let title = TITLES[route.name] || "Dashboard";
  if (route.name === "agent" || route.name === "llm") {
    const agent = store.agentById(route.params.id);
    title = agent ? agent.role || agent.name : "Agent";
  }
  $("#title").textContent = title;
  document.title = `${title} · Crewlet`;
}

// ---- sidebar ----
// A flat list, with `Company` as the one collapsible group — every entry
// resolves to a view backed by real data.
const NAV = [
  { name: "dashboard", icon: "grid", label: "Dashboard" },
  {
    group: "company",
    icon: "building",
    label: "Company",
    items: [
      { name: "company", icon: "grid", label: "Overview" },
      { name: "people", icon: "users", label: "People Directory" },
      { name: "org", icon: "hash", label: "Org Chart" },
      { name: "audit", icon: "refresh", label: "Audit log" },
    ],
  },
  { name: "agents", icon: "user", label: "Agents" },
  { name: "events", icon: "activity", label: "Activity", count: "events" },
  { name: "tokens", icon: "zap", label: "Tokens" },
  { name: "tools", icon: "wrench", label: "Tools" },
  { name: "schedules", icon: "clock", label: "Schedules" },
  { name: "config", icon: "database", label: "Configuration" },
];

const GROUP_KEY = "crewlet-nav-groups";
// Read once: this used to be re-read and JSON-parsed on every store
// emit, which meant a localStorage hit per LLM round.
let collapsed = readCollapsedGroups();

function readCollapsedGroups() {
  try {
    return new Set(JSON.parse(localStorage.getItem(GROUP_KEY) || "[]"));
  } catch {
    return new Set();
  }
}

function toggleGroup(name) {
  if (collapsed.has(name)) collapsed.delete(name);
  else collapsed.add(name);
  localStorage.setItem(GROUP_KEY, JSON.stringify([...collapsed]));
  renderNav();
}

function renderNav() {
  schedule("nav", () => {
    const current = activeRoute ? activeRoute.name : "dashboard";
    const events = store.state.events;
    // A count in the nav ring is only worth the pixels if it tells you
    // whether to click. "44 events" never does; "2 failed" always does.
    // So Activity carries its failure count in red when there is one and
    // falls back to the plain total when there is not.
    const failures = events.filter((e) => e.failed).length;
    const counts = {
      events: failures
        ? { value: failures, alert: true, title: `${failures} failed` }
        : { value: events.length, alert: false, title: `${events.length} events` },
    };

    const item = (n, sub = false) => {
      const count = n.count ? counts[n.count] : null;
      return `
      <div class="nav-item ${sub ? "sub" : ""} ${current === n.name ? "active" : ""}"
           data-k="nav:${n.name}" data-action="nav" data-nav="${n.name}">
        ${icon(n.icon, "sm")}<span class="label">${esc(n.label)}</span>
        ${
          count && count.value
            ? `<span class="nav-count ${count.alert ? "alert" : ""}" title="${esc(count.title)}">${esc(String(count.value))}</span>`
            : ""
        }
      </div>`;
    };

    const group = (g) => {
      const open = !collapsed.has(g.group);
      const holds = g.items.some((i) => i.name === current);
      return `
      <div class="nav-group ${open ? "open" : ""}" data-k="grp:${g.group}">
        <div class="nav-item nav-group-head ${holds && !open ? "active" : ""}"
             data-action="nav-group" data-group="${esc(g.group)}">
          ${icon(g.icon, "sm")}<span class="label">${esc(g.label)}</span>
          ${icon("chevron", "chevron")}
        </div>
        <div class="nav-group-items">${g.items.map((i) => item(i, true)).join("")}</div>
      </div>`;
    };

    patch(
      $("#nav"),
      `<div class="nav-section">${NAV.map((n) => (n.group ? group(n) : item(n))).join("")}</div>`,
    );
  });
}

// ---- chrome (live dot + in-flight pill) ----
function renderChrome() {
  schedule("chrome", () => {
    const state = store.state;
    const health = state.health || {};
    const dot = $("#live-dot");
    dot.className =
      "live-dot " +
      (health.status === "ok"
        ? "ok"
        : health.status === "shutting_down"
          ? "drain"
          : state.connected
            ? "ok"
            : "down");

    // Degraded mode: the socket is down and the client is polling REST
    // every few seconds. The dot alone does not read as "this data may
    // be stale" — and a dashboard that is quietly wrong is worse than
    // one that says it cannot see.
    const banner = $("#degraded");
    if (state.connected) {
      banner.hidden = true;
    } else {
      banner.hidden = false;
      const last = state.events && state.events[0];
      $("#degraded-text").textContent = last
        ? `Not connected to the engine — showing the last state received, from ${relTime(last.timestamp)}.`
        : "Not connected to the engine — retrying.";
    }

    const footer = $("#chrome-footer");
    const pill = $("#inflight");
    const n = health.in_flight || 0;
    if (health.shutting_down) {
      pill.className = "inflight-pill drain";
      pill.textContent = `draining · ${n} in flight`;
      footer.hidden = false;
    } else if (n > 0) {
      pill.className = "inflight-pill";
      pill.textContent = `${n} in flight`;
      footer.hidden = false;
    } else {
      footer.hidden = true;
    }
  });
}

// ---- theme ----
function initTheme() {
  setTheme(localStorage.getItem("crewlet-theme") || "dark");
  $("#theme-btn").addEventListener("click", () => {
    const next =
      document.documentElement.getAttribute("data-theme") === "dark"
        ? "light"
        : "dark";
    setTheme(next);
    localStorage.setItem("crewlet-theme", next);
  });
}

function setTheme(theme) {
  document.documentElement.setAttribute("data-theme", theme);
  $("#theme-btn").innerHTML = icon(theme === "dark" ? "sun" : "moon", "sm");
}

// ---- global delegation ----
function initDelegation() {
  // One document-level click delegate. App-level navigation is handled
  // here; anything else is forwarded to the active view, so views never
  // attach (and leak) their own listeners on the reused #view element.
  delegate(document.body, "click", (action, el, ev) => {
    if (action === "nav") navigate("/" + el.dataset.nav);
    else if (action === "nav-group") toggleGroup(el.dataset.group);
    else if (action === "agent")
      navigate("/agents/" + encodeURIComponent(el.dataset.id));
    else if (action === "view-events") navigate("/events");
    else if (action === "view-tokens") navigate("/tokens");
    else if (action === "view-tools") navigate("/tools");
    else if (action === "view-agents") navigate("/agents");
    else if (action === "view-org") navigate("/org");
    else if (action === "view-schedules") navigate("/schedules");
    else if (active && active.onAction) active.onAction(action, el, ev);
  });
  $("#menu-btn").addEventListener("click", () =>
    $("#app").classList.toggle("nav-open"),
  );
}

// ---- boot ----
function boot() {
  initTheme();
  initDelegation();

  store.subscribe(["health"], renderChrome);
  store.subscribe(["events"], renderNav);
  store.subscribe(["events"], renderChrome);

  // The socket starts first so a query issued by the first view's mount
  // goes out as soon as the connection is up (it would queue either way,
  // but there is no reason to make the first paint wait a tick longer).
  socket.start();
  onRouteChange(mountRoute);
  mountRoute();
}

boot();

// Re-exported for inline debugging.
window.__crewlet = { store, socket };
