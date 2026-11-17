/**
 * TypeScript and JavaScript with JSX; a state carries the template stack
 * (a template's text and each `${` and brace in one) and a block comment.
 */
import { MAX_STATE, Scan, clike, done, isSpace, skipSpace, until, word } from "./scan.ts";
import type { Grammar, Line, State } from "./types.ts";

/** The deepest template stack carried; a frame past it is not pushed. */
const MAX_STACK = 16;
const IDENT = /[A-Za-z_$][\w$]*/y;
const TAG = /[A-Za-z_$][\w$.:-]*/y;
const ATTR = /[A-Za-z_$][\w$:-]*/y;
const FLAGS = /[a-z]*/y;

const push = (s: Scan, frame: string): boolean => {
  if (s.stack.length >= MAX_STACK) return false;
  s.stack += frame;
  return true;
};
const top = (s: Scan) => s.stack[s.stack.length - 1];
const pop = (s: Scan) => {
  s.stack = s.stack.slice(0, -1);
};

/** A template's text from `i`, to its close, its next `${` or the line's end. */
function templateText(s: Scan, i: number, framed: boolean): number {
  const text = s.text;
  const n = text.length;
  while (i < n) {
    const ch = text[i];
    if (ch === "\\") i += 2;
    else if (ch === "`") {
      if (framed) pop(s);
      return s.emit(i + 1, "string", true);
    } else if (framed && ch === "$" && text[i + 1] === "{" && push(s, "$")) {
      s.out.push(i, "string");
      return s.emit(i + 2, "punctuation", false);
    } else i++;
  }
  s.out.push(n, "string");
  return n;
}

/** A regex literal from its `/`; one the line does not close runs to its end. */
function regex(s: Scan, i: number): number {
  const text = s.text;
  let inClass = false;
  for (let j = i + 1; j < text.length; j++) {
    const ch = text[j];
    if (ch === "\\") j++;
    else if (ch === "[") inClass = true;
    else if (ch === "]") inClass = false;
    else if (ch === "/" && !inClass) return s.emit(word(text, j + 1, FLAGS), "string", true);
  }
  return s.emit(text.length, "string", true);
}

/** A JSX tag's attributes from `i`, to its `>`, an expression's `{` or the line's end. */
function tagRest(s: Scan, i: number): number {
  const text = s.text;
  const n = text.length;
  while (i < n) {
    const ch = text[i]!;
    if (isSpace(ch)) {
      i = skipSpace(text, i);
      s.out.push(i, null);
    } else if (ch === ">") {
      return s.emit(i + 1, "punctuation", false);
    } else if (ch === "/" && text[i + 1] === ">") {
      return s.emit(i + 2, "punctuation", false);
    } else if (ch === "=") {
      i = s.emit(i + 1, "punctuation", false);
    } else if (ch === '"' || ch === "'") {
      const end = until(text, i + 1, ch);
      i = s.emit(end < 0 ? n : end, "string", true);
    } else if (ch === "{") {
      // Code until the matching `}`, which returns here ([braces]).
      if (push(s, "<")) return s.emit(i + 1, "punctuation", false);
      return i;
    } else {
      const end = word(text, i, ATTR);
      if (end === i) return i;
      i = s.emit(end, "function", false);
    }
  }
  return n;
}

/** A JSX tag from its `<`: the name, then its attributes. */
function tag(s: Scan, i: number): number {
  const text = s.text;
  let j = s.emit(i + 1, "punctuation", false);
  if (text[j] === "/") j = s.emit(j + 1, "punctuation", false);
  if (text[j] === ">") return s.emit(j + 1, "punctuation", false);
  const end = word(text, j, TAG);
  if (end > j) j = s.emit(end, "keyword", false);
  return tagRest(s, j);
}

const isLetter = (c: string | undefined) =>
  c !== undefined && ((c >= "a" && c <= "z") || (c >= "A" && c <= "Z"));

const base = clike({
  keywords:
    "abstract accessor as asserts async await break case catch class const continue debugger declare default delete do else enum export extends finally for from function get if implements import in infer instanceof interface is keyof let namespace new of override private protected public readonly return satisfies set static switch throw try type typeof var while with yield",
  constants: "true false null undefined this super",
  types: "string number boolean any unknown never void object symbol bigint",
  typeAfter: "class interface type enum extends implements new",
  functionAfter: "function",
  line: ["//"],
  block: ["/*", "*/"],
  quotes: `"'`,
  ident: IDENT,
  hooks: [
    (s, i) => {
      const ch = s.text[i];
      if (ch === "`") {
        s.out.push(i + 1, "string");
        return templateText(s, i + 1, push(s, "`"));
      }
      if (ch === "{" && s.stack !== "") {
        push(s, "{");
        return s.emit(i + 1, "punctuation", false);
      }
      if (ch === "}" && s.stack !== "") {
        const frame = top(s);
        pop(s);
        const end = s.emit(i + 1, "punctuation", true);
        if (frame === "$") return templateText(s, end, true);
        if (frame === "<") return tagRest(s, end);
        return end;
      }
      if (ch === "/" && !s.operand) {
        const next = s.text[i + 1];
        if (next !== "/" && next !== "*") return regex(s, i);
      }
      if (ch === "<") {
        const next = s.text[i + 1];
        if (next === "/" && (isLetter(s.text[i + 2]) || s.text[i + 2] === ">")) return tag(s, i);
        if (!s.operand && (isLetter(next) || next === ">")) return tag(s, i);
      }
      return -1;
    },
  ],
});

/** A state of this grammar: the template stack, then an open block comment. */
function parse(state: State): { stack: string; open: string } {
  let k = 0;
  while (k < state.length && "`${".includes(state[k]!)) k++;
  const open = state.slice(k);
  if (k > MAX_STACK || (open !== "" && open !== "*")) return { stack: "", open: "" };
  return { stack: state.slice(0, k), open };
}

export const ts: Grammar = {
  line(text: string, state: State): Line {
    const s = new Scan(text);
    const { stack, open } = parse(state);
    s.stack = stack;
    let i = 0;
    if (open) i = base.resume(s, open);
    else if (top(s) === "`") i = templateText(s, 0, true);
    while (i < text.length) i = base.scan(s, i);
    // A tag's expression ends with its line: only the template frames below it carry.
    const cut = s.stack.indexOf("<");
    const carried = (cut < 0 ? s.stack : s.stack.slice(0, cut)) + s.open;
    return done(s.out, carried.length <= MAX_STATE ? carried : "");
  },
};
