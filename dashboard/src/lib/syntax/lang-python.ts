/** Python; a state carries an open triple-quoted string. */
import { clike, isWordChar, word, type Multiline } from "./scan.ts";
import type { Grammar } from "./types.ts";

const TRIPLE: Multiline[] = [
  { open: '"""', close: '"""', escape: true },
  { open: "'''", close: "'''", escape: true },
];
const PREFIX = /(?:[rRbBuUfF]|[rR][bBfF]|[bBfF][rR])(?=["'])/y;
const DECORATOR = /@[A-Za-z_][\w.]*/y;

export const python: Grammar = clike({
  keywords:
    "and as assert async await break case class continue def del elif else except finally for from global if import in is lambda match nonlocal not or pass raise return try while with yield",
  constants: "True False None",
  types: "int str list dict set tuple bool float bytes object type frozenset complex",
  typeAfter: "class",
  functionAfter: "def",
  line: ["#"],
  quotes: `"'`,
  multiline: TRIPLE,
  hooks: [
    // A string's prefix (r, b, f, u, rb ...) is part of the string.
    (s, i) => {
      if (!"rRbBuUfF".includes(s.text[i]!) || isWordChar(s.text[i - 1])) return -1;
      const end = word(s.text, i, PREFIX);
      if (end === i) return -1;
      s.out.push(end, "string");
      return end;
    },
    (s, i) => {
      if (s.text[i] !== "@" || s.operand) return -1;
      const end = word(s.text, i, DECORATOR);
      return end > i + 1 ? s.emit(end, "function", true) : -1;
    },
  ],
});
