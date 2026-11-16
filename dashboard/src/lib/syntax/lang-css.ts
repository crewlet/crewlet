/**
 * CSS; a state carries the brace depth (capped at 8), then an open block
 * comment (`*`) or a string a trailing `\` goes on with.
 */
import { Tokens, done, isDigit, isSpace, skipSpace, step, until, word } from "./scan.ts";
import type { Grammar, Kind, Line, State } from "./types.ts";

const MAX_DEPTH = 8;
const IDENT = /-{0,2}[A-Za-z_][\w-]*/y;
const NUMBER = /[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?(?:%|[A-Za-z]+)?/y;
const HASH = /#[\w-]+/y;

/** A state read back: the depth, and what it leaves open; an unknown one is the file's start. */
function parse(state: State): { depth: number; open: string } {
  const depth = isDigit(state[0]) ? Number(state[0]) : 0;
  const open = isDigit(state[0]) ? state.slice(1) : state;
  if (depth > MAX_DEPTH || (open !== "" && open !== "*" && open !== '"' && open !== "'")) {
    return { depth: 0, open: "" };
  }
  return { depth, open };
}

export const css: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    let { depth, open } = parse(state);
    let i = 0;
    const carry = () => done(out, (depth > 0 ? String(depth) : "") + open);
    if (open) {
      const close = open === "*" ? "*/" : open;
      const end = until(text, 0, close, open !== "*");
      out.push(end < 0 ? n : end, open === "*" ? "comment" : "string");
      if (end < 0) {
        if (open !== "*" && !text.endsWith("\\")) open = "";
        return carry();
      }
      i = end;
      open = "";
    }
    let declStart = depth > 0;
    let inValue = false;
    let prelude = false;
    let pseudo = false;
    while (i < n) {
      const ch = text[i]!;
      if (isSpace(ch)) {
        out.push((i = skipSpace(text, i)), null);
        continue;
      }
      if (text.startsWith("/*", i)) {
        const end = until(text, i + 2, "*/");
        out.push((i = end < 0 ? n : end), "comment");
        if (end < 0) open = "*";
        continue;
      }
      if (ch === '"' || ch === "'") {
        const end = until(text, i + 1, ch, true);
        out.push((i = end < 0 ? n : end), "string");
        if (end < 0 && text.endsWith("\\")) open = ch;
        continue;
      }
      if (ch === "@" || ch === "!") {
        const end = word(text, i + 1, IDENT);
        if (end > i + 1) {
          out.push((i = end), "keyword");
          if (ch === "@") prelude = true;
          continue;
        }
      }
      if (ch === "#") {
        const end = word(text, i, HASH);
        if (end > i + 1) {
          out.push((i = end), inValue || prelude ? "number" : "type");
          continue;
        }
      }
      if (ch === "." && !inValue && !prelude && !isDigit(text[i + 1])) {
        const end = word(text, i + 1, IDENT);
        if (end > i + 1) {
          out.push((i = end), "type");
          continue;
        }
      }
      if (
        isDigit(ch) ||
        ((ch === "." || ch === "-" || ch === "+") && (isDigit(text[i + 1]) || text[i + 1] === "."))
      ) {
        const end = word(text, i, NUMBER);
        if (end > i) {
          out.push((i = end), "number");
          continue;
        }
      }
      const end = word(text, i, IDENT);
      if (end > i) {
        const w = text.slice(i, end);
        const after = text[skipSpace(text, end)];
        let kind: Kind | null = null;
        if (text[end] === "(") {
          kind = "function";
          if (w === "url") {
            const close = until(text, end + 1, ")");
            out.push(end, kind);
            out.push(end + 1, "punctuation");
            const to = close < 0 ? n : close - 1;
            out.push(to, "string");
            i = to;
            continue;
          }
        } else if (depth > 0 && declStart && after === ":") kind = "function";
        else if (!inValue && !prelude && !pseudo) kind = "keyword";
        out.push((i = end), kind);
        declStart = false;
        pseudo = false;
        continue;
      }
      if (ch === "{") {
        depth = Math.min(depth + 1, MAX_DEPTH);
        declStart = true;
        inValue = prelude = false;
      } else if (ch === "}") {
        depth = Math.max(depth - 1, 0);
        declStart = true;
        inValue = false;
      } else if (ch === ";") {
        declStart = true;
        inValue = prelude = false;
      } else if (ch === ":") {
        if (depth > 0 && !inValue && out.list[out.list.length - 1]?.kind === "function")
          inValue = true;
        else pseudo = !inValue && !prelude;
      }
      if (step(text, i) === i + 1 && "{}:;,>+~()[]=*/|^$%&".includes(ch)) {
        out.push((i += 1), "punctuation");
      } else out.push((i = step(text, i)), null);
    }
    return carry();
  },
};
