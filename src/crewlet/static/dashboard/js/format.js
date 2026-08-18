// Formatting + escaping helpers (pure, no DOM).

export function esc(s) {
  return s == null
    ? ""
    : String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

export function escAttr(s) {
  return esc(s).replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

// Quote-escape a value that is ALREADY html-escaped, for use inside an
// attribute. `esc` deliberately leaves quotes alone (they are harmless
// in text and escaping them makes prose noisy), which is exactly what
// makes them dangerous once the text lands in an attribute instead.
function quoteAttr(escaped) {
  return String(escaped).replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

export function trunc(s, n) {
  s = s == null ? "" : String(s);
  return s.length > n ? s.slice(0, n) + "…" : s;
}

export function prettyJson(v) {
  try {
    return JSON.stringify(typeof v === "string" ? JSON.parse(v) : v, null, 2);
  } catch {
    return typeof v === "string" ? v : JSON.stringify(v);
  }
}

export function fmtNum(n) {
  return (Number(n) || 0).toLocaleString();
}

// A short magnitude for a figure that has to fit in a strip: 77,280 →
// "77.3k". Only for display slots where the exact number is available
// elsewhere (a title, the Tokens view) — never as the only rendering of
// a value someone might need to read exactly.
export function fmtCompact(n) {
  const v = Number(n) || 0;
  const abs = Math.abs(v);
  if (abs < 1000) return String(Math.round(v));
  if (abs < 1_000_000) {
    const k = v / 1000;
    return (abs < 10_000 ? k.toFixed(1) : Math.round(k)) + "k";
  }
  const m = v / 1_000_000;
  return (abs < 10_000_000 ? m.toFixed(1) : Math.round(m)) + "M";
}

// Very small, safe markdown for memory/summary text. Escapes first, then
// applies bold / inline-code / bullet markers. Newlines are preserved by
// the consumer's `white-space: pre-wrap`.
export function mdLite(s) {
  let h = esc(s || "");
  h = h.replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>");
  h = h.replace(/`([^`\n]+)`/g, "<code>$1</code>");
  h = h.replace(/^(\s*)[-*]\s+/gm, "$1• ");
  return h;
}

// Inline markdown on a single (already-escaped-by-`mdBlock`) run: inline code,
// bold, and http(s) links. Italic via `*`/`_` is deliberately omitted — it
// mangles `snake_case` identifiers and `a * b` arithmetic that show up in
// coding-agent reports, and the cost of missing it is tiny.
function mdInline(s) {
  let h = esc(s);
  h = h.replace(/`([^`]+)`/g, "<code>$1</code>");
  h = h.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  // The URL goes into an attribute, so it needs the quote escaping `esc`
  // deliberately leaves out. Without it a link whose URL contains a
  // double quote closes `href` early and everything after it becomes
  // attributes — `[x](https://a" onmouseover="…)` is an injection, not a
  // link.
  h = h.replace(
    /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
    (_m, label, url) =>
      `<a href="${quoteAttr(url)}" target="_blank" rel="noopener">${label}</a>`,
  );
  // Autolink bare URLs (findings reports cite PRs/links bare). The leading
  // `(^|[\s(])` group keeps it from matching a URL already inside an
  // ``href="…"`` produced just above (that one is preceded by a quote).
  h = h.replace(
    /(^|[\s(])(https?:\/\/[^\s<)]+)/g,
    (_m, lead, url) =>
      `${lead}<a href="${quoteAttr(url)}" target="_blank" rel="noopener">${url}</a>`,
  );
  return h;
}

// Block-level markdown → HTML for coding-agent findings reports (the only
// rich-markdown source in the dashboard). Escapes every text run first, then
// re-introduces a known, safe tag set — headings, fenced code, GFM tables,
// ordered/unordered lists, blockquotes, hr, and inline code/bold/links.
// Not a full CommonMark implementation: just the subset these reports use.
// Every branch advances the cursor, so it can never loop.
export function mdBlock(src) {
  const lines = String(src == null ? "" : src).split("\n");
  const isSep = (s) => /^[\s|:-]+$/.test(s) && s.includes("-") && s.includes("|");
  const cells = (r) =>
    r
      .replace(/^\s*\|/, "")
      .replace(/\|\s*$/, "")
      .split("|")
      .map((c) => c.trim());
  const out = [];
  let i = 0;
  const n = lines.length;
  while (i < n) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    // Fenced code block.
    if (/^\s*```/.test(line)) {
      const buf = [];
      i++;
      while (i < n && !/^\s*```/.test(lines[i])) buf.push(lines[i++]);
      if (i < n) i++; // closing fence
      out.push(`<pre class="code md-code">${esc(buf.join("\n"))}</pre>`);
      continue;
    }
    // ATX heading.
    const h = line.match(/^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/);
    if (h) {
      out.push(`<div class="md-h md-h${h[1].length}">${mdInline(h[2])}</div>`);
      i++;
      continue;
    }
    // Horizontal rule.
    if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(line)) {
      out.push('<hr class="md-hr">');
      i++;
      continue;
    }
    // GFM table (header row followed by a |---|---| separator).
    if (line.includes("|") && i + 1 < n && isSep(lines[i + 1])) {
      const head = cells(line);
      i += 2;
      const rows = [];
      while (i < n && lines[i].includes("|") && lines[i].trim()) rows.push(cells(lines[i++]));
      const th = head.map((c) => `<th>${mdInline(c)}</th>`).join("");
      const tb = rows
        .map((r) => `<tr>${r.map((c) => `<td>${mdInline(c)}</td>`).join("")}</tr>`)
        .join("");
      out.push(`<table class="md-table"><thead><tr>${th}</tr></thead><tbody>${tb}</tbody></table>`);
      continue;
    }
    // Unordered list.
    if (/^\s*[-*+]\s+/.test(line)) {
      const items = [];
      while (i < n && /^\s*[-*+]\s+/.test(lines[i]))
        items.push(lines[i++].replace(/^\s*[-*+]\s+/, ""));
      out.push(`<ul class="md-ul">${items.map((it) => `<li>${mdInline(it)}</li>`).join("")}</ul>`);
      continue;
    }
    // Ordered list.
    if (/^\s*\d+[.)]\s+/.test(line)) {
      const items = [];
      while (i < n && /^\s*\d+[.)]\s+/.test(lines[i]))
        items.push(lines[i++].replace(/^\s*\d+[.)]\s+/, ""));
      out.push(`<ol class="md-ol">${items.map((it) => `<li>${mdInline(it)}</li>`).join("")}</ol>`);
      continue;
    }
    // Blockquote.
    if (/^\s*>\s?/.test(line)) {
      const buf = [];
      while (i < n && /^\s*>\s?/.test(lines[i])) buf.push(lines[i++].replace(/^\s*>\s?/, ""));
      out.push(`<blockquote class="md-quote">${buf.map(mdInline).join("<br>")}</blockquote>`);
      continue;
    }
    // Paragraph: gather consecutive lines until a blank line or a block starter.
    const buf = [];
    while (
      i < n &&
      lines[i].trim() &&
      !/^\s*(#{1,6}\s|[-*+]\s|\d+[.)]\s|>\s?|```)/.test(lines[i]) &&
      !(lines[i].includes("|") && i + 1 < n && isSep(lines[i + 1]))
    )
      buf.push(lines[i++]);
    if (buf.length) out.push(`<p class="md-p">${buf.map(mdInline).join(" ")}</p>`);
    else {
      out.push(`<p class="md-p">${mdInline(line)}</p>`); // safety: always advance
      i++;
    }
  }
  return out.join("");
}

// Parse an ISO timestamp, treating a missing timezone as UTC (the API
// emits naive-UTC and aware-UTC strings interchangeably).
export function parseUTC(ts) {
  if (!ts) return null;
  let s = String(ts);
  if (!/[zZ]|[+-]\d\d:?\d\d$/.test(s)) s += "Z";
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

export function fmtTime(ts) {
  const d = parseUTC(ts);
  if (!d) return "";
  return d.toLocaleTimeString("en-GB", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function fmtDateTime(ts) {
  const d = parseUTC(ts);
  if (!d) return "";
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// A bare magnitude ("3h", "2d"), no direction. `relTime` / `untilTime` add it.
function relMagnitude(secs) {
  if (secs < 60) return secs + "s";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return mins + "m";
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return hrs + "h";
  return Math.floor(hrs / 24) + "d";
}

export function relTime(ts) {
  const d = parseUTC(ts);
  if (!d) return "";
  const secs = Math.floor((Date.now() - d.getTime()) / 1000);
  if (secs < 5) return "just now";
  if (secs >= 30 * 86400) return d.toLocaleDateString();
  return relMagnitude(secs) + " ago";
}

// The forward-looking twin, for scheduled work that has not fired yet.
// `relTime` measures elapsed time and clamps anything in the future to
// "just now", which would render every upcoming run as imminent.
export function untilTime(ts) {
  const d = parseUTC(ts);
  if (!d) return "";
  const secs = Math.floor((d.getTime() - Date.now()) / 1000);
  if (secs <= 0) return "due now";
  if (secs >= 30 * 86400) return "on " + d.toLocaleDateString();
  return "in " + relMagnitude(secs);
}

export function fmtDuration(startTs, endTs) {
  const a = parseUTC(startTs);
  const b = parseUTC(endTs);
  if (!a || !b) return "";
  return fmtMs(Math.max(0, b.getTime() - a.getTime()));
}

export function fmtMs(ms) {
  ms = Number(ms) || 0;
  if (ms < 1000) return ms + "ms";
  const s = ms / 1000;
  if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + "s";
  const m = Math.floor(s / 60);
  return m + "m " + Math.floor(s % 60) + "s";
}

export function shortId(id, n = 8) {
  return id ? String(id).slice(0, n) : "";
}
