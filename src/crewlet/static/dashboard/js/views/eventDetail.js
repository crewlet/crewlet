// Universal event detail — fetches /events/{id} and renders A2A,
// webhook, LLM, or generic detail based on the event type.

import {
  esc,
  escAttr,
  fmtDateTime,
  prettyJson,
  relTime,
  shortId,
} from "../format.js";
import { icon } from "../icons.js";
import { integrationFromSource, integrationMeta } from "../state.js";
import { empty } from "../ui.js";
import { copyToClipboard, toast } from "../dom.js";
import {
  isCodingAgentPhase,
  toolsAvailable,
  promptSections,
  responseBody,
  failureBlock,
  failureOf,
  tokenStats,
} from "../llm.js";
import { recordFromEvent, recordFromPhase } from "../records.js";

const A2A = new Set([
  "a2a_channel_opened",
  "a2a_message_sent",
  "a2a_message_delivered",
  "a2a_channel_closed",
]);

/**
 * Mark a value as markup that `kv` must not escape.
 *
 * Everything else `kv` renders is escaped. It used to interpolate every
 * value raw and rely on each caller to remember `esc` — and most of
 * these values are webhook payload fields (a Jira summary, a Slack
 * channel name, a GitHub PR title), i.e. text an outsider can choose. A
 * caller that forgot was a stored-XSS hole, and the default has to be
 * the safe one.
 */
function raw(html) {
  return { __html: html };
}

function kv(rows) {
  const body = rows
    .filter(([, v]) => v !== undefined && v !== null && v !== "")
    .map(([k, v]) => {
      const value = v && typeof v === "object" && "__html" in v ? v.__html : esc(v);
      return `<dt>${esc(k)}</dt><dd>${value}</dd>`;
    })
    .join("");
  return `<dl class="kv">${body}</dl>`;
}

/**
 * Render a payload-supplied URL as a link, or as plain text.
 *
 * The scheme is checked rather than trusted: these URLs come out of a
 * webhook body, and a `javascript:` value would otherwise be one click
 * away from running script in the operator's dashboard session. Only
 * http(s) becomes an anchor; anything else renders as escaped text.
 *
 * `base.css` gives `a` `color: inherit`, so an anchor with no class is
 * indistinguishable from the text around it — `trace-link` is the
 * dashboard's link treatment and the only one defined.
 */
function linkTo(url) {
  const href = String(url || "");
  if (!href) return "";
  if (!/^https?:\/\//i.test(href)) return href;
  return raw(
    `<a class="trace-link" href="${escAttr(href)}" target="_blank" rel="noopener noreferrer">${esc(
      href,
    )} ↗</a>`,
  );
}

function firstLine(s) {
  return String(s == null ? "" : s).split("\n")[0].trim();
}

// GitLab user arrays (`assignees`, `reviewers`, and both sides of a
// `changes` diff) hold user objects; the router keys on `username`.
function glNames(users) {
  return (Array.isArray(users) ? users : [])
    .map((u) => (u && (u.username || u.name)) || "")
    .filter(Boolean)
    .join(", ");
}

/**
 * The `previous → current` transition for a `changes` entry.
 *
 * `changes.{assignees,reviewers}` is the diff `parse_gitlab_webhook`
 * routes on — a *newly added* assignee is the target, someone removed is
 * not. Rendering only the resulting list would hide exactly that: "who
 * is on it now" never says who was just put there. Falls back to
 * `current` when the event carries no diff (an `open`, not an `update`).
 */
function glTransition(changes, key, current) {
  const entry = changes[key];
  if (!entry || typeof entry !== "object") return current;
  const before = glNames(entry.previous);
  const after = glNames(entry.current);
  if (!before && !after) return current;
  return `${before || "—"} → ${after || "—"}`;
}

function glFailedJobs(builds) {
  return (Array.isArray(builds) ? builds : [])
    .filter((b) => b && b.status === "failed")
    .map((b) => (b.stage ? `${b.stage}/${b.name || ""}` : b.name || ""))
    .filter(Boolean)
    .join(", ");
}

// Plane's CE serializers hand an FK back as a UUID string OR an expanded
// `{id: …}` dict, and disagree on `project` vs `project_id` — the
// coercions in `notifications/transports/plane.py` mirrored, because a
// reader of this screen has to see the same value the router did.
function planeRef(value) {
  if (value && typeof value === "object") value = value.id;
  return value == null ? "" : String(value).toLowerCase();
}

function planeParties(value) {
  return (Array.isArray(value) ? value : [])
    .map((v) =>
      v && typeof v === "object" ? v.display_name || v.first_name || v.id : v,
    )
    .map((v) => (v == null ? "" : String(v)))
    .filter(Boolean)
    .join(", ");
}

const PLANE_MENTION_TAG = /<mention-component[^>]*>(?:<\/mention-component>)?/gi;
const PLANE_MENTION_ID = /entity_identifier="([^"]*)"/i;

// The mentioned user UUIDs a comment carries. The transport routes a
// Plane comment to exactly these ids, so they are the payload's answer
// to "why did this comment wake that agent".
function planeMentionIds(html) {
  const out = [];
  const seen = new Set();
  for (const tag of String(html || "").match(PLANE_MENTION_TAG) || []) {
    const found = tag.match(PLANE_MENTION_ID);
    const id = found ? (found[1] || "").toLowerCase() : "";
    if (id && !seen.has(id)) {
      seen.add(id);
      out.push(id);
    }
  }
  return out;
}

const HTML_ENTITIES = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
};

function unentity(s) {
  return s.replace(/&(#\d+|#[xX][0-9a-fA-F]+|[a-zA-Z]+);/g, (whole, ref) => {
    if (ref[0] !== "#") {
      const v = HTML_ENTITIES[ref.toLowerCase()];
      return v === undefined ? whole : v;
    }
    const cp =
      ref[1] === "x" || ref[1] === "X"
        ? parseInt(ref.slice(2), 16)
        : parseInt(ref.slice(1), 10);
    if (!Number.isInteger(cp) || cp < 1 || cp > 0x10ffff) return whole;
    return String.fromCodePoint(cp);
  });
}

/**
 * Flatten Plane's `comment_html` / `description_html` to readable text.
 *
 * Mirrors `_strip_html` in `notifications/transports/plane.py`: mention
 * components collapse to `@{uuid}`, every other tag becomes whitespace,
 * and only then are entities decoded. That order is not cosmetic —
 * decoding first turns an entity-encoded `&lt;` into a bracket the tag
 * strip then reads as markup, swallowing everything up to the next `>`.
 * Decoding at all is safe because the result is handed to `esc` before
 * it reaches the DOM, so a decoded `<script>` is re-escaped on the way
 * out.
 */
function planeText(html) {
  const flattened = String(html || "")
    .replace(PLANE_MENTION_TAG, (tag) => {
      const found = tag.match(PLANE_MENTION_ID);
      return found && found[1] ? `@${found[1]}` : " ";
    })
    .replace(/<[^>]+>/g, " ");
  return unentity(flattened).replace(/\s+/g, " ").trim();
}

// Flatten Atlassian Document Format → plain text.
function adfText(node) {
  if (!node) return "";
  if (typeof node === "string") return node;
  if (Array.isArray(node)) return node.map(adfText).join("");
  let out = "";
  if (node.type === "text") out += node.text || "";
  else if (node.type === "mention") out += "@" + (node.attrs?.text || "");
  else if (node.type === "emoji") out += node.attrs?.shortName || "";
  else if (node.type === "hardBreak") out += "\n";
  if (node.content) out += adfText(node.content);
  if (["paragraph", "heading", "listItem"].includes(node.type)) out += "\n";
  return out;
}

export function createEventDetailView({ query, navigate, refresh, params }) {
  // NOT named `raw`: that is the module-level marker `kv` reads to skip
  // escaping, and a closure variable of the same name shadows it for
  // every function nested here — which turned the one `raw(...)` call in
  // `render` into a call on the fetched event object, and crashed the
  // whole screen with "raw is not a function".
  let loadedEvent = null;
  let loading = true;
  let loadError = "";
  const openPrompts = new Set();
  const collapsedThinks = new Set();
  const openTools = new Set();

  function renderLLM(p, ev) {
    // The record comes from the shared normalizer rather than a field
    // list written out here. This view used to keep its own copy of that
    // list, which is how a failed phase reached this screen with its
    // failure quietly stripped off.
    const rec = recordFromEvent(ev) || recordFromPhase(p, ev.timestamp);
    const failure = failureOf(rec);
    return `
      ${failure ? failureBlock(failure) : ""}
      ${tokenStats(rec)}
      ${toolsAvailable(rec)}
      <div class="block-label">Prompt</div>${promptSections(rec, {
        keyPrefix: "ev",
        isOpen: (k, long) => (openPrompts.has(k) ? true : !long),
      })}
      <div class="block-label">Response</div>${responseBody(rec.response, rec.tool_executions, {
        keyPrefix: "ev",
        thinkOpen: (k) => !collapsedThinks.has(k),
        toolOpen: (k) => openTools.has(k),
        codingAgent: isCodingAgentPhase(rec),
        model: rec.model,
        failure,
        inProgress: false,
      })}`;
  }

  function renderA2A(p, ev) {
    const rows = [
      ["Channel", p.channel_id],
      ["Source", ev.source],
      ["Requester", p.requester],
      ["Target", p.target],
      ["Sender", p.sender],
      ["Recipient", p.recipient],
      ["Messages", p.message_count],
      ["Closed by", p.closed_by],
      ["Duration", p.duration_ms != null ? (p.duration_ms / 1000).toFixed(1) + "s" : ""],
    ];
    let out = kv(rows);
    if (p.content)
      out += `<div class="block-label">Message</div><pre class="code">${esc(p.content)}</pre>`;
    return out;
  }

  function renderWebhook(p, ev) {
    const body = p.body && typeof p.body === "object" ? p.body : p;
    const source = ev.source || "";
    let specific = "";
    if (source === "jira") {
      const issue = body.issue || {};
      const f = issue.fields || {};
      const comment = body.comment;
      specific = kv([
        ["Event", body.webhookEvent],
        ["Issue", issue.key],
        ["Summary", f.summary || ""],
        ["Status", f.status?.name],
        ["Assignee", f.assignee?.displayName],
      ]);
      if (comment)
        specific += `<div class="block-label">Comment</div><pre class="code">${esc(adfText(comment.body))}</pre>`;
    } else if (source === "confluence") {
      const page = body.page || body.content || {};
      const comment = body.comment;
      specific = kv([
        ["Event", body.event || body.webhookEvent],
        ["Page", page.title],
        ["Space", page.space?.key],
      ]);
      if (comment)
        specific += `<div class="block-label">Comment</div><pre class="code">${esc(adfText(comment.body?.value ?? comment.body))}</pre>`;
    } else if (source === "slack") {
      const e = body.event || {};
      specific = kv([
        ["Event", e.type || body.type],
        ["Channel", e.channel],
        ["User", e.user],
      ]);
      if (e.text)
        specific += `<div class="block-label">Message</div><pre class="code">${esc(e.text)}</pre>`;
    } else if (source === "mattermost") {
      // Mattermost events arrive from the websocket fleet, not a webhook,
      // so the post is nested rather than at the envelope root.
      const post = body.post || {};
      specific = kv([
        ["Event", body.event],
        ["Channel", body.channel_name || post.channel_id],
        ["Type", body.channel_type],
        ["User", body.sender_name || post.user_id],
        ["Thread", post.root_id],
        ["Replayed", body.replayed ? "yes (reconnect backfill)" : ""],
      ]);
      if (post.message)
        specific += `<div class="block-label">Message</div><pre class="code">${esc(post.message)}</pre>`;
    } else if (source === "github") {
      const pr = body.pull_request;
      const issue = body.issue;
      specific = kv([
        ["Action", body.action],
        ["Sender", body.sender?.login],
        ["Repo", body.repository?.full_name],
        ["PR", pr ? `#${pr.number} ${pr.title || ""}` : ""],
        ["Issue", issue ? `#${issue.number} ${issue.title || ""}` : ""],
      ]);
    } else if (source === "gitlab") {
      const kind = body.object_kind || "";
      const attrs = body.object_attributes || {};
      const action = attrs.action || "";
      const changes =
        body.changes && typeof body.changes === "object" ? body.changes : {};
      // A push hook carries neither `object_attributes` nor a `user`
      // object — its actor is flattened onto the envelope instead.
      const actor =
        body.user?.username ||
        body.user?.name ||
        body.user_username ||
        body.user_name ||
        "";
      // On an `issue` / `merge_request` hook the object IS
      // `object_attributes`; on a `note` or `pipeline` hook the thing the
      // event hangs off is a sibling key.
      const mr = kind === "merge_request" ? attrs : body.merge_request || {};
      const issue = kind === "issue" ? attrs : body.issue || {};
      const status = kind === "pipeline" ? attrs.status || "" : "";
      const commits = Array.isArray(body.commits) ? body.commits : [];
      specific = kv([
        ["Event", action ? `${kind}.${action}` : kind],
        ["Actor", actor],
        ["Project", body.project?.path_with_namespace],
        ["Merge request", mr.iid ? `!${mr.iid} ${mr.title || ""}`.trim() : ""],
        ["Issue", issue.iid ? `#${issue.iid} ${issue.title || ""}`.trim() : ""],
        ["State", kind === "issue" || kind === "merge_request" ? attrs.state : ""],
        [
          "Branches",
          kind === "merge_request" && attrs.source_branch
            ? `${attrs.source_branch} → ${attrs.target_branch || ""}`.trim()
            : "",
        ],
        [
          "Pipeline",
          kind === "pipeline" ? `#${attrs.id || ""} on ${attrs.ref || ""}`.trim() : "",
        ],
        // `pipeline.failed` is the one GitLab event routed back to the
        // actor rather than to the thread — the agent whose push broke
        // the build owns the fix — so it must not read like any other
        // status line.
        [
          "Status",
          status === "failed"
            ? raw('<span class="badge failed">failed</span>')
            : status,
        ],
        ["Failed jobs", status === "failed" ? glFailedJobs(body.builds) : ""],
        [
          "Duration",
          kind === "pipeline" && attrs.duration != null ? `${attrs.duration}s` : "",
        ],
        [
          "Branch",
          kind === "push" ? String(body.ref || "").replace(/^refs\/heads\//, "") : "",
        ],
        [
          "Commits",
          kind === "push" ? String(body.total_commits_count ?? commits.length) : "",
        ],
        ["Assignees", glTransition(changes, "assignees", glNames(body.assignees))],
        ["Reviewers", glTransition(changes, "reviewers", glNames(body.reviewers))],
        ["Changed", Object.keys(changes).join(", ")],
        ["Link", linkTo(attrs.url || mr.url || issue.url || body.project?.web_url)],
      ]);
      const note = kind === "note" ? attrs.note || "" : "";
      const description =
        kind === "issue" || kind === "merge_request" ? attrs.description || "" : "";
      if (note)
        specific += `<div class="block-label">Comment</div><pre class="code">${esc(note)}</pre>`;
      else if (description)
        specific += `<div class="block-label">Description</div><pre class="code">${esc(description)}</pre>`;
      if (kind === "push" && commits.length)
        specific += `<div class="block-label">Commits</div><pre class="code">${esc(
          commits
            .map(
              (c) =>
                `${String((c && c.id) || "").slice(0, 8)} ${firstLine(
                  c && (c.title || c.message),
                )}`,
            )
            .join("\n"),
        )}</pre>`;
    } else if (source === "plane") {
      const event = body.event || "";
      const action = body.action || "";
      const data = body.data && typeof body.data === "object" ? body.data : {};
      const activity =
        body.activity && typeof body.activity === "object" ? body.activity : {};
      const actorRef = activity.actor;
      const actor =
        (actorRef && typeof actorRef === "object"
          ? actorRef.display_name || actorRef.first_name || actorRef.id
          : actorRef) || "";
      const isPage = event === "page";
      // A page is workspace-scoped with a `projects` M2M, so its project
      // ref is a list rather than the scalar FK a work item carries —
      // `_page_project_id` in the transport draws the same distinction.
      const projectRef = isPage
        ? data.project_id ||
          data.project ||
          (Array.isArray(data.projects) ? data.projects[0] : "") ||
          (Array.isArray(data.project_ids) ? data.project_ids[0] : "")
        : data.project_id || data.project;
      const identifier =
        data.project && typeof data.project === "object"
          ? data.project.identifier || ""
          : "";
      const seq = data.sequence_id;
      // ``ENG-42`` only when the payload expanded the project FK — the
      // id→identifier cache that resolves the rest lives in the engine,
      // and a bare sequence number is still worth more than nothing.
      const workItemKey =
        seq === undefined || seq === null || seq === ""
          ? ""
          : identifier
            ? `${identifier}-${seq}`
            : `#${seq}`;
      // `data.id` is the work item on an `issue` event but the COMMENT /
      // INTAKE row on the others — the work item is `data.issue` there,
      // the same distinction the transport draws before it builds a URL.
      const workItemId = planeRef(event === "issue" ? data.id : data.issue);
      const oldValue = activity.old_value == null ? "" : String(activity.old_value);
      const newValue = activity.new_value == null ? "" : String(activity.new_value);
      const mentions =
        event === "issue_comment" ? planeMentionIds(data.comment_html) : [];
      specific = kv([
        ["Event", action ? `${event}.${action}` : event],
        ["Actor", actor],
        ["Workspace", body.workspace_slug],
        ["Project", identifier || planeRef(projectRef)],
        [
          "Work item",
          isPage ? "" : [workItemKey, data.name || ""].filter(Boolean).join(" "),
        ],
        ["Work item id", isPage ? "" : workItemId],
        ["Page", isPage ? data.name || "" : ""],
        ["Page id", isPage ? planeRef(data.id) : ""],
        // `activity.field` is the discriminator the transport routes an
        // update on: an `assignees` edit is a directed ping at the newly
        // added assignee, anything else fans out to the work item's
        // subscribers. Without it an `issue.updated` row says nothing
        // about what actually moved.
        ["Field", activity.field],
        ["Change", oldValue || newValue ? `${oldValue || "—"} → ${newValue || "—"}` : ""],
        ["Assignees", planeParties(data.assignees)],
        ["Mentions", mentions.join(", ")],
      ]);
      const commentText = planeText(data.comment_html);
      const descriptionText = planeText(data.description_html);
      if (commentText)
        specific += `<div class="block-label">Comment</div><pre class="code">${esc(commentText)}</pre>`;
      else if (descriptionText)
        specific += `<div class="block-label">Description</div><pre class="code">${esc(descriptionText)}</pre>`;
    }
    return `
      ${specific}
      <div class="block-label">Raw payload</div>
      <pre class="code" style="max-height:340px">${esc(prettyJson(p))}</pre>`;
  }

  // Inbound notification (and its skip / coalesce telemetry siblings):
  // lead with a branded integration badge so the source is unmistakable,
  // then lay the message out for reading instead of dumping raw JSON.
  function renderNotification(p, ev) {
    const key = p.notification_source || integrationFromSource(ev.source);
    const m = integrationMeta(key);
    const head = `
      <div class="notif-head">
        <span class="int-badge lg" style="--int-color:${m.color}">${icon(m.icon, "sm")}${esc(m.label)}</span>
        ${p.source_event_type ? `<span class="chip">${esc(p.source_event_type)}</span>` : ""}
      </div>`;

    if (ev.type === "notification_skipped") {
      return (
        head +
        kv([
          ["Recipient", p.handle || ""],
          ["Reason", p.reason || ""],
        ])
      );
    }
    if (ev.type === "notifications_coalesced") {
      return (
        head +
        kv([
          ["Agent", p.agent_handle || ""],
          ["Conversation", p.conversation_key || ""],
          ["Messages", p.count],
          ["First", p.first_at ? fmtDateTime(p.first_at) : ""],
          ["Last", p.last_at ? fmtDateTime(p.last_at) : ""],
        ])
      );
    }

    // external_notification: the readable inbound message is
    // ``salient_body`` (raw, no planner scaffolding); fall back to the
    // enriched ``body`` only when no distinct salient body was emitted.
    const msg =
      p.salient_body != null && p.salient_body !== "" ? p.salient_body : p.body;
    const messages = Array.isArray(p.messages) ? p.messages : [];
    const meta = p.metadata && typeof p.metadata === "object" ? p.metadata : {};
    // `routed_via` is promoted out of the chip row below rather than
    // shown twice: it is the transport's answer to "why did this wake
    // THIS agent" (`assignee`, `mention`, `subscriber`,
    // `project_lead_fallback`, `intake_triage`, …), and as one chip among
    // a dozen coordinates it read as another id.
    const metaKeys = Object.keys(meta).filter((k) => k !== "routed_via");

    let out =
      head +
      kv([
        ["Sender", p.sender || ""],
        ["Subject", p.subject || ""],
        ["Recipient", p.recipient_email || ""],
        ["Routed via", meta.routed_via || ""],
      ]);
    if (msg)
      out += `<div class="block-label">Message</div><pre class="code">${esc(msg)}</pre>`;
    if (messages.length) {
      out +=
        `<div class="block-label">Coalesced messages (${messages.length})</div>` +
        `<div class="notif-msgs">` +
        messages
          .map(
            (cm, i) => `
          <div class="notif-msg" data-k="msg:${i}">
            <div class="notif-msg-meta">
              ${cm.sender ? `<b>${esc(cm.sender)}</b>` : ""}
              ${cm.source_event_type ? `<span class="chip">${esc(cm.source_event_type)}</span>` : ""}
              ${cm.timestamp ? `<span class="row-ts">${esc(relTime(cm.timestamp))}</span>` : ""}
            </div>
            ${cm.body ? `<pre class="code">${esc(cm.body)}</pre>` : ""}
          </div>`,
          )
          .join("") +
        `</div>`;
    }
    if (metaKeys.length)
      out +=
        `<div class="block-label">Metadata</div><div class="chip-row">` +
        metaKeys
          .map((k) => `<span class="chip">${esc(k)}: ${esc(String(meta[k]))}</span>`)
          .join("") +
        `</div>`;
    return out;
  }

  function renderGeneric(p) {
    return `<div class="block-label">Payload</div><pre class="code" style="max-height:420px">${esc(prettyJson(p))}</pre>`;
  }

  function render(ev) {
    const p = ev.payload || {};
    let title = ev.type;
    let body;
    if (ev.type === "agent_turn_completed" || ev.type === "agent_phase_completed") {
      title = "LLM Call";
      body = renderLLM(p, ev);
    } else if (A2A.has(ev.type)) {
      title = "A2A Event";
      body = renderA2A(p, ev);
    } else if (ev.category === "notification") {
      title = "Notification";
      body = renderNotification(p, ev);
    } else if (ev.category === "webhook" || /^(webhook|forge):/.test(ev.type)) {
      title = "Webhook";
      body = renderWebhook(p, ev);
    } else {
      body = renderGeneric(p);
    }

    return `
      <div class="back-link" data-action="back">${icon("chevron", "sm")} Back to activity</div>
      <div class="card event-detail">
        <div style="display:flex;align-items:center;gap:10px;margin-bottom:6px">
          <span class="badge info">${esc(title)}</span>
          <span class="row-ts">${esc(fmtDateTime(ev.timestamp))}</span>
          <span style="flex:1"></span>
          <button class="btn sm" data-action="copy">${icon("copy", "sm")} Copy</button>
        </div>
        ${kv([
          ["Type", ev.type],
          ["Source", ev.source || ""],
          ["Summary", ev.summary || ""],
          [
            "Trace",
            ev.trace_id
              ? raw(
                  `<span class="mono trace-link" data-action="open-trace" data-trace="${escAttr(
                    ev.trace_id,
                  )}">${esc(shortId(ev.trace_id, 16))} →</span>`,
                )
              : "",
          ],
        ])}
        <div style="margin-top:12px">${body}</div>
      </div>`;
  }

  async function load(id) {
    try {
      loadedEvent = await query("event", { id });
    } catch (err) {
      loadError = err.message;
    }
    loading = false;
    refresh();
  }

  return {
    slices: [],

    mount() {
      load(params.id);
    },

    render() {
      const back = `<div class="back-link" data-action="back">${icon("chevron", "sm")} Back</div>`;
      if (loading) return back + '<div class="skel skel-row" style="margin:24px 0"></div>';
      if (loadError || !loadedEvent) {
        return (
          back +
          empty(
            "inbox",
            loadError === "no_event_store"
              ? "No event store configured"
              : "Event not found",
            loadError === "offline" ? "Reconnecting…" : "",
          )
        );
      }
      return render(loadedEvent);
    },

    onAction(action, target) {
      if (action === "back") {
        navigate("/events");
        return;
      }
      if (action === "open-trace") {
        navigate("/traces/" + encodeURIComponent(target.dataset.trace));
        return;
      }
      if (action === "copy") {
        copyToClipboard(JSON.stringify(loadedEvent || {}, null, 2)).then(() => toast("Copied"));
        return;
      }
      if (action === "toggle-prompt") {
        const k = target.dataset.pkey;
        if (openPrompts.has(k)) openPrompts.delete(k);
        else openPrompts.add(k);
      } else if (action === "toggle-think") {
        const k = target.dataset.tkey;
        if (collapsedThinks.has(k)) collapsedThinks.delete(k);
        else collapsedThinks.add(k);
      } else if (action === "toggle-tool") {
        const k = target.dataset.tkey;
        if (openTools.has(k)) openTools.delete(k);
        else openTools.add(k);
      } else {
        return;
      }
      refresh();
    },
  };
}
