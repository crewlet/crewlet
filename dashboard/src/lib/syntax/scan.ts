/**
 * The scanner most grammars are a configuration of. Linear: each step consumes
 * what it reads, and an unclosed construct runs to the line's end.
 */
import type { Grammar, Kind, Line, State, Token } from "./types.ts";

/** A line's tokens as they are pushed, adjacent runs of one kind merged. */
export class Tokens {
  readonly list: Token[] = [];
  /** Where the last token ends. */
  at = 0;
  push(end: number, kind: Kind | null): void {
    if (end <= this.at) return;
    const last = this.list[this.list.length - 1];
    if (last && last.kind === kind) last.end = end;
    else this.list.push({ end, kind });
    this.at = end;
  }
}

/** A line answered from its tokens and the state it leaves. */
export const done = (out: Tokens, state: State): Line => ({ tokens: out.list, state });

/** The longest state a grammar writes; a construct whose state would be longer is not carried. */
export const MAX_STATE = 64;

export const isSpace = (c: string | undefined) => c === " " || c === "\t";
export const isDigit = (c: string | undefined) => c !== undefined && c >= "0" && c <= "9";
export const isWordChar = (c: string | undefined) =>
  c !== undefined &&
  ((c >= "a" && c <= "z") || (c >= "A" && c <= "Z") || (c >= "0" && c <= "9") || c === "_");

/** The end of the run of spaces and tabs at `i`. */
export function skipSpace(text: string, i: number): number {
  while (isSpace(text[i])) i++;
  return i;
}

/** One code point on from `i`, so no token ends inside a surrogate pair. */
export function step(text: string, i: number): number {
  const c = text.charCodeAt(i);
  if (c >= 0xd800 && c <= 0xdbff) {
    const d = text.charCodeAt(i + 1);
    if (d >= 0xdc00 && d <= 0xdfff) return i + 2;
  }
  return i + 1;
}

const WORD = /[A-Za-z_][\w]*/y;

/** The end of the word matched at `i` by a sticky `pattern`, or `i` when none starts there. */
export function word(text: string, i: number, pattern: RegExp = WORD): number {
  pattern.lastIndex = i;
  return pattern.test(text) ? pattern.lastIndex : i;
}

/**
 * The end of `close` from `i` on, a backslash escaping the unit after it when
 * `escape`; -1 when the line ends first.
 */
export function until(text: string, i: number, close: string, escape = false): number {
  const n = text.length;
  while (i < n) {
    if (escape && text[i] === "\\") {
      i += 2;
      continue;
    }
    if (text.startsWith(close, i)) return i + close.length;
    i++;
  }
  return -1;
}

const NUMBER =
  /(?:0[xX][\da-fA-F_]+|0[bB][01_]+|0[oO][0-7_]+|(?:\d[\d_]*(?:\.\d[\d_]*)?|\.\d[\d_]*)(?:[eE][+-]?\d[\d_]*)?)[A-Za-z\d_]*/y;

/** The punctuation every grammar draws in its ink. */
export const isPunct = (c: string | undefined) =>
  c !== undefined && "[]{}()<>.,;:=+-*/%!&|^~?@#$\\".includes(c);

/** A word set from a space-separated list. */
export const words = (list = ""): ReadonlySet<string> => new Set(list.split(" ").filter(Boolean));

/** One line's scan, which hooks read and push to. */
export class Scan {
  readonly out = new Tokens();
  /** The construct the line leaves open, written by whoever opens it. */
  open = "";
  /** A grammar's own frames that live across lines (TS's template stack). */
  stack = "";
  /** The last significant word, so a word after `type` or `func` is named. */
  prevWord = "";
  /** Whether the last significant token ends an operand (a word, a literal, `)`). */
  operand = false;
  readonly text: string;
  constructor(text: string) {
    this.text = text;
  }
  /** Push a token that ends here and is significant. */
  emit(end: number, kind: Kind | null, operand: boolean): number {
    this.out.push(end, kind);
    this.prevWord = "";
    this.operand = operand;
    return end;
  }
}

/** A grammar's own form tried before the defaults: the position after it, or -1. */
export type Hook = (s: Scan, i: number) => number;

/** A quote that may span lines, and whether a backslash escapes in it. */
export interface Multiline {
  open: string;
  close: string;
  escape: boolean;
}

export interface CLike {
  /** Space-separated reserved words: keyword kind. */
  keywords: string;
  /** Literal constants (true false nil ...): keyword kind. */
  constants?: string;
  /** Builtin type names: type kind. */
  types?: string;
  /** Words whose next identifier is a type (type, struct, class, enum ...). */
  typeAfter?: string;
  /** Words whose next identifier is a function (func, fn, def, rpc). */
  functionAfter?: string;
  /** An identifier starting A-Z is a type (Rust, Protobuf). */
  capitalTypes?: boolean;
  /** Words match in any case (SQL). */
  caseless?: boolean;
  /** Line comment openers. */
  line?: string[];
  /** A block comment, carried in the state as `*` (one a depth when it nests). */
  block?: [string, string];
  /** Nested block comments, their depth capped at 8. */
  nests?: boolean;
  /** Single-line quotes, each closing itself, a backslash escaping. */
  quotes?: string;
  /** Quotes that may span lines, carried as `Q<index>`. */
  multiline?: Multiline[];
  /** A sticky number pattern in place of the default. */
  number?: RegExp;
  /** A sticky identifier pattern in place of `[A-Za-z_]\w*`. */
  ident?: RegExp;
  /** A grammar's own forms tried before the defaults. */
  hooks?: Hook[];
  /** A grammar's own open construct continued at a line's start: the position after it, or -1 if unknown. */
  resume?: (s: Scan, open: string) => number;
  /** The kind of a word just scanned, given the chance to differ (a macro's `!`). */
  wordKind?: (s: Scan, start: number, end: number, kind: Kind | null) => Kind | null;
}

const MAX_DEPTH = 8;

/** A configuration's grammar, plus its step and its resume, which TS's own line reuses. */
export interface CLikeGrammar extends Grammar {
  scan(s: Scan, i: number): number;
  resume(s: Scan, open: string): number;
}

export function clike(c: CLike): CLikeGrammar {
  const keywords = words(c.keywords);
  const constants = words(c.constants);
  // Each known word's kind, read once a word: keywords and constants, then types.
  const known = new Map<string, Kind>();
  for (const w of words(c.types)) known.set(w, "type");
  for (const w of [...keywords, ...constants]) known.set(w, "keyword");
  const hooks = c.hooks ?? [];
  const typeAfter = words(c.typeAfter);
  const functionAfter = words(c.functionAfter);
  const lineOpeners = c.line ?? [];
  const multiline = c.multiline ?? [];
  const quotes = c.quotes ?? "";
  const number = c.number ?? NUMBER;
  const ident = c.ident ?? WORD;

  /** The rest of a block comment from `i`, at `depth` open; leaves it open at the line's end. */
  function comment(s: Scan, i: number, depth: number): number {
    const [open, close] = c.block!;
    const n = s.text.length;
    while (i < n) {
      if (c.nests && s.text.startsWith(open, i)) {
        depth = Math.min(depth + 1, MAX_DEPTH);
        i += open.length;
      } else if (s.text.startsWith(close, i)) {
        i += close.length;
        if (--depth === 0) return s.emit(i, "comment", false);
      } else i++;
    }
    s.out.push(n, "comment");
    s.open = "*".repeat(depth);
    return n;
  }

  /** The rest of a multi-line quote from `i`; leaves it open at the line's end. */
  function rest(s: Scan, i: number, q: number): number {
    const m = multiline[q]!;
    const end = until(s.text, i, m.close, m.escape);
    if (end >= 0) return s.emit(end, "string", true);
    s.out.push(s.text.length, "string");
    s.open = `Q${q}`;
    return s.text.length;
  }

  function scan(s: Scan, i: number): number {
    const text = s.text;
    const ch = text[i]!;
    for (const hook of hooks) {
      const end = hook(s, i);
      if (end > i) return end;
    }
    if (isSpace(ch)) {
      const end = skipSpace(text, i);
      s.out.push(end, null);
      return end;
    }
    for (const opener of lineOpeners) {
      if (text.startsWith(opener, i)) {
        s.out.push(text.length, "comment");
        return text.length;
      }
    }
    if (c.block && text.startsWith(c.block[0], i)) return comment(s, i + c.block[0].length, 1);
    for (let q = 0; q < multiline.length; q++) {
      if (text.startsWith(multiline[q]!.open, i)) return rest(s, i + multiline[q]!.open.length, q);
    }
    if (quotes.includes(ch)) {
      const end = until(text, i + 1, ch, true);
      return s.emit(end < 0 ? text.length : end, "string", true);
    }
    if (isDigit(ch) || (ch === "." && isDigit(text[i + 1]))) {
      const end = word(text, i, number);
      if (end > i) return s.emit(end, "number", true);
    }
    const end = word(text, i, ident);
    if (end > i) {
      const w = text.slice(i, end);
      const key = c.caseless ? w.toLowerCase() : w;
      let kind: Kind | null = known.get(key) ?? null;
      if (kind === null) {
        if (typeAfter.has(s.prevWord)) kind = "type";
        else if (functionAfter.has(s.prevWord)) kind = "function";
        else if (text[skipSpace(text, end)] === "(") kind = "function";
        else if (c.capitalTypes && ch >= "A" && ch <= "Z") kind = "type";
      }
      // A member after a dot is never a keyword: `x.type` is a property.
      if (kind === "keyword" && text[i - 1] === "." && text[i - 2] !== ".") {
        kind = text[skipSpace(text, end)] === "(" ? "function" : null;
      }
      if (c.wordKind) kind = c.wordKind(s, i, end, kind);
      s.out.push(end, kind);
      s.operand = !keywords.has(key) || constants.has(key);
      s.prevWord = key;
      return end;
    }
    if (isPunct(ch)) return s.emit(i + 1, "punctuation", ch === ")" || ch === "]" || ch === "}");
    const next = step(text, i);
    s.out.push(next, null);
    return next;
  }

  /** The open construct continued from a line's start, or 0 when the state is unknown. */
  function resume(s: Scan, open: string): number {
    if (open === "") return 0;
    if (c.block && open.length <= MAX_DEPTH && "*".repeat(open.length) === open) {
      return comment(s, 0, c.nests ? open.length : 1);
    }
    if (open[0] === "Q") {
      const q = Number(open.slice(1));
      if (open.length === 2 && q >= 0 && q < multiline.length) return rest(s, 0, q);
    }
    if (c.resume) {
      const end = c.resume(s, open);
      if (end >= 0) return end;
    }
    return 0;
  }

  return {
    scan,
    resume,
    line(text: string, state: State): Line {
      const s = new Scan(text);
      let i = resume(s, state);
      while (i < text.length) i = scan(s, i);
      return done(s.out, s.stack + s.open);
    },
  };
}
