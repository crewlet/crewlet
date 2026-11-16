/**
 * HTML, XML and SVG; a state carries an open tag (`t`, with an open quote),
 * a comment (`c`), CDATA (`d`), a declaration (`!`), or a script or style body.
 */
import { Tokens, done, isSpace, skipSpace, step, until, word } from "./scan.ts";
import type { Grammar, Line, State } from "./types.ts";

const NAME = /[A-Za-z_:][\w:.-]*/y;
const ATTR = /[^\s"'<>/=]+/y;
const ENTITY = /&(?:#\d+|#[xX][\da-fA-F]+|[A-Za-z][\w]*);/y;
/** The bodies drawn as text until their close tag, by their state. */
const RAW: Record<string, string> = { s: "</script", y: "</style" };

export const markup: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    let i = 0;
    let mode = state;
    /** The tag whose body is raw text once its `>` closes, if any. */
    let rawNext = "";
    const closed = (end: number, kind: "comment" | "string" | "keyword", next: string) => {
      out.push(end < 0 ? n : end, kind);
      if (end >= 0) mode = next;
      return end < 0 ? n : end;
    };
    while (i < n) {
      if (mode === "c") {
        i = closed(until(text, i, "-->"), "comment", "");
      } else if (mode === "d") {
        i = closed(until(text, i, "]]>"), "string", "");
      } else if (mode === "!") {
        const end = until(text, i, ">");
        out.push(end < 0 ? n : end - 1, "keyword");
        if (end >= 0) out.push(end, "punctuation");
        if (end >= 0) mode = "";
        i = end < 0 ? n : end;
      } else if (mode === "s" || mode === "y") {
        // A SCRIPT OR STYLE BODY is text, to its close tag in any case.
        const close = RAW[mode]!;
        let at = text.indexOf("</", i);
        while (at >= 0 && text.slice(at, at + close.length).toLowerCase() !== close) {
          at = text.indexOf("</", at + 2);
        }
        out.push(at < 0 ? n : at, null);
        if (at >= 0) mode = "";
        i = at < 0 ? n : at;
      } else if (mode === "t" || mode === "t'" || mode === 't"') {
        // INSIDE A TAG: attributes, their values, and the tag's close.
        const ch = text[i]!;
        if (mode !== "t") {
          i = closed(until(text, i, mode[1]!), "string", "t");
        } else if (isSpace(ch)) {
          out.push((i = skipSpace(text, i)), null);
        } else if (ch === ">" || text.startsWith("/>", i) || text.startsWith("?>", i)) {
          out.push((i += ch === ">" ? 1 : 2), "punctuation");
          mode = ch === ">" ? rawNext : "";
          rawNext = "";
        } else if (ch === "=" || ch === "/") {
          out.push((i += 1), "punctuation");
        } else if (ch === '"' || ch === "'") {
          const end = until(text, i + 1, ch);
          out.push(end < 0 ? n : end, "string");
          if (end < 0) mode = `t${ch}`;
          i = end < 0 ? n : end;
        } else {
          const end = word(text, i, ATTR);
          const kind = text[i - 1] === "=" ? "string" : "function";
          out.push((i = end > i ? end : step(text, i)), kind);
        }
      } else {
        // TEXT: tags, comments, CDATA, declarations and entities.
        const ch = text[i]!;
        if (text.startsWith("<!--", i)) {
          mode = "c";
          out.push((i += 4), "comment");
        } else if (text.startsWith("<![CDATA[", i)) {
          mode = "d";
          out.push((i += 9), "punctuation");
        } else if (text.startsWith("<!", i) || text.startsWith("<?", i)) {
          const declaration = text[i + 1] === "!";
          out.push(i + 2, "punctuation");
          const end = word(text, i + 2, NAME);
          out.push(end, "keyword");
          i = Math.max(end, i + 2);
          mode = declaration ? "!" : "t";
        } else if (ch === "<") {
          const start = text[i + 1] === "/" ? i + 2 : i + 1;
          const end = word(text, start, NAME);
          if (end > start) {
            const name = text.slice(start, end).toLowerCase();
            const opening = start === i + 1;
            out.push(start, "punctuation");
            out.push((i = end), "keyword");
            mode = "t";
            rawNext = opening && name === "script" ? "s" : opening && name === "style" ? "y" : "";
          } else out.push((i += 1), null);
        } else if (ch === "&") {
          const end = word(text, i, ENTITY);
          const kind = end > i ? "number" : null;
          out.push((i = Math.max(end, i + 1)), kind);
        } else {
          let end = i + 1;
          while (end < n && text[end] !== "<" && text[end] !== "&") end++;
          out.push((i = end), null);
        }
      }
    }
    const known = ["", "t", "t'", 't"', "c", "d", "!", "s", "y"];
    return done(out, known.includes(mode) ? mode : "");
  },
};
