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
//
// The nav is grouped by the question a room answers rather than by the
// kind of data it holds. Nine top-level nouns meant the questions an
// operator actually arrives with — is anything waiting on me, did
// anything break, what is this costing — each needed three or four
// screens and a mental join.

import { Store } from "./store.js";
import { LiveSocket } from "./socket.js";
import { parseRoute, navigate, onRouteChange } from "./router.js";
import { patch } from "./patch.js";
import { schedule, cancel } from "./scheduler.js";
import { $, delegate } from "./dom.js";
import { esc } from "./format.js";
import { icon } from "./icons.js";
import { apiToken, storeToken } from "./authToken.js";
import { promptModal } from "./modal.js";
import { buildAttention, attentionCounts } from "./attention.js";
import { buildResults, flatResults } from "./commandPalette.js";
import {
  dotClass,
  renderHealthPopover,
  bannerFor,
  bannerTone,
} from "./health.js";

import { createMissionView } from "./views/mission.js";
import { createWorkView } from "./views/work.js";
import { createActivityView } from "./views/activity.js";
import { createOrgRoomView } from "./views/orgRoom.js";
import { createSchedulesView } from "./views/schedules.js";
import { createSpendView } from "./views/spend.js";
import { createIntegrationsView } from "./views/integrations.js";
import { createFleetView } from "./views/fleet.js";
import { createConfigView } from "./views/config.js";
import { createToolsView } from "./views/tools.js";
import { createSeatView } from "./views/seat.js";
import { createEventDetailView } from "./views/eventDetail.js";
import { createTraceView } from "./views/trace.js";

const store = new Store();
const socket = new LiveSocket(store);
socket.setToken(apiToken());

const VIEWS = {
  mission: createMissionView,
  work: createWorkView,
  activity: createActivityView,
  org: createOrgRoomView,
  schedules: createSchedulesView,
  spend: createSpendView,
  integrations: createIntegrationsView,
  fleet: createFleetView,
  config: createConfigView,
  tools: createToolsView,
  seat: createSeatView,
  llm: createSeatView, // the seat view owns the llm sub-route
  eventDetail: createEventDetailView,
  trace: createTraceView,
};

const TITLES = {
  mission: "Mission Control",
  work: "Work",
  activity: "Activity",
  org: "Org",
  schedules: "Schedules",
  spend: "Spend & Budgets",
  integrations: "Integrations",
  fleet: "Fleet",
  config: "Configuration",
  tools: "Tools",
  eventDetail: "Event",
  trace: "Trace",
};

let active = null;
let activeRoute = null;
let unsubscribeView = null;
let unsubscribeEvents = null;

// Context handed to every view. `query` is the only way a view reaches
// the server, and it goes over the same socket the pushes arrive on.
// tests/test_dashboard/js/wiring.test.mjs holds every view's declared
// dependencies against these keys — the Fleet view once asked for an
// `api` that was never here, and failed silently for it.
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
  if (route.redirect) {
    navigate(route.redirect);
    return;
  }
  const root = $("#view");

  if (active && active.destroy) active.destroy();
  if (unsubscribeView) unsubscribeView();
  if (unsubscribeEvents) unsubscribeEvents();
  cancel("view");
  root.innerHTML = "";

  const factory = VIEWS[route.name] || VIEWS.mission;
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
  let title = TITLES[route.name] || "Mission Control";
  if (route.name === "seat" || route.name === "llm") {
    const seat = store.agentByKey(route.params.key);
    title = seat ? seat.role || seat.name : "Seat";
  }
  $("#title").textContent = title;
  // The count rides in the tab title so a backgrounded dashboard is a
  // pager. It is the total, not the failure count: a question waiting
  // four days is not a failure and still needs somebody.
  const counts = attentionCounts(buildAttention(store.state));
  const badge = counts && counts.total ? `(${counts.total}) ` : "";
  document.title = `${badge}${title} · Crewlet`;
}

// ---- sidebar ----
// Three zones, each the answer to one question. Every entry resolves to
// a view backed by real data — a nav item that leads to an empty screen
// is worse than no nav item.
const NAV = [
  {
    zone: "now",
    title: "Now",
    question: "What is happening, and what needs me?",
    items: [
      { name: "mission", icon: "grid", label: "Mission Control" },
      { name: "work", icon: "cpu", label: "Work" },
      { name: "activity", icon: "activity", label: "Activity", count: "failures" },
    ],
  },
  {
    zone: "company",
    title: "Company",
    question: "Who is this company?",
    items: [
      { name: "org", icon: "building", label: "Org" },
      { name: "schedules", icon: "clock", label: "Schedules" },
    ],
  },
  {
    zone: "operations",
    title: "Operations",
    question: "Is the machine healthy?",
    items: [
      { name: "spend", icon: "zap", label: "Spend & Budgets" },
      { name: "integrations", icon: "globe", label: "Integrations" },
      { name: "fleet", icon: "cpu", label: "Fleet" },
      { name: "config", icon: "database", label: "Configuration" },
      { name: "tools", icon: "wrench", label: "Tools" },
    ],
  },
];

function renderNav() {
  schedule("nav", () => {
    const current = activeRoute ? activeRoute.name : "mission";
    const state = store.state;
    const attention = buildAttention(state);
    const counts = attentionCounts(attention);
    const failures = state.events.filter((e) => e.failed).length;

    const item = (n) => {
      // Two different counts, deliberately. A zone badge counts open
      // obligations — things a person has to do. Activity's badge counts
      // failures in the retained feed, which is a different question and
      // was the only badge this nav had.
      let badge = null;
      if (n.count === "failures" && failures) {
        badge = { value: failures, alert: true, title: `${failures} failed` };
      }
      return `
      <div class="nav-item ${current === n.name ? "active" : ""}"
           data-k="nav:${n.name}" data-action="nav" data-nav="${n.name}"
           role="link" tabindex="0">
        ${icon(n.icon, "sm")}<span class="label">${esc(n.label)}</span>
        ${
          badge
            ? `<span class="nav-count ${badge.alert ? "alert" : ""}" title="${esc(badge.title)}">${esc(String(badge.value))}</span>`
            : ""
        }
      </div>`;
    };

    const zone = (z) => {
      // The zone badge is only drawn when the page can actually see the
      // engine. `attentionCounts` answers null while the socket is down,
      // because a confident zero during an outage is exactly the lie
      // this surface exists to prevent.
      const open = counts ? counts[z.zone] || 0 : 0;
      return `
      <div class="nav-zone" data-k="zone:${z.zone}">
        <div class="nav-zone-head" title="${esc(z.question)}">
          <span class="nav-title">${esc(z.title)}</span>
          ${open ? `<span class="zone-count" title="${open} waiting on you">${open}</span>` : ""}
        </div>
        ${z.items.map(item).join("")}
      </div>`;
    };

    patch($("#nav"), `<div class="nav-section">${NAV.map(zone).join("")}</div>`);
  });
}

// ---- command palette ----
// Chrome, not a view: it outlives whatever is under it, and it takes the
// keyboard while open.

let paletteOpen = false;
let paletteQuery = "";
let paletteIndex = 0;

function togglePalette(open) {
  paletteOpen = open === undefined ? !paletteOpen : open;
  if (paletteOpen) {
    paletteQuery = "";
    paletteIndex = 0;
  }
  renderPalette();
  if (paletteOpen) $("#palette-input")?.focus();
}

function paletteGroups() {
  return buildResults(store.state, paletteQuery);
}

function renderPalette() {
  const host = $("#palette");
  host.hidden = !paletteOpen;
  if (!paletteOpen) {
    host.innerHTML = "";
    return;
  }
  const groups = paletteGroups();
  const flat = flatResults(groups);
  if (paletteIndex >= flat.length) paletteIndex = Math.max(0, flat.length - 1);

  let cursor = -1;
  const body = groups
    .map(
      (group) => `
      <div class="pal-group" data-k="g:${group.title}">
        <div class="pal-group-title">${esc(group.title)}</div>
        ${group.items
          .map((hit) => {
            cursor++;
            return `<div class="pal-item ${cursor === paletteIndex ? "on" : ""}"
                 data-k="${esc(hit.id)}" data-action="palette-go"
                 data-route="${esc(hit.route)}" role="option"
                 aria-selected="${cursor === paletteIndex}">
              <span class="pal-label">${esc(hit.label)}</span>
              <span class="pal-hint">${esc(hit.hint)}</span>
            </div>`;
          })
          .join("")}
      </div>`,
    )
    .join("");

  patch(
    host,
    `<div class="modal-veil" data-action="palette-dismiss">
       <div class="palette" role="dialog" aria-modal="true" aria-label="Search">
         <input id="palette-input" class="palette-input" type="text"
                placeholder="Search seats, rooms, or paste an event or trace id"
                value="${esc(paletteQuery)}" autocomplete="off"
                spellcheck="false" role="combobox" aria-expanded="true"
                aria-controls="palette-results" />
         <div class="palette-results" id="palette-results" role="listbox">
           ${body || `<div class="pal-empty">Nothing matches “${esc(paletteQuery)}”.</div>`}
         </div>
       </div>
     </div>`,
  );
  const input = $("#palette-input");
  if (input && document.activeElement !== input) {
    input.focus();
    // Keep the caret at the end: the input is re-rendered on every
    // keystroke, and a patched value resets the selection to the start.
    input.setSelectionRange(paletteQuery.length, paletteQuery.length);
  }
}

function paletteKey(ev) {
  if (!paletteOpen) return false;
  const flat = flatResults(paletteGroups());
  if (ev.key === "Escape") {
    togglePalette(false);
    return true;
  }
  if (ev.key === "ArrowDown" || (ev.key === "n" && ev.ctrlKey)) {
    paletteIndex = Math.min(paletteIndex + 1, Math.max(0, flat.length - 1));
    renderPalette();
    return true;
  }
  if (ev.key === "ArrowUp" || (ev.key === "p" && ev.ctrlKey)) {
    paletteIndex = Math.max(paletteIndex - 1, 0);
    renderPalette();
    return true;
  }
  if (ev.key === "Enter") {
    const hit = flat[paletteIndex];
    if (hit) {
      togglePalette(false);
      navigate(hit.route);
    }
    return true;
  }
  return false;
}

// ---- chrome (live dot + health popover + in-flight pill) ----

// Popover state. `healthStream` holds the reply to the `stream` query —
// per-socket facts (dropped envelopes, queue depth) that cannot ride the
// shared health tick, because that tick encodes ONE JSON string for
// every client. It is fetched on open and cleared on close, so what the
// popover shows is always from this viewing rather than the last one.
let healthOpen = false;
let healthStream = null;

function toggleHealth(open) {
  healthOpen = open === undefined ? !healthOpen : open;
  $("#live-dot-btn").setAttribute("aria-expanded", healthOpen ? "true" : "false");
  if (healthOpen) {
    healthStream = null;
    ctx
      .query("stream")
      .then((data) => {
        if (!healthOpen) return;
        healthStream = data;
        renderChrome();
      })
      .catch(() => {
        // A store-less or standalone deployment can decline this one
        // query while the rest of the popover is perfectly answerable.
        // The row stays on "…" rather than the whole surface failing.
      });
  } else {
    healthStream = null;
  }
  renderChrome();
}

function renderChrome() {
  schedule("chrome", () => {
    const state = store.state;
    const health = state.health || {};
    $("#live-dot").className = "live-dot " + dotClass(health, state.connected);

    const pop = $("#health-pop");
    pop.hidden = !healthOpen;
    if (healthOpen) {
      patch(
        pop,
        renderHealthPopover(health, healthStream, state.events, state.connected),
      );
    } else if (pop.firstChild) {
      pop.innerHTML = "";
    }

    // Always-on chrome for the conditions that must never wait for a
    // click: a rejected API token (the socket will never come back on
    // its own), a socket that is down (the page is polling REST and may
    // be stale) and an engine with no active company configuration
    // (every inbound webhook is being turned away). A dashboard that is
    // quietly wrong is worse than one that says it cannot see.
    const banner = $("#degraded");
    const text = bannerFor(health, state.connected, state.events, state.authRejected);
    banner.hidden = !text;
    banner.className =
      "degraded " + bannerTone(health, state.connected, state.authRejected);
    if (text) $("#degraded-text").textContent = text;
    $("#degraded-action").hidden = !state.authRejected;

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

// ---- theme + density ----
function initTheme() {
  setTheme(localStorage.getItem("crewlet-theme") || "dark");
  setDensity(localStorage.getItem("crewlet-density") || "comfortable");
  $("#theme-btn").addEventListener("click", () => {
    const next =
      document.documentElement.getAttribute("data-theme") === "dark"
        ? "light"
        : "dark";
    setTheme(next);
    localStorage.setItem("crewlet-theme", next);
  });
  $("#density-btn").addEventListener("click", () => {
    const next =
      document.documentElement.getAttribute("data-density") === "compact"
        ? "comfortable"
        : "compact";
    setDensity(next);
    localStorage.setItem("crewlet-density", next);
  });
}

function setTheme(theme) {
  document.documentElement.setAttribute("data-theme", theme);
  $("#theme-btn").innerHTML = icon(theme === "dark" ? "sun" : "moon", "sm");
  $("#theme-btn").title = theme === "dark" ? "Switch to light" : "Switch to dark";
}

// One scalar drives the whole spacing ramp, so "fit more rows on screen"
// is a token swap rather than a second stylesheet.
function setDensity(density) {
  document.documentElement.setAttribute("data-density", density);
  $("#density-btn").innerHTML = icon(
    density === "compact" ? "rows-wide" : "rows-tight",
    "sm",
  );
  $("#density-btn").title =
    density === "compact" ? "Comfortable spacing" : "Compact spacing";
}

// ---- token entry ----
// One flow, shared by the chrome banner and by any view that discovers
// it needs a credential. It used to be a `window.prompt` in one place
// and a re-implementation in two views.
async function askForToken() {
  const entered = await promptModal({
    title: "API token",
    body: "This engine is configured to require a token for the surface you are opening. It is stored in this browser only.",
    label: "Bearer token",
    secret: true,
  });
  if (!entered) return false;
  storeToken(entered);
  socket.setToken(entered);
  store.setAuthRejected(false);
  socket.reconnect();
  renderView();
  return true;
}

// ---- global delegation ----
function initDelegation() {
  // One document-level click delegate. App-level navigation is handled
  // here; anything else is forwarded to the active view, so views never
  // attach (and leak) their own listeners on the reused #view element.
  // Dismissal is its OWN document listener, not a branch in the
  // delegate: the delegate only fires when the click landed inside
  // something carrying `data-action`, so a popover that closed from
  // there would stay open for every click on ordinary page content —
  // which is most of the page.
  document.addEventListener("click", (ev) => {
    if (!healthOpen) return;
    if (ev.target.closest("#health-pop, #live-dot-btn")) return;
    toggleHealth(false);
  });

  document.addEventListener("keydown", (ev) => {
    if (paletteKey(ev)) {
      ev.preventDefault();
      return;
    }
    // The palette's own shortcut. Checked after its key handling so the
    // combination closes it again while it is open.
    if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k") {
      ev.preventDefault();
      togglePalette();
      return;
    }
    if (ev.key === "Escape" && healthOpen) toggleHealth(false);
    // A bare "/" opens search the way it does in every other tool, but
    // only when the reader is not already typing into something.
    if (ev.key === "/" && !isTyping(ev.target) && !paletteOpen) {
      ev.preventDefault();
      togglePalette(true);
    }
  });

  // Typing, forwarded the same way clicks are. A view holding an editor
  // has to keep the text in its own state — the next patch renders from
  // state, and a value living only in the DOM would be reverted by it —
  // so it needs to hear every keystroke.
  document.addEventListener("input", (ev) => {
    const target = ev.target;
    if (!target) return;
    if (target.id === "palette-input") {
      paletteQuery = target.value;
      paletteIndex = 0;
      renderPalette();
      return;
    }
    const action = target.dataset && target.dataset.action;
    if (action && active && active.onAction) active.onAction(action, target, ev);
  });

  // Rows are links, so they answer to the keyboard like links.
  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    const el = ev.target.closest?.("[data-action][tabindex]");
    if (!el) return;
    ev.preventDefault();
    el.click();
  });

  delegate(document.body, "click", (action, el, ev) => {
    if (action === "nav") navigate("/" + el.dataset.nav);
    else if (action === "health") toggleHealth();
    else if (action === "palette") togglePalette(true);
    else if (action === "palette-dismiss") {
      if (ev.target.classList.contains("modal-veil")) togglePalette(false);
    } else if (action === "palette-go") {
      togglePalette(false);
      navigate(el.dataset.route);
    } else if (action === "reconnect") {
      socket.reconnect();
      toggleHealth(false);
    } else if (action === "set-token-chrome") {
      // Its own action name, not the views' `set-token`: this delegate
      // runs before the branch that forwards to the active view, so
      // sharing the name would swallow the Configuration button and
      // leave that view un-reloaded after a token was set.
      askForToken();
    } else if (action === "seat") {
      navigate("/seats/" + encodeURIComponent(el.dataset.seat));
    } else if (action === "go") {
      // A row that names its own destination, so a new panel does not
      // need a new app-level action every time.
      if (healthOpen) toggleHealth(false);
      navigate(el.dataset.route);
    } else if (active && active.onAction) active.onAction(action, el, ev);
  });

  $("#menu-btn").addEventListener("click", () =>
    $("#app").classList.toggle("nav-open"),
  );
}

function isTyping(target) {
  if (!target || !target.tagName) return false;
  const tag = target.tagName.toLowerCase();
  return tag === "input" || tag === "textarea" || target.isContentEditable;
}

// ---- boot ----
function boot() {
  initTheme();
  initDelegation();

  // The socket can tell a refused credential from an unreachable engine;
  // only the shell can draw a dialog. It fires once — a 30-second
  // reconnect backoff must not reopen one forever — and the banner
  // carries the state afterwards.
  socket.onAuthRejected(() => askForToken());

  store.subscribe(["health"], renderChrome);
  store.subscribe(["events"], renderNav);
  store.subscribe(["events"], renderChrome);
  // The attention badges read the slices the obligations come from, so a
  // question answered in chat clears its badge without a refresh.
  store.subscribe(["sandboxes", "agents", "budget", "health"], renderNav);
  store.subscribe(["sandboxes", "agents", "budget", "health"], () => {
    if (activeRoute) setTitle(activeRoute);
  });

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
