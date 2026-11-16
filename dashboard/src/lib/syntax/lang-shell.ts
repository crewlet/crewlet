/**
 * POSIX shells and bash; a state carries an open quote, or a heredoc's
 * delimiter (`h-EOF` strips tabs, `h_EOF` does not).
 */
import { Tokens, done, isDigit, isSpace, skipSpace, step, until, word, words } from "./scan.ts";
import type { Grammar, Line, State } from "./types.ts";

const KEYWORDS = words(
  "if then else elif fi for while until do done case esac in function select time coproc",
);
const BUILTINS = words(
  "echo cd export local return set unset read printf test source exit shift trap eval exec readonly declare alias",
);
/** Keywords after which a command starts. */
const OPENS = words("if then else elif do while until time");
const NAME = /[A-Za-z_]\w*/y;
const DELIMITER = /[A-Za-z_][\w.-]*/y;
const DIGITS = /\d+/y;
const META = "|&;<>(){}[]";
/** The longest heredoc delimiter carried. */
const MAX_DELIMITER = 40;

const isMeta = (c: string | undefined) => c !== undefined && META.includes(c);
const ends = (c: string | undefined) =>
  c === undefined || isSpace(c) || isMeta(c) || c === '"' || c === "'" || c === "$" || c === "\\";

/**
 * Tokens of a shell line from `i` on, pushed to `out`, from `state`; the state
 * it leaves. `make` reads `$(VAR)` and `$<` as a Makefile recipe's variables.
 */
export function shellFrom(out: Tokens, text: string, i: number, state: State, make = false): State {
  const n = text.length;
  // A HEREDOC'S BODY: every line string, to the one that is its delimiter.
  if (state[0] === "h" && (state[1] === "-" || state[1] === "_") && state.length > 2) {
    let at = i;
    if (state[1] === "-") while (text[at] === "\t") at++;
    out.push(n, "string");
    return text.slice(at) === state.slice(2) ? "" : state;
  }
  let open = "";
  let heredoc = "";
  if (state === "'" || state === '"') {
    const end = until(text, i, state, state === '"');
    if (end < 0) {
      out.push(n, "string");
      return state;
    }
    out.push((i = end), "string");
  }
  let command = state !== "'" && state !== '"';
  let loop = false;
  let named = false;
  while (i < n) {
    const ch = text[i]!;
    const prev = text[i - 1];
    if (isSpace(ch)) {
      out.push((i = skipSpace(text, i)), null);
    } else if (ch === "#" && (prev === undefined || isSpace(prev) || isMeta(prev))) {
      out.push((i = n), "comment");
    } else if (ch === "'" || ch === '"') {
      const end = until(text, i + 1, ch, ch === '"');
      if (end < 0) {
        out.push((i = n), "string");
        open = ch;
      } else out.push((i = end), "string");
      command = false;
    } else if (ch === "$") {
      const next = text[i + 1];
      if (make && next === "$") {
        out.push((i += 1), "punctuation");
        continue;
      }
      if (next === "{") {
        const end = until(text, i + 2, "}");
        out.push((i = end < 0 ? n : end), "type");
        command = false;
      } else if (make && next === "(" && text[word(text, i + 2, NAME)] === ")") {
        const end = word(text, i + 2, NAME);
        const simple = end > i + 2;
        out.push((i = simple ? end + 1 : i + 2), simple ? "type" : "punctuation");
        command = !simple;
      } else if (next === "(") {
        out.push((i += 2), "punctuation");
        command = true;
      } else if (word(text, i + 1, NAME) > i + 1) {
        out.push((i = word(text, i + 1, NAME)), "type");
        command = false;
      } else if (isDigit(next) || (next !== undefined && "@*#?$!-".includes(next))) {
        out.push((i += 2), "type");
      } else if (make && next !== undefined && "<^%+".includes(next)) {
        out.push((i += 2), "type");
      } else out.push((i += 1), "punctuation");
    } else if (ch === "<" && text[i + 1] === "<" && text[i + 2] !== "<") {
      // A heredoc: its body is the lines after this one.
      let j = i + 2;
      const dash = text[j] === "-";
      if (dash) j++;
      out.push(j, "punctuation");
      j = skipSpace(text, j);
      out.push(j, null);
      const quote = text[j] === "'" || text[j] === '"' ? text[j]! : "";
      const start = j + quote.length;
      const end = word(text, start, DELIMITER);
      if (end > start && (!quote || text[end] === quote)) {
        out.push(end + quote.length, "string");
        if (!heredoc && end - start <= MAX_DELIMITER) {
          heredoc = `h${dash ? "-" : "_"}${text.slice(start, end)}`;
        }
        i = end + quote.length;
      } else i = j;
    } else if (ch === "\\") {
      out.push((i = i + 1 < n ? step(text, i + 1) : n), null);
    } else if (isMeta(ch)) {
      out.push((i += 1), "punctuation");
      command = ch !== ">" && ch !== "<" && ch !== ")" && ch !== "]";
    } else {
      let end = i;
      while (!ends(text[end])) end = step(text, end);
      const w = text.slice(i, end);
      let kind: "keyword" | "function" | "number" | null = null;
      if (command && KEYWORDS.has(w)) kind = "keyword";
      else if (w === "in" && loop) kind = "keyword";
      else if (command && BUILTINS.has(w)) kind = "function";
      else if (named || (command && text.startsWith("()", skipSpace(text, end)))) kind = "function";
      else if (isDigit(w[0]) && word(w, 0, DIGITS) === w.length) kind = "number";
      out.push((i = end), kind);
      named = kind === "keyword" && w === "function";
      if (kind === "keyword") loop = w === "for" || w === "case" || w === "select";
      command = kind === "keyword" ? OPENS.has(w) : command && w.includes("=");
    }
  }
  return open || heredoc;
}

export const shell: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const next = shellFrom(out, text, 0, state);
    return done(out, next);
  },
};
