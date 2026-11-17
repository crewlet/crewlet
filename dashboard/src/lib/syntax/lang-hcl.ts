/** HCL and Terraform; a state carries a heredoc's tag (`hEOT`) or a block comment. */
import { Scan, clike, skipSpace, word } from "./scan.ts";
import type { Grammar } from "./types.ts";

const TAG = /[A-Za-z_]\w*/y;
/** The longest heredoc tag carried. */
const MAX_TAG = 40;

/** A heredoc's body line: string, and the tag's own line closes it. */
function body(s: Scan, tag: string): number {
  s.out.push(s.text.length, "string");
  s.open = s.text.trim() === tag ? "" : `h${tag}`;
  return s.text.length;
}

/** A string from its `"`, its `${...}` kept inside it however it nests quotes. */
function string(s: Scan, i: number): number {
  const t = s.text;
  let depth = 0;
  for (let j = i + 1; j < t.length; j++) {
    const ch = t[j];
    if (ch === "\\") j++;
    else if ((ch === "$" || ch === "%") && t[j + 1] === "{") {
      depth++;
      j++;
    } else if (ch === "}" && depth > 0) depth--;
    else if (ch === '"' && depth === 0) return s.emit(j + 1, "string", true);
  }
  return s.emit(t.length, "string", true);
}

export const hcl: Grammar = clike({
  keywords:
    "resource data variable output module provider terraform locals moved import check removed for in if else endif endfor",
  constants: "true false null",
  types: "string number bool list map set object tuple any",
  line: ["#", "//"],
  block: ["/*", "*/"],
  hooks: [
    (s, i) => (s.text[i] === '"' ? string(s, i) : -1),
    (s, i) => {
      const t = s.text;
      if (!t.startsWith("<<", i)) return -1;
      const start = t[i + 2] === "-" ? i + 3 : i + 2;
      const end = word(t, start, TAG);
      if (end === start) return -1;
      s.out.push(start, "punctuation");
      s.out.push(end, "string");
      if (end - start <= MAX_TAG) s.open = `h${t.slice(start, end)}`;
      return end;
    },
  ],
  resume: (s, open) =>
    open[0] === "h" && open.length > 1 && word(open, 1, TAG) === open.length
      ? body(s, open.slice(1))
      : -1,
  // An attribute's name, `name =`, is drawn as a key.
  wordKind: (s, _start, end, kind) => {
    const at = skipSpace(s.text, end);
    return s.text[at] === "=" && s.text[at + 1] !== "=" && kind === null ? "function" : kind;
  },
});
