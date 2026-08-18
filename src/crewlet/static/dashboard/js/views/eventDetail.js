// Universal event detail — fetches /events/{id} and renders A2A,
// webhook, LLM, or generic detail based on the event type.

import { esc, fmtDateTime, prettyJson, relTime, shortId } from "../format.js";
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
    const metaKeys = Object.keys(meta);

    let out =
      head +
      kv([
        ["Sender", p.sender || ""],
        ["Subject", p.subject || ""],
        ["Recipient", p.recipient_email || ""],
      ]);
    if (msg)
      out += `<div class="block-label">Message</div><pre class="code">${esc(msg)}</pre>`;
    if (messages.length) {
      out +=
        `<div class="block-label">Coalesced messages (${messages.length})</div>` +
        `<div class="notif-msgs">` +
        messages
          .map(
            (cm) => `
          <div class="notif-msg">
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
              ? raw(`<span class="mono">${esc(shortId(ev.trace_id, 16))}</span>`)
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
