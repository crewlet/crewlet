/** JSON, with JSONC's comments; a state carries an open block comment. */
import { Tokens, done, isSpace, skipSpace, step, until, word } from "./scan.ts";
import type { Grammar, Line, State } from "./types.ts";

const NUMBER = /-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?/y;
const WORD = /[A-Za-z_$][\w$]*/y;

export const json: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    let i = 0;
    let open = "";
    if (state === "*") {
      const end = until(text, 0, "*/");
      if (end < 0) {
        out.push(n, "comment");
        return done(out, "*");
      }
      out.push((i = end), "comment");
    }
    while (i < n) {
      const ch = text[i]!;
      if (isSpace(ch)) out.push((i = skipSpace(text, i)), null);
      else if (text.startsWith("//", i)) out.push((i = n), "comment");
      else if (text.startsWith("/*", i)) {
        const end = until(text, i + 2, "*/");
        if (end < 0) open = "*";
        out.push((i = end < 0 ? n : end), "comment");
      } else if (ch === '"') {
        const end = until(text, i + 1, '"', true);
        if (end < 0) out.push((i = n), "string");
        else out.push((i = end), text[skipSpace(text, end)] === ":" ? "function" : "string");
      } else if (ch === "-" || (ch >= "0" && ch <= "9")) {
        const end = word(text, i, NUMBER);
        const kind = end > i ? "number" : "punctuation";
        out.push((i = Math.max(end, i + 1)), kind);
      } else if ("{}[]:,".includes(ch)) out.push((i += 1), "punctuation");
      else {
        const end = word(text, i, WORD);
        if (end > i) {
          const w = text.slice(i, end);
          const kind = w === "true" || w === "false" || w === "null" ? "keyword" : null;
          out.push((i = end), kind);
        } else out.push((i = step(text, i)), null);
      }
    }
    return done(out, open);
  },
};
