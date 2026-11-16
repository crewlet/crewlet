/** Go, and go.mod and go.work; a state carries a raw string or a block comment. */
import { Tokens, done, isSpace, skipSpace, until, words } from "./scan.ts";
import { clike } from "./scan.ts";
import type { Grammar, Kind, Line } from "./types.ts";

export const go: Grammar = clike({
  keywords:
    "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var",
  constants: "true false nil iota",
  types:
    "any bool byte comparable complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr",
  typeAfter: "type",
  functionAfter: "func",
  line: ["//"],
  block: ["/*", "*/"],
  quotes: `"'`,
  multiline: [{ open: "`", close: "`", escape: false }],
});

const MOD_WORDS = words("module go require replace exclude retract toolchain godebug tool ignore");
const VERSION = /v?\d[\d.]*(?:[-+][\w.+-]*)?/y;

/** go.mod and go.work: directives, module paths, versions and `//` comments. */
export const gomod: Grammar = {
  line(text: string): Line {
    const out = new Tokens();
    const n = text.length;
    let i = 0;
    while (i < n) {
      const ch = text[i]!;
      if (isSpace(ch)) {
        i = skipSpace(text, i);
        out.push(i, null);
      } else if (text.startsWith("//", i)) {
        out.push((i = n), "comment");
      } else if (ch === '"' || ch === "`") {
        const end = until(text, i + 1, ch, ch === '"');
        out.push((i = end < 0 ? n : end), "string");
      } else if (ch === "(" || ch === ")") {
        out.push(++i, "punctuation");
      } else if (text.startsWith("=>", i)) {
        out.push((i += 2), "punctuation");
      } else {
        // A run up to a space, a quote or a bracket: a directive, a version or a path.
        let end = i;
        while (end < n && !isSpace(text[end]) && !'"`()'.includes(text[end]!)) {
          if (text.startsWith("//", end) || text.startsWith("=>", end)) break;
          end++;
        }
        const w = text.slice(i, end);
        VERSION.lastIndex = 0;
        const version = VERSION.test(w) && VERSION.lastIndex === w.length;
        const kind: Kind | null = MOD_WORDS.has(w) ? "keyword" : version ? "number" : null;
        out.push((i = end), kind);
      }
    }
    return done(out, "");
  },
};
