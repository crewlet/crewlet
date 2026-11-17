/**
 * Makefiles, their recipes read as shell; a state carries a `define` body
 * (`d`) or a recipe that goes on (`r` and the shell's own state).
 */
import { Tokens, done, isSpace, skipSpace, word, words } from "./scan.ts";
import { shellFrom } from "./lang-shell.ts";
import type { Grammar, Line, State } from "./types.ts";

const DIRECTIVES = words(
  "include -include sinclude ifeq ifneq ifdef ifndef else endif define endef export override unexport vpath",
);
const NAME = /[\w.-]+/y;
const DIRECTIVE = /-?[a-z]+/y;

/** A value from `i`: variables as types, a function's name as a call, `#` on as a comment. */
function values(out: Tokens, text: string, i: number): void {
  const n = text.length;
  while (i < n) {
    const ch = text[i]!;
    if (isSpace(ch)) out.push((i = skipSpace(text, i)), null);
    else if (ch === "#") out.push((i = n), "comment");
    else if (ch === "$" && (text[i + 1] === "(" || text[i + 1] === "{")) {
      const close = text[i + 1] === "(" ? ")" : "}";
      const end = word(text, i + 2, NAME);
      if (end > i + 2 && text[end] === close) out.push((i = end + 1), "type");
      else {
        out.push(i + 2, "punctuation");
        if (end > i + 2) out.push(end, "function");
        i = Math.max(end, i + 2);
      }
    } else if (ch === "$" && i + 1 < n) out.push((i += 2), "type");
    else if ("(){}|:;=,".includes(ch)) out.push((i += 1), "punctuation");
    else {
      let end = i + 1;
      while (end < n && !isSpace(text[end]) && !"$#(){}|:;=,".includes(text[end]!)) end++;
      out.push((i = end), null);
    }
  }
}

/** Where a line's assignment operator or target colon is, before any `#`, outside `$(...)`. */
function head(text: string, from: number): { at: number; op: string } | null {
  let depth = 0;
  for (let j = from; j < text.length; j++) {
    const ch = text[j];
    if (ch === "#") return null;
    if (ch === "$" && (text[j + 1] === "(" || text[j + 1] === "{")) {
      depth++;
      j++;
    } else if ((ch === ")" || ch === "}") && depth > 0) depth--;
    else if (depth === 0 && ch === "=") {
      const before = text[j - 1];
      if (before === "?" || before === "+" || before === "!")
        return { at: j - 1, op: before + "=" };
      if (before === ":") {
        const at = text[j - 2] === ":" ? j - 2 : j - 1;
        return { at, op: text.slice(at, j + 1) };
      }
      return { at: j, op: "=" };
    } else if (depth === 0 && ch === ":" && text[j + 1] !== "=") {
      if (text[j + 1] === ":" && text[j + 2] === "=") continue;
      return { at: j, op: ":" };
    }
  }
  return null;
}

export const makefile: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    if (state === "d") {
      const at = skipSpace(text, 0);
      if (text.startsWith("endef", at) && (at + 5 === n || isSpace(text[at + 5]))) {
        out.push(at, null);
        out.push(at + 5, "keyword");
        values(out, text, at + 5);
        return done(out, "");
      }
      out.push(n, "string");
      return done(out, "d");
    }
    // A RECIPE: a line under a rule that starts with a tab, or one a recipe's `\` goes on to.
    if (state[0] === "r" || text[0] === "\t") {
      const shell = shellFrom(out, text, 0, state[0] === "r" ? state.slice(1) : "", true);
      const goesOn = text.trimEnd().endsWith("\\");
      return done(out, goesOn || shell ? `r${shell}` : "");
    }
    let i = skipSpace(text, 0);
    out.push(i, null);
    const directive = word(text, i, DIRECTIVE);
    if (
      directive > i &&
      DIRECTIVES.has(text.slice(i, directive)) &&
      (directive === n || isSpace(text[directive]) || text[directive] === "(")
    ) {
      const name = text.slice(i, directive);
      out.push(directive, "keyword");
      if (name === "define") {
        values(out, text, directive);
        return done(out, "d");
      }
      if (name !== "export" && name !== "override") {
        values(out, text, directive);
        return done(out, "");
      }
      i = directive;
    }
    const found = head(text, i);
    if (found) {
      // A rule's targets are named as definitions; an assignment's name as a variable.
      const kind = found.op === ":" ? "function" : "type";
      let j = i;
      while (j < found.at) {
        if (isSpace(text[j])) out.push((j = skipSpace(text, j)), null);
        else {
          const start = j;
          while (j < found.at && !isSpace(text[j])) j++;
          out.push(j, text[start] === "$" ? "type" : kind);
        }
      }
      out.push(found.at + found.op.length, "punctuation");
      i = found.at + found.op.length;
    }
    values(out, text, i);
    return done(out, "");
  },
};
