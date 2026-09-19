/**
 * The engine's refusal, rendered as the list of problems it actually is.
 *
 * A validation refusal is several problems joined with newlines, each one
 * `path: kind: what to do`. HTML collapses those newlines, so the banner
 * showed one run-on paragraph in which the second problem's config path ran
 * straight into the end of the first one's sentence: "…nowhere to search
 * integrations.confluence.webhook_secret: required value missing…". A reader
 * could not tell how many things were wrong, let alone which.
 *
 * The parts are styled because they are different KINDS of thing and a wall
 * of one face makes a reader parse the punctuation to find out which is
 * which: a config path is a place in their document, a `${VAR}` is an entry
 * in their secret store, a route is an address on this engine, and a link is
 * somewhere to go. Each of those is uilet's [InlineCode] now, which is where
 * the two faces this file drew by hand already live: `reference` holds a
 * `${NAME}`'s braces on one line, because a pointer torn across a line break
 * names nothing.
 *
 * THIS IS CONTENT, NOT A CONTAINER, and that is the one part of the port that
 * did not happen. [Callout] is a card with ONE body and no list of its own,
 * and this is the body: the setup dialog already puts it in a danger Callout
 * (`SetupDialog.tsx`), and [Field] puts the same thing in a field's error
 * line, which is one sentence rather than a card. Made a Callout itself it
 * would nest one inside the dialog's and box every refused field on the form.
 * What makes the two compose is `tone`: a chip inside a message carries that
 * message's ink, or a calm grey identifier sits in the middle of an alarming
 * sentence.
 */

import type { ReactNode } from "react";
import { InlineCode, type InlineCodeTone } from "@crewlethq/ui";

/** One problem: where it is, and what to do about it. */
export interface Problem {
  /** The config path, when the line opens with one. */
  path?: string;
  /** The rest of the line. */
  text: string;
}

// A CONFIG PATH, as ONE FRAGMENT shared by the three patterns below that find
// one: at the head of a line, inside a sentence, and among the tokens a
// sentence is marked up with. They were three spellings of one shape, and
// they drifted in the direction that hides a problem: a map key is written
// as the operator wrote it, so `mcp_env.jira-cloud.API_TOKEN` has a hyphen
// and capitals, and none of the three recognised it: a refusal naming one
// rendered the path as prose, and [paths] never reported it to a caller that
// acts on what a refusal names.
//
// The first segment is a top-level key, always lower snake case, which is
// what stops an ordinary capitalised word from starting a path. Later
// segments are map keys too: letters of either case, digits, underscores and
// hyphens, never ending in a hyphen, so a path followed by a dash in a
// sentence does not swallow it. Each segment may carry list indexes.
const INDEX = String.raw`(?:\[\d+\])*`;
const HEAD_SEGMENT = String.raw`[a-z][a-z0-9_]*` + INDEX;
const KEY_SEGMENT = String.raw`\.[A-Za-z0-9_](?:[A-Za-z0-9_-]*[A-Za-z0-9_])?` + INDEX;
// Not followed by more of a key: a lookahead rather than `\b`, because a
// path can end in `]`, where a word boundary before a space does not exist.
//
// AND THE HYPHEN IS NOT IN IT, although a key may carry one. Excluding it
// here refuses the whole match rather than a shorter one: the segment above
// already declines to end in a hyphen, so on `mcp_env.jira-cloud.API_TOKEN-`
// the engine backtracks segment by segment looking for a shorter path that
// this lookahead will accept, finds none, and reports NO PATH AT ALL —
// measured on three sentences, each of which named a path and rendered it as
// prose. The segment is what stops a trailing dash being swallowed; this only
// has to stop a path ending mid-key.
const END = String.raw`(?![A-Za-z0-9_])`;
// A path inside a sentence has three segments or more, so an abbreviation
// ("e.g") or a file name ("crewlet.yaml") is not mistaken for one.
const INLINE_PATH = String.raw`\b${HEAD_SEGMENT}(?:${KEY_SEGMENT}){2,}${END}`;

// What a problem line opens with. Anchored, so a sentence that merely
// contains a dot is not mistaken for one, and any number of segments counts.
const pathHead = new RegExp(String.raw`^(${HEAD_SEGMENT}(?:${KEY_SEGMENT})*): (.*)$`, "s");

/**
 * split turns a joined refusal into its problems.
 *
 * BLANK LINES DROPPED and nothing else: what the engine said is what a person
 * reads, because a message this screen rewrote would be a second, quieter
 * statement of a rule that lives in Go.
 */
export function split(detail: string): Problem[] {
  return detail
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const head = pathHead.exec(line);
      return head ? { path: head[1], text: head[2] ?? "" } : { text: line };
    });
}

/**
 * paths finds every config path a refusal names, wherever it sits in the line.
 *
 * NOT [split]'s `path`, WHICH IS ONLY THE HEAD. A problem line sometimes opens
 * with `some.config.path: ` and sometimes carries the path inside its sentence
 * — `integrations.datadog.route_to required value missing: …` is the engine's
 * commonest shape — and the head pattern sees only the first of those. The
 * renderer never cared, because [marked] picks a path out of the prose either
 * way; a caller that wants to ACT on one does.
 *
 * Shared with the renderer rather than reimplemented beside it, so what a
 * caller matches on is exactly what the reader sees marked up. The setup
 * dialog opens the disclosure hiding a field a refusal names, and a second
 * pattern here would be a field revealed on one screen and hidden on another
 * for a difference nobody could see.
 */
export function paths(detail: string): string[] {
  const out = new Set<string>();
  for (const line of detail.split("\n")) {
    const head = pathHead.exec(line.trim());
    if (head?.[1]) out.add(head[1]);
    for (const m of line.matchAll(inlinePath)) out.add(m[0]);
  }
  return [...out];
}

// A config path sitting inside a sentence. The same fragment [marked] builds
// its path token from below, so what a caller matches on is what a reader
// sees marked up.
const inlinePath = new RegExp(INLINE_PATH, "g");

// What gets its own face inside a sentence, in the order they are tried.
//
// A NAMED LINK AND A BACKTICK COME FIRST, because they are what an author
// wrote deliberately and every other pattern would claim a piece of what is
// inside them. Then a bare URL, which contains characters the rest would
// split. The last three are recognised rather than marked up: a config path,
// a route and a quoted value are what the engine's own sentences are full of,
// and asking every message to annotate them would be asking every author to
// remember.
const token = new RegExp(
  String.raw`\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)|` +
    "`([^`]+)`|" +
    String.raw`(https?:\/\/[^\s,)]+[^\s,.)])|(\$\{[A-Za-z0-9_]+\})|(\/[a-z0-9-]+(?:\/[a-z0-9{}_-]+)+)|("[^"]*")|` +
    `(${INLINE_PATH})`,
  "g",
);

/**
 * marked renders a sentence with its values, paths and links picked out.
 *
 * TWO THINGS AN AUTHOR MARKS and five the renderer recognises. A link the
 * words carry, `[Create a token](https://…)`, and a literal in backticks are
 * written deliberately, because only the person writing the sentence knows
 * where the link belongs in it and which word is a value. Everything else is
 * found: a config path, a route, a `${VAR}` and a quoted value are what these
 * sentences are full of, and asking every one of them to annotate those would
 * be asking every author to remember.
 *
 * `tone` is which ink the chips take, and it is a fact about WHERE the
 * sentence is drawn rather than about the sentence. In ordinary prose — a
 * requirement's help line, a disconnect dialog's path — a chip keeps the code
 * face's own quiet ink, which is what says "this is a string you type". Inside
 * a refusal it takes the message's, or a calm grey identifier sits in the
 * middle of an alarming sentence. [Problems] is the one caller that asks for
 * `inherit`; everything else takes the default.
 */
export function marked(text: string, tone: InlineCodeTone = "default"): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let key = 0;
  for (const m of text.matchAll(token)) {
    const at = m.index ?? 0;
    if (at > last) out.push(text.slice(last, at));
    const [whole, linked, href, ticked, url, ref, route, quoted, path] = m;
    // A NEW TAB, on every anchor here: this sits in a dialog holding a
    // half-filled form, and following a link in place would throw the form
    // away to read a page about how to fill it in.
    if (linked && href) {
      out.push(
        <a key={key++} href={href} target="_blank" rel="noreferrer">
          {linked}
        </a>,
      );
    } else if (ticked) {
      out.push(
        <InlineCode key={key++} tone={tone}>
          {ticked}
        </InlineCode>,
      );
    } else if (url) {
      out.push(
        <a key={key++} href={url} target="_blank" rel="noreferrer">
          {url}
        </a>,
      );
    } else if (ref) {
      out.push(
        // A SEALED ENTRY'S NAME, which is one token a reader copies back: the
        // `reference` variant is what keeps its braces together, because a
        // pointer broken across a line names nothing.
        <InlineCode key={key++} variant="reference" tone={tone}>
          {ref}
        </InlineCode>,
      );
    } else {
      out.push(
        <InlineCode key={key++} tone={tone}>
          {route ?? quoted ?? path}
        </InlineCode>,
      );
    }
    last = at + whole.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/**
 * Problems renders a refusal as one line per thing that is wrong.
 *
 * INSIDE SOMETHING THAT IS ALREADY SAYING THIS IS BAD — a danger Callout, a
 * field's error line — so the chips take that message's ink rather than their
 * own. See the note at the top for why this is not the Callout itself.
 */
export function Problems({ detail }: { detail: string }) {
  const problems = split(detail);
  // ONE PROBLEM IS A SENTENCE, not a list of one: a bullet on its own reads
  // as the first of several and sets a reader looking for the rest.
  if (problems.length === 1) {
    const only = problems[0];
    if (!only) return null;
    return (
      <span>
        {only.path && <InlineCode tone="inherit">{only.path}</InlineCode>}
        {only.path && " "}
        {marked(only.text, "inherit")}
      </span>
    );
  }
  return (
    <ul className="problems">
      {problems.map((p, i) => (
        <li key={i}>
          {p.path && <InlineCode tone="inherit">{p.path}</InlineCode>}
          {p.path && " "}
          {marked(p.text, "inherit")}
        </li>
      ))}
    </ul>
  );
}

/**
 * A refusal as a field's error line, or nothing at all.
 *
 * The design system's form row takes a NODE for its error, so every caller
 * would otherwise spell the same ternary: a refusal is a list of problems
 * rather than a sentence, and a field that rendered its error as plain text
 * would be the one place in the product where a config path does not read as
 * one.
 */
export function withProblems(detail: string | undefined): ReactNode {
  return detail ? <Problems detail={detail} /> : undefined;
}
