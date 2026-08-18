// Shared rendering for an LLM call: prompt, and a response that
// interleaves reasoning (<think>) and tool calls inline, in the order
// they happened — tool calls render as clickable numbered badges within
// the response flow, each expanding to its arguments + result.
//
// Open/closed state is supplied by the caller via predicates so a
// re-render (or live streaming) preserves what the user expanded.

import { esc, escAttr, mdBlock, prettyJson, relTime, trunc } from "./format.js";
import { icon } from "./icons.js";
import { integrationMeta } from "./state.js";

// The two coding-agent runners (a sandbox Execute phase publishes its findings
// under one of these as ``model``). Kept in sync with
// ``RoleSandboxConfig.coding_agent`` on the backend.
const CODING_AGENTS = new Set(["opencode", "claude-code"]);

// Whether a phase record is a coding-agent's findings report (the sandbox
// Execute output) — rendered specially (markdown findings + a collapsible
// activity log) rather than as a plain LLM response. The model IS the coding
// agent for that one phase; ``notes`` is a belt-and-braces fallback.
export function isCodingAgentPhase(rec) {
  return (
    CODING_AGENTS.has(rec?.model) || String(rec?.notes || "").startsWith("backend=sandbox")
  );
}

// Section delimiters the engine writes into a coding-agent phase's response
// (``execute_sandbox._sandbox_phase_response``). Matched verbatim here.
const TRANSCRIPT_MARK = "--- Coding-agent transcript ---";
const ERROR_MARK = "--- Error ---";

// Split a coding-agent phase response into its { findings, transcript, error }
// sections. The engine concatenates them with the markers above; we slice on
// the markers (tolerant of where each lands) so each renders in its own block.
function splitCodingSections(response) {
  const cut = (s, mark) => {
    const idx = s.indexOf(mark);
    if (idx < 0) return [s, null];
    return [s.slice(0, idx).replace(/\n+$/, ""), s.slice(idx + mark.length).replace(/^\n+/, "")];
  };
  let [findings, rest] = cut(response || "", TRANSCRIPT_MARK);
  let transcript = "";
  let error = "";
  if (rest != null) {
    [transcript, error] = cut(rest, ERROR_MARK).map((x) => x || "");
  } else {
    [findings, error] = cut(findings, ERROR_MARK).map((x) => x ?? "");
  }
  return { findings, transcript: transcript || "", error: error || "" };
}

// Render the coding agent's activity transcript as a collapsed-by-default
// <details> (zero-JS toggle): each ``[tool] name: detail → error: msg`` line
// becomes a tidy step row; free-text lines (the agent's streamed prose) render
// as-is. ``[tool]`` lines drive the step count.
function codingActivity(transcript) {
  const lines = transcript.split("\n").filter((l) => l.trim());
  if (!lines.length) return "";
  let steps = 0;
  const rows = lines
    .map((l) => {
      const m = l.match(/^\[tool\]\s+(.*)$/);
      if (!m) return `<div class="ca-text">${esc(l)}</div>`;
      steps++;
      let body = m[1];
      let err = "";
      const ei = body.indexOf(" → error: ");
      if (ei >= 0) {
        err = body.slice(ei + " → error: ".length);
        body = body.slice(0, ei);
      }
      const ci = body.indexOf(": ");
      const name = ci >= 0 ? body.slice(0, ci) : body;
      const detail = ci >= 0 ? body.slice(ci + 2) : "";
      return `<div class="ca-step${err ? " err" : ""}">
        <span class="ca-tool">${esc(name)}</span>
        ${detail ? `<code class="ca-cmd">${esc(detail)}</code>` : ""}
        ${err ? `<span class="ca-err">${esc(err)}</span>` : ""}
      </div>`;
    })
    .join("");
  return `<details class="ca-activity">
    <summary>${icon("code", "sm")}<span>Coding-agent activity</span><span class="ca-count">${steps} step${steps === 1 ? "" : "s"}</span></summary>
    <div class="ca-activity-body">${rows}</div>
  </details>`;
}

// Render a coding-agent phase: the findings report as formatted markdown, the
// activity transcript as a collapsible step list, and any error in its own
// block. This replaces the cramped plain-text rendering for sandbox Execute
// output (the report is markdown-heavy — headings, tables, lists).
export function codingAgentBody(response, opts = {}) {
  const { model = "" } = opts;
  const { findings, transcript, error } = splitCodingSections(response || "");
  let html = "";
  if (model) {
    html += `<div class="ca-banner">${icon("cpu", "sm")}<span>${esc(model)}</span><span class="ca-banner-sub">coding agent</span></div>`;
  }
  if (findings.trim()) html += `<div class="ca-findings md">${mdBlock(findings)}</div>`;
  if (transcript.trim()) html += codingActivity(transcript);
  if (error.trim())
    html += `<div class="ca-error"><div class="block-label">Error</div><pre class="code">${esc(error.trim())}</pre></div>`;
  return html || '<div class="empty-sub">No coding-agent output</div>';
}

// A friendly label for an event type: "task_assigned" → "Task assigned",
// "a2a.message_sent" → "A2a message sent".
function triggerLabel(type) {
  return String(type || "event")
    .replace(/[._]/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

// Render the event that triggered the turn as the invocation's *source*.
//
// ``trigger`` is the compact descriptor the engine threads onto every
// phase event: ``{id, type, summary, actor, timestamp}`` (see
// ``crewlet.events.types.describe_trigger``). Returns "" when there is
// no trigger (engine-internal turn) so callers can splice it in
// unconditionally.
//
// ``opts``:
//   compact — single inline chip (for the turn header); otherwise a
//             labelled "Source" block (for a row body / detail view).
//
// A notification trigger names its originating integration, so the source
// renders the branded icon + label (e.g. Slack · @alice) instead of a
// generic "external notification"; every other trigger keeps its plain
// type label.
//
// When the trigger carries an ``id`` the element is clickable; the
// caller wires ``data-action="open-event"`` to navigate to the event
// detail.
export function triggerSource(trigger, opts = {}) {
  const { compact = false } = opts;
  const t = trigger || {};
  if (!t.type && !t.summary && !t.id) return "";
  const integration = t.integration ? integrationMeta(t.integration) : null;
  const label = integration ? integration.label : triggerLabel(t.type);
  const iconId = integration ? integration.icon : "inbox";
  const text = t.summary || label;
  const clickable = !!t.id;
  const attrs = clickable
    ? `data-action="open-event" data-event-id="${escAttr(t.id)}"`
    : "";
  const colorVar = integration ? ` style="--int-color:${integration.color}"` : "";
  const cls = `trigger-src${clickable ? " clickable" : ""}${integration ? " integration" : ""}`;

  if (compact) {
    const who = t.sender
      ? `<span class="trigger-actor">${esc(t.sender)}</span>`
      : "";
    return `<span class="${cls} compact"${colorVar} ${attrs} title="${escAttr(text)}">
      ${icon(iconId, "sm")}<span class="trigger-type">${esc(label)}</span>${who}</span>`;
  }
  const actorName = t.sender || t.actor;
  const actor = actorName
    ? `<span class="trigger-actor">${esc(actorName)}</span>`
    : "";
  const when = t.timestamp
    ? `<span class="trigger-when">${esc(relTime(t.timestamp))}</span>`
    : "";
  return `
    <div class="block-label">Source</div>
    <div class="${cls}"${colorVar} ${attrs}>
      ${icon(iconId, "sm")}
      <span class="trigger-type">${esc(label)}</span>
      ${actor}
      <span class="trigger-summary">${esc(text)}</span>
      ${when}
      ${clickable ? icon("external", "sm") : ""}
    </div>`;
}

export function toolsAvailable(record) {
  const inCall = record.tools_available || [];
  const catalogue = record.tool_catalogue || [];
  if (!inCall.length && !catalogue.length) return "";
  const chips = (names) =>
    `<div class="chip-row">${names.map((n) => `<span class="chip">${esc(n)}</span>`).join("")}</div>`;
  let out = "";
  if (inCall.length)
    out += `<div class="block-label">Tools in call (${inCall.length})</div>${chips(inCall)}`;
  if (catalogue.length)
    out += `<div class="block-label">Plan tool catalogue (${catalogue.length})</div>${chips(catalogue)}`;
  return out;
}

// Prompt messages longer than this collapse by default — a multi-thousand
// char system prompt would otherwise bury the response and the rest of the
// invocation. Short messages (a one-line user ping) stay open: they cost
// little screen space and hiding them just adds a click.
const PROMPT_COLLAPSE_CHARS = 600;

// Compact size for the collapsed header: "480 chars", "4.2k chars".
function promptSize(n) {
  if (n < 1000) return `${n} chars`;
  const k = n / 1000;
  return `${k < 10 ? k.toFixed(1) : Math.round(k)}k chars`;
}

// One-line, whitespace-collapsed preview shown while a message is collapsed.
function promptPreview(s) {
  return trunc(String(s || "").replace(/\s+/g, " ").trim(), 110);
}

// Render the prompt as a stack of collapsible message blocks (one per
// system / user / assistant / tool message). Long messages start collapsed
// so the whole invocation stays scannable — you see the shape (system,
// user, response, tools) at a glance and expand only the message you want
// to read; the expanded body is height-capped and scrolls.
//
// ``opts``:
//   keyPrefix — stable prefix for message keys so toggles survive
//               re-renders / live streaming; defaults to "".
//   isOpen    — (key, long) => bool deciding each message's open state.
//               Omitted (static views) → "short open, long collapsed".
export function promptSections(record, opts = {}) {
  const { keyPrefix = "", isOpen = null } = opts;
  let msgs = record.prompt_messages;
  if (!msgs || !msgs.length) {
    msgs = [];
    if (record.system_prompt)
      msgs.push({ role: "system", content: record.system_prompt });
    if (record.user_prompt) msgs.push({ role: "user", content: record.user_prompt });
    if (!msgs.length && record.prompt)
      msgs.push({ role: "user", content: record.prompt });
  }
  if (!msgs.length) return '<div class="empty-sub">No prompt recorded</div>';
  return msgs
    .map((m, i) => {
      const role = m.role || "user";
      const content = m.content || "";
      const key = `${keyPrefix}#p${i}`;
      const long = content.length > PROMPT_COLLAPSE_CHARS;
      const open = isOpen ? isOpen(key, long) : !long;
      return `
      <div class="msg-block ${open ? "open" : ""}">
        <div class="msg-head" data-action="toggle-prompt" data-pkey="${escAttr(key)}">
          ${icon("chevron", "chevron")}
          <span class="msg-role ${esc(role)}">${esc(role)}</span>
          <span class="msg-size">${promptSize(content.length)}</span>
          <span class="msg-preview">${esc(promptPreview(content))}</span>
        </div>
        <pre class="code msg-pre">${esc(content)}</pre>
      </div>`;
    })
    .join("");
}

// Render the response body, interleaving <think> blocks and tool calls
// inline. ``opts``:
//   keyPrefix  — stable prefix for think/tool element keys (so toggles
//                survive re-renders); defaults to "".
//   thinkOpen  — (key) => bool, default open (reasoning shown).
//   toolOpen   — (key) => bool, default closed.
export function responseBody(response, toolExecutions, opts = {}) {
  // A sandbox Execute phase's output is a coding-agent findings report, not a
  // native LLM response — render it with its own findings / activity / error
  // sections (markdown + a collapsible step log).
  if (opts.codingAgent) return codingAgentBody(response, opts);
  // A failed phase leads with WHY. Everything the phase managed before it
  // died still renders underneath — the prompt it was working, the tools
  // that had already run — because that partial work is most of what
  // makes a failure diagnosable.
  const failurePrefix = opts.failure ? failureBlock(opts.failure) : "";
  const {
    keyPrefix = "",
    thinkOpen = () => true,
    toolOpen = () => false,
  } = opts;
  const resp = response || "";
  const tools = toolExecutions || [];

  // 1. Pull out <think> blocks, leaving placeholders so paragraph
  //    layout and tool distribution work on the visible text.
  const thinks = [];
  const PH = "\u0000THINK\u0000";
  const stripped = resp.replace(/<think>[\s\S]*?<\/think>/gi, (m) => {
    thinks.push(m);
    return PH;
  });
  const paras = stripped.split(/\n\n+/);

  let thinkIdx = 0;
  const thinkHtml = (footerBadges = "", footerDetails = "") => {
    if (thinkIdx >= thinks.length) return "";
    const key = `${keyPrefix}#t${thinkIdx}`;
    const content = thinks[thinkIdx].replace(/<\/?think>/gi, "").trim();
    thinkIdx++;
    const footer = footerBadges
      ? `<div class="think-tools">${footerBadges}</div>${footerDetails}`
      : "";
    return `
      <div class="think ${thinkOpen(key) ? "open" : ""}">
        <div class="think-head" data-action="toggle-think" data-tkey="${escAttr(key)}">
          ${icon("think", "sm")}<span>Reasoning</span>${icon("chevron", "chevron")}
        </div>
        <div class="think-body">${esc(content)}</div>${footer}
      </div>`;
  };

  const toolBadge = (tc, j) => {
    const key = `${keyPrefix}#x${j}`;
    const okColor = tc.success === false ? "var(--red)" : "var(--green)";
    return `<span class="tool-badge" data-action="toggle-tool" data-tkey="${escAttr(key)}" title="${escAttr(tc.name || "tool")}">
      <i class="dot" style="background:${okColor}"></i>${j + 1}. ${esc(tc.name || "tool")} ${icon("chevron", "sm")}</span>`;
  };
  const toolDetail = (tc, j) => {
    const key = `${keyPrefix}#x${j}`;
    let args = tc.arguments;
    try {
      args = typeof args === "string" ? args : JSON.stringify(args);
    } catch {
      args = String(args);
    }
    return `<div class="tool-detail ${toolOpen(key) ? "open" : ""}" data-tool-key="${escAttr(key)}">
      <div class="block-label">Arguments</div><pre class="code">${esc(prettyJson(args))}</pre>
      <div class="block-label">Result</div><pre class="code">${esc(String(tc.result ?? tc.error ?? ""))}</pre>
    </div>`;
  };

  // Render one paragraph (which may contain think placeholders). Trailing
  // tool badges/details attach to the last text chunk, or are tucked into
  // the final think block when reasoning is the last thing in the para.
  const renderPara = (para, trailBadges, trailDetails) => {
    const chunks = para.split(PH);
    let out = "";
    chunks.forEach((chunk, ci) => {
      const isLast = ci === chunks.length - 1;
      const hasTextAfter = chunks.slice(ci + 1).some((c) => c.trim());
      if (chunk.trim()) {
        out += `<div class="resp-para">${esc(chunk.trim())}${isLast ? trailBadges || "" : ""}</div>`;
        if (isLast && trailDetails) out += trailDetails;
      }
      if (!isLast) {
        const lastThink = !hasTextAfter;
        out += thinkHtml(lastThink ? trailBadges : "", lastThink ? trailDetails : "");
      }
    });
    return out;
  };

  if (!tools.length) {
    const out = paras
      .map((p) =>
        p.includes(PH)
          ? renderPara(p, "", "")
          : p.trim()
            ? `<div class="resp-para">${esc(p)}</div>`
            : "",
      )
      .join("");
    return failurePrefix + (out || emptyResponse(opts));
  }

  // Distribute tool calls across inter-paragraph slots, preserving order.
  const numSlots = Math.max(paras.length - 1, 1);
  const slots = Array.from({ length: numSlots }, () => []);
  tools.forEach((tc, j) => {
    const idx = Math.min(Math.floor((j * numSlots) / tools.length), numSlots - 1);
    slots[idx].push({ tc, j });
  });

  let html = "";
  paras.forEach((para, pIdx) => {
    const slotTools = pIdx < numSlots ? slots[pIdx] : [];
    const badges = slotTools.map(({ tc, j }) => toolBadge(tc, j)).join("");
    const details = slotTools.map(({ tc, j }) => toolDetail(tc, j)).join("");
    if (para.includes(PH)) {
      html += renderPara(para, badges, details);
    } else if (para.trim() || badges) {
      html += `<div class="resp-para">${esc(para)}${badges}</div>${details}`;
    }
  });
  return failurePrefix + (html || emptyResponse(opts));
}

// What to say when a record carries no response text.
//
// "No response text yet" is a statement about an in-flight call, and
// showing it for a call that will never answer is how a hard failure
// used to read as a hang. A finished call with nothing in it says so;
// a failed one has already rendered its reason above and does not need
// a placeholder at all.
function emptyResponse(opts = {}) {
  if (opts.failure) return "";
  if (opts.inProgress === false) {
    return '<div class="empty-sub">This call produced no response text.</div>';
  }
  return '<div class="empty-sub">No response text yet</div>';
}

// Human labels for the failure classes the engine reports. The key is
// ``error_kind`` — the classified provider error where there was one,
// otherwise the exception type or guard-breach kind.
const FAILURE_LABEL = {
  rate_limit: "Rate limited",
  auth: "Authentication rejected",
  timeout: "Timed out",
  overloaded: "Provider overloaded",
  connection: "Could not reach the provider",
  budget_exhausted: "Token budget exhausted",
  llm_unavailable: "No LLM provider available",
  unhandled_exception: "Unhandled error",
  scheduled_timeout: "Ran past its time cap",
  stall: "Stalled",
  max_iter: "Hit the round cap",
  depth_cap: "Delegation too deep",
  fatal: "Provider rejected the call",
};

export function failureLabel(kind) {
  const key = String(kind || "").toLowerCase();
  return FAILURE_LABEL[key] || (key ? triggerLabel(key) : "Failed");
}

// The failure banner shown at the top of a failed call.
export function failureBlock(failure) {
  if (!failure) return "";
  const kind = failure.kind || failure.error_kind || "";
  const message = failure.message || failure.error || "";
  return `
    <div class="failure">
      <div class="failure-head">
        ${icon("alert", "sm")}
        <span class="failure-title">${esc(failureLabel(kind))}</span>
        ${kind ? `<span class="failure-kind">${esc(kind)}</span>` : ""}
      </div>
      ${message ? `<pre class="failure-msg">${esc(message)}</pre>` : ""}
    </div>`;
}

// Normalize the failure a record carries into the shape the block above
// renders, or ``null`` when the record did not fail. Records arrive from
// two places — the event store's LLM history and the live projection's
// in-flight call — and this is the one place that difference is
// reconciled.
export function failureOf(record) {
  if (!record) return null;
  if (record.error && typeof record.error === "object") return record.error;
  if (!record.failed) return null;
  return { kind: record.error_kind || "", message: record.error || "" };
}

// One-line stats row (in / out / total tokens).
export function tokenStats(record) {
  return `
    <div class="stat-grid" style="grid-template-columns:repeat(3,1fr);margin:0 0 6px">
      <div class="stat"><div class="stat-label">Input</div><div class="stat-value">${(record.input_tokens || 0).toLocaleString()}</div></div>
      <div class="stat"><div class="stat-label">Output</div><div class="stat-value">${(record.output_tokens || 0).toLocaleString()}</div></div>
      <div class="stat"><div class="stat-label">Total</div><div class="stat-value">${(record.total_tokens || 0).toLocaleString()}</div></div>
    </div>`;
}
