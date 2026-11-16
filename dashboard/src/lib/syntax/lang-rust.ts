/**
 * Rust; a state carries an open string, a raw string with its hashes, or the
 * depth of nested block comments.
 */
import { Scan, clike, isWordChar, step, until, word } from "./scan.ts";
import type { Grammar } from "./types.ts";

const LIFETIME = /[A-Za-z_]\w*/y;
/** The most hashes a raw string's state carries. */
const MAX_HASHES = 32;

/** A raw string's text from `i` to its `"` and `hashes` hashes, or the line's end. */
function raw(s: Scan, i: number, hashes: number): number {
  const end = until(s.text, i, '"' + "#".repeat(hashes));
  if (end >= 0) return s.emit(end, "string", true);
  s.out.push(s.text.length, "string");
  if (hashes <= MAX_HASHES) s.open = `R${hashes}`;
  return s.text.length;
}

export const rust: Grammar = clike({
  keywords:
    "as async await break const continue crate dyn else enum extern fn for if impl in let loop match mod move mut pub ref return static struct super trait type union unsafe use where while yield",
  constants: "true false self Self",
  types: "i8 i16 i32 i64 i128 isize u8 u16 u32 u64 u128 usize f32 f64 bool char str",
  typeAfter: "struct enum trait type union",
  functionAfter: "fn",
  capitalTypes: true,
  line: ["//"],
  block: ["/*", "*/"],
  nests: true,
  multiline: [{ open: '"', close: '"', escape: true }],
  hooks: [
    // A raw string, r"..." or r#"..."#, and a byte string's b.
    (s, i) => {
      const t = s.text;
      if (isWordChar(t[i - 1])) return -1;
      let j = i;
      if (t[j] === "b") {
        if (t[j + 1] === '"' || t[j + 1] === "'") {
          s.out.push(j + 1, "string");
          return j + 1;
        }
        j++;
      }
      if (t[j] !== "r") return -1;
      let hashes = 0;
      while (t[j + 1 + hashes] === "#") hashes++;
      if (t[j + 1 + hashes] !== '"') return -1;
      s.out.push(j + 2 + hashes, "string");
      return raw(s, j + 2 + hashes, hashes);
    },
    // A char ('c', '\n') or a lifetime ('a).
    (s, i) => {
      const t = s.text;
      if (t[i] !== "'") return -1;
      if (t[i + 1] === "\\") {
        const end = until(t, i + 1, "'", true);
        return s.emit(end < 0 ? t.length : end, "string", true);
      }
      if (i + 1 < t.length) {
        const after = step(t, i + 1);
        if (t[after] === "'") return s.emit(after + 1, "string", true);
      }
      const end = word(t, i + 1, LIFETIME);
      return end > i + 1 ? s.emit(end, "keyword", true) : s.emit(i + 1, "punctuation", false);
    },
  ],
  resume: (s, open) => {
    const hashes = Number(open.slice(1));
    if (open[0] !== "R" || String(hashes) !== open.slice(1) || hashes > MAX_HASHES) {
      return -1;
    }
    return raw(s, 0, hashes);
  },
  // A macro's name, `println!`, is drawn as a call.
  wordKind: (s, _start, end, kind) =>
    s.text[end] === "!" && s.text[end + 1] !== "=" ? "function" : kind,
});
