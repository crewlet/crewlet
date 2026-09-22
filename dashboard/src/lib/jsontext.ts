/**
 * Indenting a JSON document WITHOUT EVER DECODING IT.
 *
 * Every tool call this engine records carries its arguments as JSON TEXT, and
 * the engine writes that text with `tools.RecordArgs` — compact, no spaces,
 * no newlines. A transcript rendering it verbatim is therefore ONE LINE
 * however many arguments the call had, with the one a reader came for — the
 * channel, the page id, the item key — somewhere in the middle of it. What
 * indenting buys is that: a key per line, so the call can be read down rather
 * than scanned across.
 *
 * WHAT IT DOES NOT BUY is a shorter line for one long VALUE. A `create_page`
 * body is four thousand characters whether or not the document around it is
 * indented, because `\n` inside a string literal is two characters and this
 * copies a literal whole. Resolving those would produce text that is no
 * longer JSON, no longer idempotent and no longer paste-able into `jq`, so it
 * is deliberately left alone: what handles a long value is the block WRAPPING,
 * which is why both callers stopped turning wrapping off at the same time as
 * they started indenting.
 *
 * # Why not `JSON.stringify(JSON.parse(text), null, 2)`
 *
 * Because the two screens that render these strings each refused to decode
 * them, in a comment, on purpose — and the reason is a DISPLAY bug rather than
 * a purity argument. TWO of the things a round trip through JavaScript's own
 * JSON loses are live on this path, and either one on its own settles it:
 *
 *   - **A number wider than 2^53 comes back a different number.** An id of
 *     `9007199254740993` re-encodes as `9007199254740992`. The engine goes out
 *     of its way for this — every decoder between a model and the record reads
 *     through Go's `json.Number` so that a 19-digit Jira id or Slack timestamp
 *     stays exact — and a screen whose job is to say which object a tool was
 *     called on, showing an id that is off by one, is worse than the same
 *     screen being ugly.
 *   - **A number's spelling is not its value.** `1.0` re-encodes as `1`, `1e3`
 *     as `1000`. What the model actually sent stops being visible, and
 *     `json.Number` is again what carried the spelling this far.
 *
 * Three more are properties of the function rather than of these records,
 * because the engine marshals a Go map before it writes the text: a resolved
 * `\u003c` (only in a record written before `tools.RecordArgs` stopped
 * escaping those), a dropped duplicate key, and integer-like keys sorted to
 * the front. None of them is the reason — they are what it costs nothing to
 * be right about once the text is walked rather than decoded.
 *
 * So this walks the TEXT. Structural characters get the newlines and the
 * two-space indent; every literal — number, string, `true`, `false`, `null` —
 * is copied across byte for byte, in the order it was written.
 *
 * `JSON.parse` is still called, for its VERDICT and nothing else: the value it
 * returns is discarded on the next line. Text this build cannot parse is
 * returned exactly as it arrived rather than half-formatted, because a
 * provider that answered with something that is not JSON at all, or a run
 * whose record was cut short, is a reading a transcript has to be able to
 * show.
 *
 * The result is byte-identical to `JSON.stringify(value, null, 2)` for every
 * document whose scalars survive the round trip and whose nesting is inside
 * [MAX_INDENT_DEPTH], which is the point: a reader has nothing new to learn,
 * and the blocks fed from a DECODED map (the live overlay's, which the socket
 * already parsed) land on the same shape as the blocks fed from wire text.
 *
 * "BYTE FOR BYTE" IS ABOUT THE LITERALS, not the document: what a reader
 * copies out of a `selectable` block is the indented text, which parses to
 * the same value as the record and is not the same bytes as it. That is the
 * right trade for a block whose whole purpose is being read, and it is why
 * the values had to be exact — a copied id is pasted into a query.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives: a
 * derivation that can only be exercised by rendering a component is one nobody
 * re-measures.
 */

/** One level of nesting, and it is JSON.stringify's own so the two agree. */
const INDENT = "  ";

/**
 * How deep the indent GROWS before it stops, in levels.
 *
 * 32, which is 64 leading spaces — most of the width one of these blocks has
 * in a 420px detail rail. Past that the indent has stopped describing the
 * structure and started pushing every value off the right edge, so a deeper
 * document keeps its newlines and stops gaining margin. Arrived at from
 * reading rather than from cost.
 *
 * It happens to be the clause that keeps the output LINEAR in the input, and
 * that is worth knowing: indentation is quadratic in nesting depth, and the
 * depth this transformation is handed is not the engine's to choose. A tool
 * result is whatever an MCP server answered, and `encoding/json` accepts a
 * document 10,000 deep, which unclamped is a 10 kB record asking the browser
 * for fifty megabytes on the main thread. `JSON.stringify(v, null, 2)` has
 * exactly the same shape — this is not a defect being worked around, it is a
 * property of indenting that only became reachable when these two blocks
 * started indenting third-party text.
 */
const MAX_INDENT_DEPTH = 32;

/** The indent for one nesting level, clamped — see [MAX_INDENT_DEPTH]. */
function margin(depth: number): string {
  return INDENT.repeat(depth < MAX_INDENT_DEPTH ? depth : MAX_INDENT_DEPTH);
}

/**
 * `text` re-indented as a JSON document, or `text` itself when it is not one.
 *
 * Idempotent, and that is load-bearing rather than incidental: insignificant
 * whitespace is dropped and re-emitted, so a producer that already indented —
 * four spaces, or the two a decoded map gets from `JSON.stringify` — lands on
 * the same shape as every other block instead of on a second house style.
 */
export function indentJSON(text: string): string {
  if (text === "") return text;
  try {
    // THE VERDICT ONLY. What comes back is thrown away; see the note above on
    // why the parsed value must never reach the screen.
    JSON.parse(text);
  } catch {
    return text;
  }

  let out = "";
  let depth = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    // A STRING IS COPIED WHOLE, and this is the clause the whole scan turns
    // on: a value may contain any of the characters below, and treating the
    // brace in `{"body":"a { b"}` as structure would indent prose.
    if (c === '"') {
      const end = endOfString(text, i);
      out += text.slice(i, end);
      i = end - 1;
      continue;
    }
    switch (c) {
      case "{":
      case "[": {
        const close = c === "{" ? "}" : "]";
        const next = nextSignificant(text, i + 1);
        if (text[next] === close) {
          // An EMPTY container stays on one line, which is what
          // JSON.stringify writes and what a reader wants: `"tags": []` says
          // "none" in a way three lines do not.
          out += c + close;
          i = next;
          continue;
        }
        depth++;
        out += c + "\n" + margin(depth);
        continue;
      }
      case "}":
      case "]":
        depth--;
        out += "\n" + margin(depth) + c;
        continue;
      case ",":
        out += ",\n" + margin(depth);
        continue;
      case ":":
        out += ": ";
        continue;
      case " ":
      case "\t":
      case "\n":
      case "\r":
        // Insignificant, and dropping it is what makes this idempotent.
        continue;
      default:
        // A number, or one of the three bare words. Copied as spelled.
        out += c;
    }
  }
  return out;
}

/**
 * One past the closing quote of the string literal that opens at `open`.
 *
 * The escape clause is the whole of it: a backslash consumes whatever follows
 * it, so `"a\"b"` is one string rather than two, and `"a\\"` ends where it
 * looks like it ends.
 */
function endOfString(text: string, open: number): number {
  for (let i = open + 1; i < text.length; i++) {
    const c = text[i];
    if (c === "\\") {
      i++;
      continue;
    }
    if (c === '"') return i + 1;
  }
  // Unreachable on a document the parse gate accepted — an unterminated
  // string is not JSON — and the honest answer if it ever is reached.
  return text.length;
}

/** The next index holding something other than JSON's insignificant space. */
function nextSignificant(text: string, from: number): number {
  let i = from;
  while (i < text.length) {
    const c = text[i];
    if (c !== " " && c !== "\t" && c !== "\n" && c !== "\r") return i;
    i++;
  }
  return i;
}
