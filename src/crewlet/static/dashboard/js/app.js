// Dashboard entry point: store + live stream + router + chrome.

import { api } from "./api.js";
import { Store } from "./store.js";
import { LiveStream } from "./ws.js";
import { parseRoute, navigate, onRouteChange } from "./router.js";
import { $, delegate } from "./dom.js";
import { esc } from "./format.js";
import { icon } from "./icons.js";

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

const store = new Store();
const stream = new LiveStream(store, api);

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
  llm: createAgentView, // agent view handles the llm sub-route
  eventDetail: createEventDetailView,
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
};

const ctx = { store, api, navigate };
let active = null;
let activeRoute = null;

// ---- view mounting ----
function mountRoute() {
  const route = parseRoute();
  const root = $("#view");
  if (active && active.destroy) active.destroy();
  root.innerHTML = "";

  const factory = VIEWS[route.name] || VIEWS.dashboard;
  active = factory({ ...ctx, params: route.params });
  activeRoute = route;
  active.mount(root);
  if (active.update) active.update(store.state);

  setTitle(route);
  renderNav(store.state);
  window.scrollTo(0, 0);
  $("#app").classList.remove("nav-open");
}

function setTitle(route) {
  let t = TITLES[route.name] || "Dashboard";
  if (route.name === "agent" || route.name === "llm") {
    const a = store.state.agents.find((x) => x.id === route.params.id);
    t = a ? a.role || a.name : "Agent";
  }
  $("#title").textContent = t;
  document.title = `${t} · Crewlet`;
}

// ---- sidebar ----
// A flat list, with `Company` as the one collapsible group — every entry
// resolves to a view backed by a real endpoint.
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

function collapsedGroups() {
  try {
    return new Set(JSON.parse(localStorage.getItem(GROUP_KEY) || "[]"));
  } catch {
    return new Set();
  }
}

function toggleGroup(name) {
  const set = collapsedGroups();
  if (set.has(name)) set.delete(name);
  else set.add(name);
  localStorage.setItem(GROUP_KEY, JSON.stringify([...set]));
  renderNav(store.state);
}

function renderNav(state) {
  const cur = activeRoute ? activeRoute.name : "dashboard";
  const collapsed = collapsedGroups();
  const counts = { events: (state.events || []).length };

  const item = (n, sub = false) => {
    const count = n.count ? counts[n.count] : 0;
    return `
      <div class="nav-item ${sub ? "sub" : ""} ${cur === n.name ? "active" : ""}"
           data-action="nav" data-nav="${n.name}">
        ${icon(n.icon, "sm")}<span class="label">${esc(n.label)}</span>
        ${count ? `<span class="nav-count">${esc(String(count))}</span>` : ""}
      </div>`;
  };

  const group = (g) => {
    const open = !collapsed.has(g.group);
    const holds = g.items.some((i) => i.name === cur);
    return `
      <div class="nav-group ${open ? "open" : ""}">
        <div class="nav-item nav-group-head ${holds && !open ? "active" : ""}"
             data-action="nav-group" data-group="${esc(g.group)}">
          ${icon(g.icon, "sm")}<span class="label">${esc(g.label)}</span>
          ${icon("chevron", "chevron")}
        </div>
        <div class="nav-group-items">${g.items.map((i) => item(i, true)).join("")}</div>
      </div>`;
  };

  $("#nav").innerHTML = `<div class="nav-section">
    ${NAV.map((n) => (n.group ? group(n) : item(n))).join("")}
  </div>`;
}

// ---- chrome (footer + live dot) ----
function renderChrome(state) {
  const h = state.health || {};
  const dot = $("#live-dot");
  const cls =
    h.status === "ok"
      ? "ok"
      : h.status === "shutting_down"
        ? "drain"
        : state.connected
          ? "ok"
          : "down";
  dot.className = "live-dot " + cls;

  const footer = $("#chrome-footer");
  const pill = $("#inflight");
  const n = h.in_flight || 0;
  if (h.shutting_down) {
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
}

// ---- theme ----
function initTheme() {
  const saved = localStorage.getItem("crewlet-theme") || "dark";
  setTheme(saved);
  $("#theme-btn").addEventListener("click", () => {
    const next =
      document.documentElement.getAttribute("data-theme") === "dark"
        ? "light"
        : "dark";
    setTheme(next);
    localStorage.setItem("crewlet-theme", next);
  });
}
function setTheme(t) {
  document.documentElement.setAttribute("data-theme", t);
  $("#theme-btn").innerHTML = icon(t === "dark" ? "sun" : "moon", "sm");
}

// ---- global delegation ----
function initDelegation() {
  // One document-level click delegate. App-level navigation is handled
  // here; anything else is forwarded to the active view's onAction, so
  // views never attach (and leak) their own listeners on the reused
  // #view element.
  delegate(document.body, "click", (action, el, ev) => {
    if (action === "nav") navigate("/" + el.dataset.nav);
    else if (action === "nav-group") toggleGroup(el.dataset.group);
    else if (action === "agent") navigate("/agents/" + encodeURIComponent(el.dataset.id));
    else if (action === "view-events") navigate("/events");
    else if (action === "view-tokens") navigate("/tokens");
    else if (action === "view-tools") navigate("/tools");
    else if (action === "view-agents") navigate("/agents");
    else if (action === "view-org") navigate("/org");
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

  store.subscribe((state) => {
    renderNav(state);
    renderChrome(state);
    if (active && active.update) active.update(state);
  });
  store.onEvent((ev) => {
    if (active && active.onEvent) active.onEvent(ev);
  });

  onRouteChange(mountRoute);
  mountRoute();
  stream.start();
}

boot();

// Re-export for any inline debugging.
window.__crewlet = { store, stream };
