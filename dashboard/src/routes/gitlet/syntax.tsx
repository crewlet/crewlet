/**
 * Code coloured by kind in Gitlet: each token's class reads the kit's syntax
 * ink (`styles/gitlet-code.css`), the seventh exception.
 */
import {
  Fragment,
  memo,
  useDeferredValue,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  SYNTAX_MAX_BYTES,
  SYNTAX_MAX_LINE,
  language,
  languageOfInfo,
  load,
  loaded,
  statesOf,
  type Lang,
} from "~/lib/syntax/index.ts";
import { KINDS, type Grammar, type Kind, type State, type Token } from "~/lib/syntax/types.ts";
import { codeRest } from "~/lib/useCodeQuery.ts";
import type { DiffSpan } from "~/lib/wordDiff.ts";
import type { DiffHunk } from "~/components/DiffView.tsx";
import type { CodeBlobAnswer, CodeDiffFile } from "~/protocol/index.ts";
import "~/styles/gitlet-code.css";

/** The one place a token's class is written (`styles/codehost.test.ts`). */
function classOf(kind: Kind): string {
  switch (kind) {
    case "keyword":
      return "code-syn-keyword";
    case "string":
      return "code-syn-string";
    case "number":
      return "code-syn-number";
    case "function":
      return "code-syn-function";
    case "type":
      return "code-syn-type";
    case "comment":
      return "code-syn-comment";
    case "punctuation":
      return "code-syn-punctuation";
  }
}

/** Each kind's token class. */
export const SYNTAX_CLASS = Object.fromEntries(KINDS.map((k) => [k, classOf(k)])) as Readonly<
  Record<Kind, string>
>;

/** A line's text with its tokens as spans; its plain text where there are none. */
export const Painted = memo(function Painted({
  text,
  tokens,
}: {
  text: string;
  tokens: readonly Token[] | null;
}): ReactNode {
  if (!tokens || tokens.length === 0) return text;
  let at = 0;
  return tokens.map((t) => {
    const piece = text.slice(at, t.end);
    const start = at;
    at = t.end;
    return t.kind ? (
      <span key={start} className={SYNTAX_CLASS[t.kind]}>
        {piece}
      </span>
    ) : (
      piece
    );
  });
});

/** A line's tokens, or null: no grammar or state yet, or a line past SYNTAX_MAX_LINE. */
export function tokensOf(
  grammar: Grammar | null,
  text: string,
  state: State | null,
): readonly Token[] | null {
  if (!grammar || state === null || text.length > SYNTAX_MAX_LINE) return null;
  return grammar.line(text, state).tokens;
}

/** The grammar for a path once its chunk has arrived, or null (plain text, or still loading). */
export function useGrammar(path: string): Grammar | null {
  return useLangGrammar(language(path));
}

/** A language's grammar once its chunk has arrived, or null (none, or still loading). */
export function useLangGrammar(lang: Lang | null): Grammar | null {
  const [arrived, setArrived] = useState<{ lang: string; grammar: Grammar } | null>(null);
  useEffect(() => {
    if (!lang || loaded(lang)) return;
    let alive = true;
    // A chunk that fails to arrive leaves the lines plain; the next mount asks again.
    load(lang).then(
      (grammar) => alive && setArrived({ lang, grammar }),
      () => undefined,
    );
    return () => {
      alive = false;
    };
  }, [lang]);
  if (!lang) return null;
  return loaded(lang) ?? (arrived?.lang === lang ? arrived.grammar : null);
}

const utf8 = new TextEncoder();

/** A Markdown fence coloured in its language: `md-code`'s box, its text unchanged. */
export function PaintedFence({ text, lang, info }: { text: string; lang: Lang; info: string }) {
  const grammar = useLangGrammar(lang);
  const lines = useMemo(() => text.split("\n"), [text]);
  const small = useMemo(() => utf8.encode(text).length < SYNTAX_MAX_BYTES, [text]);
  // THE PLAIN PAINT FIRST, as a file's: the walk reads the deferred grammar.
  const deferred = useDeferredValue(small ? grammar : null, null);
  const tokens = useMemo(() => {
    if (!deferred) return null;
    const states = statesOf(deferred, lines, "");
    return lines.map((line, i) => tokensOf(deferred, line, states[i]!));
  }, [deferred, lines]);
  return (
    <pre className="md-code" data-lang={info}>
      <code>
        {lines.map((line, i) => (
          <Fragment key={i}>
            {i > 0 && "\n"}
            <Painted text={line} tokens={tokens?.[i] ?? null} />
          </Fragment>
        ))}
      </code>
    </pre>
  );
}

/** `renderMarkdown`'s fence hook: a fence in a known language coloured, any other left to `md-code`. */
export function paintFence(text: string, info: string, key: string): ReactNode {
  const lang = languageOfInfo(info);
  return lang ? <PaintedFence key={key} text={text} lang={lang} info={info} /> : null;
}

/** A blob's lines read by its whole id, as far as they have been asked. */
export interface BlobLines {
  /** Lines 1 to the line asked through (all of them with none), or null until read. */
  lines: readonly string[] | null;
  /** The blob's size, from its first page. */
  size: number | null;
  /** The blob is at or past SYNTAX_MAX_BYTES, or binary: only its first page was read. */
  over: boolean;
  failed: boolean;
}

/** What one blob's reads hold: its lines from line 1, and how far they go. */
interface Held {
  oid: string;
  /** The last line read; Infinity once the whole blob is. */
  through: number;
  lines: string[];
  size: number | null;
  over: boolean;
  failed: boolean;
}

/** A page's lines once joined: the newline that ends the last splits into no line. */
function linesOf(text: string): string[] {
  const split = text.split("\n");
  if (split.length > 0 && split[split.length - 1] === "") split.pop();
  return split;
}

/**
 * A blob's lines from line 1 through `through` (the whole blob with none), read
 * by its whole id with no path, which the engine marks immutable. Lines read
 * once are kept: a smaller `through` asks nothing, a larger one reads on.
 */
export function useBlobLines(
  repo: string,
  oid: string | null,
  { through, enabled = true }: { through?: number; enabled?: boolean } = {},
): BlobLines {
  const want = through ?? Infinity;
  const [held, setHeld] = useState<Held | null>(null);
  const now = held && held.oid === oid ? held : null;
  const kept = useRef(now);
  kept.current = now;
  useEffect(() => {
    const at = kept.current;
    if (!enabled || oid === null || want < 1) return;
    if (at && (at.over || at.failed || at.through >= want)) return;
    const base: Held = at ?? { oid, through: 0, lines: [], size: null, over: false, failed: false };
    let alive = true;
    void (async () => {
      let text = "";
      let size = base.size;
      try {
        let cursor = "";
        do {
          const answer = await codeRest.query("code_blob", {
            name: repo,
            ref: oid,
            start_line: base.through + 1,
            ...(Number.isFinite(want) ? { end_line: want } : {}),
            ...(cursor ? { cursor } : {}),
          });
          const page = answer.blob;
          if (!page) throw new Error("not a blob");
          size = page.size;
          // PAST THE BOUND, OR BINARY: its first page says so, and nothing more is read.
          if (page.binary || page.size >= SYNTAX_MAX_BYTES) {
            if (alive) setHeld({ ...base, size, over: true });
            return;
          }
          text += page.text ?? "";
          cursor = answer.next ?? "";
        } while (cursor);
      } catch {
        if (alive) setHeld({ ...base, size, failed: true });
        return;
      }
      if (alive)
        setHeld({ ...base, lines: [...base.lines, ...linesOf(text)], size, through: want });
    })();
    return () => {
      alive = false;
    };
  }, [enabled, oid, repo, want]);
  // ONE ARRAY UNTIL A READ LANDS, so a render that read nothing walks nothing.
  const lines = useMemo(
    () =>
      now && !now.over && !now.failed && now.through >= want
        ? Number.isFinite(want)
          ? now.lines.slice(0, want)
          : now.lines
        : null,
    [now, want],
  );
  return {
    lines,
    size: now?.size ?? null,
    over: now?.over ?? false,
    failed: now?.failed ?? false,
  };
}

/** No lines: what a view draws plain from. */
const NOTHING = { oid: "", grammar: null, from: 1, lines: [] as readonly string[], prefix: null };

/**
 * The state each drawn line opens in, by its number, or null while unknown:
 * walked from line 1 over `lines` when they start there, else after the
 * blob's lines above them. The walk runs in a transition, after the plain paint.
 */
export function useLineStates(args: {
  repo: string;
  blob: CodeBlobAnswer | null;
  grammar: Grammar | null;
  from: number;
  lines: readonly string[];
}): (line: number) => State | null {
  const { repo, blob, grammar, from, lines } = args;
  const colours =
    grammar !== null &&
    blob !== null &&
    !blob.binary &&
    blob.size < SYNTAX_MAX_BYTES &&
    lines.length > 0;
  const prefix = useBlobLines(repo, blob?.oid ?? null, {
    through: from - 1,
    enabled: colours && from > 1,
  });
  const oid = blob?.oid ?? null;
  const input = useMemo(
    () =>
      colours && oid !== null && (from === 1 || prefix.lines)
        ? { oid, grammar, from, lines, prefix: prefix.lines }
        : NOTHING,
    [colours, oid, grammar, from, lines, prefix.lines],
  );
  // THE PLAIN PAINT FIRST: the walk reads the deferred input, so a long file's
  // first paint and a press are never held by it.
  const deferred = useDeferredValue(input, NOTHING);
  const walked = useMemo(() => {
    const { grammar, prefix, lines } = deferred;
    if (!grammar) return null;
    let start: State = "";
    if (prefix) for (const line of prefix) start = grammar.line(line, start).state;
    return statesOf(grammar, lines, start);
  }, [deferred]);
  // A LINE'S STATE IS THE FILE'S, whichever lines are drawn, so one the walk
  // has not reached yet is plain rather than coloured from another's.
  return useMemo(
    () => (line: number) => {
      if (!walked || deferred.oid !== oid) return null;
      const k = line - deferred.from;
      return k >= 0 && k < deferred.lines.length ? walked[k]! : null;
    },
    [walked, deferred, oid],
  );
}

/** A diff's side, as DiffView asks for a line's spans. */
type Side = "before" | "after";

/** Each side's patch lines by number: unchanged and removed before, unchanged and added after. */
function patchLines(hunks: readonly DiffHunk[], side: Side): Map<number, string> {
  const out = new Map<number, string>();
  for (const hunk of hunks) {
    for (const line of hunk.lines) {
      const n = side === "before" ? line.before : line.after;
      if (n !== null && line.path === undefined) out.set(n, line.text);
    }
  }
  return out;
}

/** Each side's line states by number, each hunk walked from "" over that side's lines. */
function hunkStates(grammar: Grammar, hunks: readonly DiffHunk[], side: Side): Map<number, State> {
  const out = new Map<number, State>();
  for (const hunk of hunks) {
    let state: State = "";
    for (const line of hunk.lines) {
      const n = side === "before" ? line.before : line.after;
      if (n === null || line.path !== undefined) continue;
      out.set(n, state);
      state = grammar.line(line.text, state).state;
    }
  }
  return out;
}

/** One side's grammar, its blob's states when read under the bound, and whether it is known yet. */
interface SideRead {
  grammar: Grammar | null;
  lines: readonly string[] | null;
  states: readonly State[] | null;
  /** Past the bound, binary or failed: each hunk starts fresh. */
  fresh: boolean;
}

/** A side's grammar and its blob, read by its whole id when there is a grammar and lines to colour. */
function useSideRead(repo: string, path: string, oid: string | null, read: boolean): SideRead {
  const grammar = useGrammar(path);
  const blob = useBlobLines(repo, oid, { enabled: grammar !== null && read });
  const states = useMemo(
    () => (grammar && blob.lines ? statesOf(grammar, blob.lines, "") : null),
    [grammar, blob.lines],
  );
  return { grammar, lines: blob.lines, states, fresh: blob.over || blob.failed };
}

/**
 * A diff's spans for DiffView: each side's line from its blob's state under the
 * bound, else from its hunk's start; plain while a blob is being read.
 */
export function useDiffTokens(args: {
  repo: string;
  file: CodeDiffFile | undefined;
  hunks: readonly DiffHunk[];
}): ((side: Side, n: number) => readonly DiffSpan[] | null) | undefined {
  const { repo, file, hunks } = args;
  const binary = file?.binary === true;
  // A PURE RENAME OR A MODE CHANGE has no lines, so no blob is read for it.
  const read = !binary && (file?.added ?? 0) + (file?.deleted ?? 0) > 0;
  // A RENAME MAY CHANGE A FILE'S LANGUAGE, so each side takes its own path's.
  const before = useSideRead(
    repo,
    file ? (file.old_path ?? file.path) : "",
    file?.old_oid ?? null,
    read,
  );
  const after = useSideRead(repo, file?.path ?? "", file?.new_oid ?? null, read);
  const { grammar: gb, lines: lb, states: sb, fresh: fb } = before;
  const { grammar: ga, lines: la, states: sa, fresh: fa } = after;
  const present = file !== undefined;
  return useMemo(() => {
    if (!present || binary) return undefined;
    const sides = {
      before: { grammar: gb, lines: lb, states: sb, fresh: fb },
      after: { grammar: ga, lines: la, states: sa, fresh: fa },
    };
    const texts = { before: patchLines(hunks, "before"), after: patchLines(hunks, "after") };
    const walked: Partial<Record<Side, Map<number, State>>> = {};
    const kept = {
      before: new Map<number, DiffSpan[] | null>(),
      after: new Map<number, DiffSpan[] | null>(),
    };
    const spansOf = (side: Side, n: number): DiffSpan[] | null => {
      const { grammar, lines, states, fresh } = sides[side];
      const text = texts[side].get(n);
      if (!grammar || text === undefined || text.length > SYNTAX_MAX_LINE) return null;
      // PLAIN UNTIL KNOWN: a line is never coloured from its hunk and then again.
      if (!fresh && !states) return null;
      const hunk = () => (walked[side] ??= hunkStates(grammar, hunks, side)).get(n) ?? "";
      // A PATCH LINE THAT IS NOT ITS BLOB'S LINE (an interdiff's base, a CRLF)
      // takes its hunk's state instead.
      const state = states && lines && lines[n - 1] === text ? states[n - 1]! : hunk();
      return grammar
        .line(text, state)
        .tokens.map((t) => ({ end: t.end, className: t.kind ? SYNTAX_CLASS[t.kind] : null }));
    };
    return (side, n) => {
      const cache = kept[side];
      if (!cache.has(n)) cache.set(n, spansOf(side, n));
      return cache.get(n)!;
    };
  }, [present, binary, hunks, gb, lb, sb, fb, ga, la, sa, fa]);
}
