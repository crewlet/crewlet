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
 * Every token in one run of prose, in order, none overlapping (rule 7), with
 * `owners` the code hosts' owners in lower case ([codeHostOwners]).
 */
export function inlineTokens(text: string, owners: ReadonlySet<string> = NO_OWNERS): InlineToken[] {
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
  const commits: InlineToken[] = [];
  for (const m of text.matchAll(COMMIT_AFTER)) {
    const sha = m[2]!;
    if (!mixed(sha)) continue;
    const at = m.index + m[0].length - sha.length;
    const before = chosen.filter((t) => t.kind === "reference" && t.end <= m.index).at(-1);
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

// --- what an address is ----------------------------------------------------

/** The vendors an address can be drawn with the mark of (`@crewlethq/icons`' `Vendor`). */
export type LinkVendor = "github" | "gitlab" | "slack" | "mattermost" | "atlassian";

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

/** The hosts an app lives at: its public one, and the company's own. */
function basesOf(hosts: readonly LinkHost[], kind: LinkHostKind, publicBase?: string): string[] {
  const own = hosts.filter((h) => h.kind === kind).map((h) => h.url);
  return publicBase ? [publicBase, ...own] : own;
}

function firstUnder(url: URL, bases: readonly string[]): string[] | null {
  for (const base of bases) {
    const parts = under(url, base);
    if (parts !== null) return parts;
  }
  return null;
}

/** A host at a domain or under it, on a dot: `acme.slack.com` is Slack's, `slack.com.evil.test` is not. */
function atDomain(host: string, domain: string): boolean {
  const h = host.toLowerCase();
  return h === domain || h.endsWith(`.${domain}`);
}

function describeGitHub(parts: string[], url: URL): LinkDescription {
  const [owner, repo, what, id] = parts;
  if (!owner || !repo || !CODE_NAME.test(owner) || !CODE_NAME.test(repo)) {
    return { vendor: "github", label: shortAddress(url), code: false, kind: "GitHub" };
  }
  if ((what === "pull" || what === "issues") && id && /^\d+$/.test(id)) {
    const label = `${owner}/${repo}#${id}`;
    return {
      vendor: "github",
      label,
      code: true,
      kind: "GitHub",
    };
  }
  if (what === "commit" && id && SHA.test(id)) {
    const label = `${owner}/${repo}@${id.slice(0, 7)}`;
    return { vendor: "github", label, code: true, kind: "GitHub" };
  }
  if (what === undefined) {
    return {
      vendor: "github",
      label: `${owner}/${repo}`,
      code: true,
      kind: "GitHub",
    };
  }
  return {
    vendor: "github",
    label: shortAddress(url),
    code: false,
    kind: "GitHub",
  };
}

function describeGitLab(parts: string[], url: URL): LinkDescription {
  const dash = parts.indexOf("-");
  const names = dash < 0 ? parts : parts.slice(0, dash);
  if (names.length < 2 || !names.every((name) => CODE_NAME.test(name))) {
    return { vendor: "gitlab", label: shortAddress(url), code: false, kind: "GitLab" };
  }
  const project = names.join("/");
  const [what, id] = dash < 0 ? [] : parts.slice(dash + 1);
  if (what === "merge_requests" && id && /^\d+$/.test(id)) {
    const label = `${project}!${id}`;
    return { vendor: "gitlab", label, code: true, kind: "GitLab" };
  }
  if (what === "issues" && id && /^\d+$/.test(id)) {
    const label = `${project}#${id}`;
    return { vendor: "gitlab", label, code: true, kind: "GitLab" };
  }
  if (what === "commit" && id && SHA.test(id)) {
    const label = `${project}@${id.slice(0, 7)}`;
    return { vendor: "gitlab", label, code: true, kind: "GitLab" };
  }
  if (dash < 0) {
    return { vendor: "gitlab", label: project, code: true, kind: "GitLab" };
  }
  return {
    vendor: "gitlab",
    label: shortAddress(url),
    code: false,
    kind: "GitLab",
  };
}

function describeSlack(url: URL): LinkDescription {
  const team = url.hostname.split(".")[0] ?? "";
  const parts = segments(url.pathname);
  if (parts[0] === "archives" && parts[1] && team !== "app") {
    const what = parts[2]?.startsWith("p") ? "Message" : "Channel";
    const label = `${what} in ${team}`;
    return { vendor: "slack", label, code: false, kind: "Slack" };
  }
  return {
    vendor: "slack",
    label: shortAddress(url),
    code: false,
    kind: "Slack",
  };
}

function describeMattermost(parts: string[], url: URL): LinkDescription {
  const [team, what, name] = parts;
  if (team && what === "channels" && name) {
    return {
      vendor: "mattermost",
      label: `~${name}`,
      code: false,
      kind: "Mattermost",
    };
  }
  if (team && what === "pl" && name) {
    return {
      vendor: "mattermost",
      label: `Post in ${team}`,
      code: false,
      kind: "Mattermost",
    };
  }
  return {
    vendor: "mattermost",
    label: shortAddress(url),
    code: false,
    kind: "Mattermost",
  };
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

function describeJira(parts: string[], tenant: string | null): LinkDescription | null {
  const at = parts.indexOf("browse");
  const key = at < 0 ? undefined : parts[at + 1];
  if (key && /^[A-Z][A-Z0-9]*-\d+$/.test(key)) {
    return {
      vendor: "atlassian",
      label: onTenant(key, tenant),
      code: tenant === null,
      kind: "Jira",
    };
  }
  return null;
}

function describeConfluence(
  parts: string[],
  url: URL,
  tenant: string | null,
): LinkDescription | null {
  const at = parts.indexOf("pages");
  const slug = at < 0 ? undefined : parts[at + 2];
  if (slug) {
    const title = slug.replace(/\+/g, " ");
    return { vendor: "atlassian", label: onTenant(title, tenant), code: false, kind: "Confluence" };
  }
  if (parts.includes("wiki") || parts.includes("spaces") || parts.includes("display")) {
    return {
      vendor: "atlassian",
      label: shortAddress(url),
      code: false,
      kind: "Confluence",
    };
  }
  return null;
}

/**
 * What an address is: whose mark it carries, what it says and what it is
 * called, read from the PARSED address alone (rule 2).
 *
 * THE HOST DECIDES, never a label: a link reading "github.com/x" that goes
 * elsewhere is drawn as where it goes. A host is an app's when it is the
 * app's public host exactly (github.com, gitlab.com), a host under the app's
 * own domain on a dot boundary (`*.slack.com`, `*.atlassian.net`), or one the
 * company configured for it (`link_hosts`, with its path prefix). Null for an
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

  const gh = firstUnder(url, basesOf(hosts, "github", "https://github.com"));
  if (gh !== null) return describeGitHub(gh, url);
  const gl = firstUnder(url, basesOf(hosts, "gitlab", "https://gitlab.com"));
  if (gl !== null) return describeGitLab(gl, url);
  if (atDomain(url.hostname, "slack.com")) return describeSlack(url);
  const mm = firstUnder(url, basesOf(hosts, "mattermost"));
  if (mm !== null) return describeMattermost(mm, url);

  const cloud = atDomain(url.hostname, "atlassian.net");
  const ownJira = firstUnder(url, basesOf(hosts, "jira"));
  const ownWiki = firstUnder(url, basesOf(hosts, "confluence"));
  const jira = ownJira ?? (cloud ? segments(url.pathname) : null);
  const wiki = ownWiki ?? (cloud ? segments(url.pathname) : null);
  const atlassian =
    (jira && describeJira(jira, ownJira === null ? url.host : null)) ??
    (wiki && describeConfluence(wiki, url, ownWiki === null ? url.host : null));
  if (atlassian) return atlassian;
  if (cloud || jira !== null || wiki !== null) {
    return {
      vendor: "atlassian",
      label: shortAddress(url),
      code: false,
      kind: "Atlassian",
    };
  }
  return { vendor: null, label: shortAddress(url), code: false, kind: null };
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
  const base = vendor === "github" ? github : gitlab;
  if (base === null) return { vendor, href: null };
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
  const encoded = path
    .split("/")
    .map((part) => encodeURIComponent(part))
    .join("/");
  return { vendor, href: safeHref(`${base}/${encoded}/${tail}`) };
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
  const href = safeHref(`${host.url}/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`);
  return { vendor: host.kind as "github" | "gitlab", href };
}

/** Every owner the code hosts name, in lower case: what [inlineTokens] reads rule 3a by. */
export function codeHostOwners(hosts: readonly LinkHost[]): ReadonlySet<string> {
  const owners = hosts.flatMap((h) =>
    h.kind === "github" || h.kind === "gitlab" ? (h.owners ?? []) : [],
  );
  return owners.length === 0 ? NO_OWNERS : new Set(owners.map((o) => o.toLowerCase()));
}
