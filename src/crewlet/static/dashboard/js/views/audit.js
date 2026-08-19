// Audit log: the company config's revision history — who changed what,
// when, the structural diff, and revert.
//
// Auth-gated, and entirely pulled: the revision list and every diff are
// operator-only queries behind the same token the Configuration view
// uses, which the shell attaches to each query frame — this view never
// passes one itself, it only makes sure the socket has the token the
// reader just typed. Nothing here comes from the store, so `slices` is
// empty and a render only ever follows a load or a click.

import {
  esc,
  escAttr,
  fmtDateTime,
  prettyJson,
  shortId,
  trunc,
} from "../format.js";
import { icon } from "../icons.js";
import { empty, sectionHead } from "../ui.js";
import { toast } from "../dom.js";
import { apiToken, promptForToken, tokenGate, tokenRejected } from "../authToken.js";

// How much history to ask for. Matches the default the `config_audit`
// query applies server-side (api/queries.py), and this panel is the
// "what changed lately" list rather than the forensic record — 50
// revisions is already weeks of a config edited several times a day.
const AUDIT_LIMIT = 50;

// Query codes that are about the transport rather than the answer: the
// socket has not opened yet (the shell mounts a view before it connects,
// so a cold load straight onto /audit always starts here) or a reply went
// missing. Both are worth asking again; every other code is a real answer
// the reader has to see.
const RETRIABLE = new Set(["offline", "timeout"]);

// Retry cadence for those. Fast enough that the page fills in the moment
// the socket opens, doubling to a ceiling well under the socket's own 30s
// reconnect cap — against a stopped engine each attempt is a locally
// rejected promise, so this costs nothing but a timer. Same values as the
// Configuration view, so the two auth-gated screens behave identically.
const RETRY_MS = 750;
const RETRY_MAX_MS = 5_000;

// The one loading placeholder, shared by the first load and by a retry
// that is still waiting on the socket.
const SKELETON = '<div class="skel skel-row" style="margin:24px 0"></div>';

// Longest changed-value preview rendered on a diff row. The row is one
// ellipsised line by design (the full value belongs in the Configuration
// view), and past ~200 characters the truncation does all the work anyway.
const DIFF_VALUE_CHARS = 200;

// The marker each structural change leads with; `~` covers replace and
// anything else the differ emits.
const DIFF_SYMBOL = { add: "+", remove: "−" };

export function createAuditView({ query, refresh, setToken }) {
  // The history — `null` until the first successful load. "Not here yet"
  // and "this org has no revisions" are different answers, and the view
  // must not show the second while it means the first.
  let revisions = null;
  let loadError = "";
  let retryTimer = 0;
  let retryDelay = RETRY_MS;
  let disposed = false;

  // Diff results, keyed by revision id: flipping back to a revision that
  // was already opened costs no second query, and the pane renders from
  // local state like everything else rather than being written into.
  //   revision id → { state: "loading" | "ready" | "error", diff }
  const diffs = new Map();
  // Which revision's diff the pane below the list is showing. One pane,
  // last one clicked wins — exactly as before.
  let openDiff = "";

  // ---- data loading ----

  async function load() {
    // No token: `render` shows the gate, and asking would only earn an
    // `unauthorized` the reader cannot act on any differently.
    if (disposed || !apiToken()) return;
    clearTimeout(retryTimer);
    retryTimer = 0;
    try {
      const data = await query("config_audit", { limit: AUDIT_LIMIT });
      if (disposed) return;
      revisions = (data && data.revisions) || [];
      loadError = "";
      retryDelay = RETRY_MS;
    } catch (err) {
      // A reply arriving after the reader has navigated away arms no
      // retry: that poll would outlive the view it was reading for, and
      // nothing would ever clear it.
      if (disposed) return;
      loadError = err.message;
      if (RETRIABLE.has(loadError)) {
        retryTimer = setTimeout(load, retryDelay);
        retryDelay = Math.min(retryDelay * 2, RETRY_MAX_MS);
      }
    }
    refresh();
  }

  async function showDiff(revId) {
    if (!revId) return;
    openDiff = revId;
    const cached = diffs.get(revId);
    // A cached diff is final — a revision's changes never change. A
    // cached *error* is not, so clicking Diff again retries it.
    if (cached && cached.state === "ready") {
      refresh();
      return;
    }
    diffs.set(revId, { state: "loading" });
    refresh();
    try {
      const diff = await query("config_diff", { revision_id: revId });
      if (disposed) return;
      diffs.set(revId, { state: "ready", diff });
    } catch {
      if (disposed) return;
      diffs.set(revId, { state: "error" });
    }
    refresh();
  }

  // The one *write* on this page, and the one thing here that is still an
  // HTTP request: the socket's query channel is read-only by construction
  // (api/queries.py maps `what` → the same read handler the GET route
  // calls), while reverting POSTs a brand-new active revision. It carries
  // the same bearer token the queries do, and moves onto the socket the
  // day that channel grows a command frame.
  async function revert(revId) {
    if (!revId) return;
    if (!window.confirm("Create a new active revision from this one?")) return;
    let ok = false;
    let code = "";
    // The one HTTP request the dashboard still makes, and deliberately:
    // reverting is a WRITE. Reads moved to the socket because two
    // transports disagreed and a re-fetch on every streamed round made
    // the page churn — neither applies to a POST fired by an explicit
    // click. It stays on the authenticated REST route so the revision it
    // creates is attributed to the operator by the same middleware that
    // guards every other write.
    try {
      const res = await fetch(
        `/config/revisions/${encodeURIComponent(revId)}/revert`,
        {
          method: "POST",
          headers: {
            Authorization: "Bearer " + apiToken(),
            "X-Summary": "revert via dashboard",
          },
        },
      );
      ok = res.ok;
      code = String(res.status);
    } catch {
      code = "offline";
    }
    if (disposed) return;
    if (ok) {
      toast("Reverted — new revision active", "ok");
      // The new revision heads the history and is what every other
      // revision is now diffed against, so both halves are stale.
      diffs.clear();
      openDiff = "";
      load();
    } else {
      toast(`Revert failed (${code})`, "err");
    }
  }

  // ---- render helpers (pure) ----

  // One revision, keyed by its id: that is the identity the diff and the
  // revert are addressed by, so the row keeps its node as new revisions
  // land on top of it.
  function revisionRow(r) {
    const id = r.revision_id;
    return `
      <div class="rev" data-k="rev:${escAttr(id)}">
        <span class="rev-mark ${r.is_active ? "active" : ""}">${r.is_active ? "●" : "○"}</span>
        <div class="row-body">
          <div class="row-title" style="font-weight:550">${esc(r.summary || "(no summary)")}</div>
          <div class="row-sub"><span class="mono">${esc(shortId(id, 12))}</span> · ${esc(r.created_by || "")} · ${esc(r.source || "")} · ${esc(fmtDateTime(r.activated_at || r.created_at))}</div>
        </div>
        <button class="btn sm" data-action="diff" data-id="${escAttr(id)}">${icon("git", "sm")} Diff</button>
        ${!r.is_active ? `<button class="btn sm" data-action="revert" data-id="${escAttr(id)}">Revert</button>` : ""}
      </div>`;
  }

  // One structural change, keyed by what it is: a diff names each path
  // once, so op+path identifies the row across renders — including the
  // flip from one revision's diff to another's — without leaning on its
  // position in the list.
  function changeRow(c) {
    const sym = DIFF_SYMBOL[c.op] || "~";
    const val =
      c.value !== undefined
        ? trunc(prettyJson(c.value).replace(/\s+/g, " "), DIFF_VALUE_CHARS)
        : "";
    return `<div class="row" style="padding:7px 14px" data-k="chg:${escAttr(c.op + "|" + c.path)}">
      <span class="diff-op ${escAttr(c.op)}">${sym}</span>
      <span class="diff-path">${esc(c.path)}</span>
      <span class="row-sub" style="margin:0;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(val)}</span>
    </div>`;
  }

  function diffPane() {
    if (!openDiff) return "";
    const entry = diffs.get(openDiff);
    if (!entry || entry.state === "loading") {
      return '<div class="skel skel-row"></div>';
    }
    if (entry.state === "error") {
      return `<div class="banner err">${icon("alert", "sm")}<span>Could not load diff.</span></div>`;
    }
    const d = entry.diff || {};
    const changes = (d.changes || []).map(changeRow).join("");
    return `
      <div class="card">
        <div class="sec" style="margin:12px 16px"><span class="sec-title">Diff
          <span class="sec-count">${esc(shortId(d.from || "", 8))} → ${esc(shortId(d.to || "", 8))}</span></span></div>
        ${changes || '<div class="empty-sub" style="padding:14px">No structural changes</div>'}
      </div>`;
  }

  return {
    // Query-loaded, every last row of it: no push should ever cost this
    // view a render.
    slices: ["health"],

    mount() {
      load();
    },

    render(state) {
      if (!apiToken()) return tokenGate("the audit log");
      if (loadError === "unauthorized") return tokenRejected();
      if (RETRIABLE.has(loadError)) {
        // A retry is already scheduled, so this is a waiting state and
        // not a dead end. While the socket is still coming up that is
        // indistinguishable from the first load — show the skeleton, and
        // only call it a failure once the engine says it is there.
        return state.connected
          ? empty("clipboard", "Could not load the audit log", "Reconnecting…")
          : SKELETON;
      }
      if (loadError) {
        return `
        <div class="banner err">${icon("alert", "sm")}<span>Could not load the audit log (${esc(loadError)}).</span></div>
        ${empty("clipboard", "No revision history")}`;
      }
      if (revisions === null) return SKELETON;

      const history = revisions.map(revisionRow).join("");

      return `
      ${sectionHead("clipboard", "Revision history", revisions.length, { action: "set-token", label: "change token" })}
      ${revisions.length ? `<div class="list">${history}</div>` : empty("clipboard", "No revisions yet")}
      <div style="margin-top:16px">${diffPane()}</div>`;
    },

    onAction(action, target) {
      if (action === "set-token") {
        promptForToken(() => {
          // The bearer rides on the query frame, so the socket has to
          // learn the new token before the reload goes out — otherwise
          // the one just typed unlocks nothing until a page reload.
          if (setToken) setToken(apiToken());
          revisions = null;
          loadError = "";
          retryDelay = RETRY_MS;
          diffs.clear();
          openDiff = "";
          load();
          refresh();
        });
      } else if (action === "diff") {
        showDiff(target.dataset.id);
      } else if (action === "revert") {
        revert(target.dataset.id);
      }
    },

    destroy() {
      // An in-flight query cannot be cancelled, so the flag is what keeps
      // its reply from refreshing a view nobody is looking at — and what
      // stops a rejection that lands after teardown from arming a retry
      // loop that outlives the page.
      disposed = true;
      clearTimeout(retryTimer);
      retryTimer = 0;
    },
  };
}
