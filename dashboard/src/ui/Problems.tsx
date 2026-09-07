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
 * somewhere to go.
 */

import type { ReactNode } from "react";

/** One problem: where it is, and what to do about it. */
export interface Problem {
  /** The config path, when the line opens with one. */
  path?: string;
  /** The rest of the line. */
  text: string;
}

// A CONFIG PATH, which is what a problem line opens with: dotted segments,
// optionally indexed. Anchored, so a sentence that merely contains a dot is
// not mistaken for one.
const pathHead = /^([a-z][a-z0-9_]*(?:\[\d+\])?(?:\.[a-z0-9_]+(?:\[\d+\])?)*): (.*)$/s;

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

// What gets its own face inside a sentence, in the order they are tried.
//
// A NAMED LINK AND A BACKTICK COME FIRST, because they are what an author
// wrote deliberately and every other pattern would claim a piece of what is
// inside them. Then a bare URL, which contains characters the rest would
// split. The last three are recognised rather than marked up: a config path,
// a route and a quoted value are what the engine's own sentences are full of,
// and asking every message to annotate them would be asking every author to
// remember.
const token =
  /\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)|`([^`]+)`|(https?:\/\/[^\s,)]+[^\s,.)])|(\$\{[A-Za-z0-9_]+\})|(\/[a-z0-9-]+(?:\/[a-z0-9{}_-]+)+)|("[^"]*")|\b([a-z][a-z0-9_]*(?:\.[a-z0-9_]+){2,})\b/g;

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
 */
export function marked(text: string): ReactNode[] {
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
        <code key={key++} className="inline">
          {ticked}
        </code>,
      );
    } else if (url) {
      out.push(
        <a key={key++} href={url} target="_blank" rel="noreferrer">
          {url}
        </a>,
      );
    } else if (ref) {
      out.push(
        <code key={key++} className="inline is-reference">
          {ref}
        </code>,
      );
    } else {
      out.push(
        <code key={key++} className="inline">
          {route ?? quoted ?? path}
        </code>,
      );
    }
    last = at + whole.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/** Problems renders a refusal as one line per thing that is wrong. */
export function Problems({ detail }: { detail: string }) {
  const problems = split(detail);
  // ONE PROBLEM IS A SENTENCE, not a list of one: a bullet on its own reads
  // as the first of several and sets a reader looking for the rest.
  if (problems.length === 1) {
    const only = problems[0];
    if (!only) return null;
    return (
      <span>
        {only.path && <code className="inline">{only.path}</code>}
        {only.path && " "}
        {marked(only.text)}
      </span>
    );
  }
  return (
    <ul className="problems">
      {problems.map((p, i) => (
        <li key={i}>
          {p.path && <code className="inline">{p.path}</code>}
          {p.path && " "}
          {marked(p.text)}
        </li>
      ))}
    </ul>
  );
}
