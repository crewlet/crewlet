/**
 * Dockerfiles, `RUN`, `CMD` and `ENTRYPOINT` read as shell; a state carries an
 * instruction a `\` goes on with (`o`), or a shell one with the shell's state (`r`).
 */
import { Tokens, done, isSpace, skipSpace, until, word, words } from "./scan.ts";
import { shellFrom } from "./lang-shell.ts";
import type { Grammar, Line, State } from "./types.ts";

const INSTRUCTIONS = words(
  "from run cmd label expose env add copy entrypoint volume user workdir arg onbuild stopsignal healthcheck shell maintainer",
);
const SHELL_FORM = words("run cmd entrypoint");
const NAME = /[A-Za-z_]\w*/y;
const INSTRUCTION = /[A-Za-z]+/y;

/** An instruction's arguments from `i`: quotes, variables, `AS`, and the exec form's brackets. */
function args(out: Tokens, text: string, i: number): void {
  const n = text.length;
  while (i < n) {
    const ch = text[i]!;
    if (isSpace(ch)) out.push((i = skipSpace(text, i)), null);
    else if (ch === '"' || ch === "'") {
      const end = until(text, i + 1, ch, true);
      out.push((i = end < 0 ? n : end), "string");
    } else if (ch === "$") {
      const end = text[i + 1] === "{" ? until(text, i + 2, "}") : word(text, i + 1, NAME);
      if (end > i + 1) out.push((i = end), "type");
      else if (end < 0) out.push((i = n), "type");
      else out.push((i += 1), "punctuation");
    } else if ("[],=\\".includes(ch)) out.push((i += 1), "punctuation");
    else {
      let end = i + 1;
      while (end < n && !isSpace(text[end]) && !"\"'$[],=\\".includes(text[end]!)) end++;
      const w = text.slice(i, end);
      out.push((i = end), w === "AS" || w === "as" ? "keyword" : null);
    }
  }
}

const goesOn = (text: string) => text.trimEnd().endsWith("\\");

/** A shell form's text from `i`, and the state it leaves. */
function shellForm(out: Tokens, text: string, i: number, state: State): State {
  const next = shellFrom(out, text, i, state);
  return goesOn(text) || next ? `r${next}` : "";
}

export const dockerfile: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    if (state[0] === "r") return done(out, shellForm(out, text, 0, state.slice(1)));
    if (state === "o") {
      args(out, text, 0);
      return done(out, goesOn(text) ? "o" : "");
    }
    const i = skipSpace(text, 0);
    out.push(i, null);
    if (text[i] === "#") {
      out.push(text.length, "comment");
      return done(out, "");
    }
    const end = word(text, i, INSTRUCTION);
    const name = text.slice(i, end).toLowerCase();
    if (end === i || !INSTRUCTIONS.has(name)) {
      args(out, text, i);
      return done(out, "");
    }
    out.push(end, "keyword");
    const at = skipSpace(text, end);
    if (SHELL_FORM.has(name) && text[at] !== "[") return done(out, shellForm(out, text, end, ""));
    args(out, text, end);
    return done(out, goesOn(text) ? "o" : "");
  },
};
