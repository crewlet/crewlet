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
  tokenStats,
} from "../llm.js";

const A2A = new Set([
  "a2a_channel_opened",
  "a2a_message_sent",
  "a2a_message_delivered",
  "a2a_channel_closed",
]);

function kv(rows) {
  const body = rows
    .filter(([, v]) => v !== undefined && v !== null && v !== "")
    .map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`)
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

export function createEventDetailView({ api, navigate, params }) {
  let root;
  let raw = null;

  function renderLLM(p, ev) {
    const rec = {
      model: p.model || "",
      phase: p.phase || "turn",
      notes: p.notes || "",
      prompt_messages: p.prompt_messages,
      system_prompt: p.system_prompt,
      user_prompt: p.user_prompt,
      prompt: p.prompt,
      response: p.response,
      input_tokens: p.input_tokens,
      output_tokens: p.output_tokens,
      total_tokens: p.total_tokens,
      tool_executions: p.tool_executions || [],
      tools_available: p.tools_available || [],
      tool_catalogue: p.tool_catalogue || [],
    };
    return `
      ${tokenStats(rec)}
      ${toolsAvailable(rec)}
      <div class="block-label">Prompt</div>${promptSections(rec)}
      <div class="block-label">Response</div>${responseBody(rec.response, rec.tool_executions, {
        codingAgent: isCodingAgentPhase(rec),
        model: rec.model,
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
        ["Summary", esc(f.summary || "")],
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
        ["PR", pr ? `#${pr.number} ${esc(pr.title || "")}` : ""],
        ["Issue", issue ? `#${issue.number} ${esc(issue.title || "")}` : ""],
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
          ["Recipient", esc(p.handle || "")],
          ["Reason", esc(p.reason || "")],
        ])
      );
    }
    if (ev.type === "notifications_coalesced") {
      return (
        head +
        kv([
          ["Agent", esc(p.agent_handle || "")],
          ["Conversation", esc(p.conversation_key || "")],
          ["Messages", p.count],
          ["First", p.first_at ? esc(fmtDateTime(p.first_at)) : ""],
          ["Last", p.last_at ? esc(fmtDateTime(p.last_at)) : ""],
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
        ["Sender", esc(p.sender || "")],
        ["Subject", esc(p.subject || "")],
        ["Recipient", esc(p.recipient_email || "")],
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
    raw = ev;
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

    root.innerHTML = `
      <div class="back-link" data-action="back">${icon("chevron", "sm")} Back to activity</div>
      <div class="card" style="padding:18px">
        <div style="display:flex;align-items:center;gap:10px;margin-bottom:6px">
          <span class="badge info">${esc(title)}</span>
          <span class="row-ts">${esc(fmtDateTime(ev.timestamp))}</span>
          <span style="flex:1"></span>
          <button class="btn sm" data-action="copy">${icon("copy", "sm")} Copy</button>
        </div>
        ${kv([
          ["Type", esc(ev.type)],
          ["Source", esc(ev.source || "")],
          ["Summary", esc(ev.summary || "")],
          ["Trace", ev.trace_id ? `<span class="mono">${esc(shortId(ev.trace_id, 16))}</span>` : ""],
        ])}
        <div style="margin-top:12px">${body}</div>
      </div>`;
  }

  async function load(id) {
    const ev = await api.event(id);
    if (!ev || ev._error) {
      root.innerHTML = `<div class="back-link" data-action="back">${icon("chevron", "sm")} Back</div>${empty("inbox", "Event not found")}`;
      return;
    }
    render(ev);
  }

  return {
    mount(el) {
      root = el;
      root.innerHTML = '<div class="skel skel-row" style="margin:24px 0"></div>';
      load(params.id);
    },
    onAction(action, target) {
      if (action === "back") navigate("/events");
      else if (action === "copy")
        copyToClipboard(JSON.stringify(raw || {}, null, 2)).then(() => toast("Copied"));
      else if (action === "toggle-prompt")
        target.closest(".msg-block")?.classList.toggle("open");
      else if (action === "toggle-think")
        target.closest(".think")?.classList.toggle("open");
      else if (action === "toggle-tool") {
        const k = target.dataset.tkey;
        const sel = window.CSS && CSS.escape ? CSS.escape(k) : k;
        root.querySelector(`[data-tool-key="${sel}"]`)?.classList.toggle("open");
      }
    },
  };
}
