// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * The stylesheets and the tree agree about every class name, in BOTH
 * directions.
 *
 * # Why this is a test and not a convention
 *
 * The stylesheet is one global namespace and `className` is a bare string, so
 * nothing connects the two: a class nobody declared is not an error, not a
 * warning, and not a visual difference a reviewer would notice in a diff. It
 * is silent, and it stays silent for as long as nobody opens that screen.
 *
 * Twelve of them had accumulated when this test was written. `dim` (23 sites)
 * was the muted-text class on two whole screens and had never once set a
 * colour — `.muted` is its real name. `gap-2` (21 sites) asked for the one rung
 * a scale of 1/3/4/6 was missing. `comment` gave a thread its blocks and was
 * never declared, so a work item's comments ran together as one paragraph.
 * `toast-close` left the dismiss X as the browser's grey chrome box. And
 * `stack`, the worst of them, DID exist — as a six-pixel segmented chart bar
 * with `overflow: hidden` — so the work item screen rendered its entire body,
 * every panel, inside a six-pixel strip.
 *
 * That last one is why the check is "declared", not "spelled like a class":
 * the failure was a name that resolved to the wrong component, and the only
 * thing that catches it is reading both sides.
 *
 * # THE OTHER DIRECTION, which is the one that let a rewrite rot
 *
 * For a long time only "used implies declared" was checked, and the inverse —
 * declared implies used — was not checked at all. A recipe whose last writer
 * was deleted is invisible in exactly the way an undeclared class is: no
 * error, no warning, and nothing in the diff, because the diff is in a FILE
 * NOBODY TOUCHED. Porting the dashboard onto `@crewlethq/ui` deleted fifteen
 * components in one pass and left sixty-four dead recipes behind — Panel,
 * Button, Badge, Chip, Avatar, Empty, Banner, Disclosure, Skeleton, Stat,
 * StatRow, Tabs, SearchInput, Select, Toast and the chart parts — roughly
 * fifteen per cent of everything the product declared, all of it still being
 * parsed by every reader's browser and, worse, still being read by the next
 * person looking for the recipe a screen actually uses.
 *
 * Two things make the inverse harder than it looks.
 *
 * THE COLLECTOR HAS TO READ SELECTORS, NOT THE FILE. A regex for `.name` over
 * a whole stylesheet also matches the extension inside `url(…/inter.woff2)`
 * and `src:`, so a crude inverse reports `css`, `ts`, `tsx`, `txt` and `woff2`
 * as orphaned classes — and it matches a class named in a COMMENT, which is
 * how `.kv`, `.screen-head`, `.stack`, `.topbar` and `.work-grid` looked like
 * live recipes here when every one of them had already been deleted and only
 * the prose remembering them was left. Harmless in the forward direction,
 * where an extra declared name only widens what is allowed; fatal in the
 * inverse, where it is the answer. So `declaredIn` walks the structure —
 * comments blanked, at-rule preludes skipped, declarations dropped at their
 * `;` — and reads class names out of SELECTORS alone.
 *
 * AN INTERPOLATED CLASS HAS NO LITERAL. `` cx("wl-bar", `is-${tone}`) `` writes
 * `is-caution` with the string `is-caution` appearing nowhere. Where the
 * collector can see the interpolation it does, and that is better than a list
 * somebody has to maintain: a template literal in a CLASS POSITION contributes
 * its STEM, and a stem covers a declared class when the stem, minus its
 * trailing separators, is itself a class this tree writes literally. That is
 * what carries `int-seat-tier--full_access` and `int-seat-tier--review`, from
 * `` `int-seat-tier int-seat-tier--${seat.tier}` `` — the modifier's own block
 * is right there in the same expression. It deliberately does NOT carry
 * `is-`: `is` is nobody's class, and honouring a bare `is-` would hide a dead
 * `.is-open`, `.is-add` or `.is-destructive` for ever. Those three fall to
 * `ALLOWED` instead, which is an entry with a reason each, never a count and
 * never a prefix.
 *
 * # What each side cannot see
 *
 * THE FORWARD SIDE reads class POSITIONS only — a `className` attribute, a
 * `className:` property holding a string, and the arguments of a `cx(…)`
 * inside an attribute — and inside `cx` only two positions are a class: a bare
 * string argument, and the right-hand side of a `&&` guard. A string that is
 * being COMPARED is not one: `cx("avatar", size !== "md" && size)` names
 * `avatar`, while `md` is the default it is testing for and `size` is the
 * variant it yields. Reading every quoted string in the call would report `md`
 * as undeclared for ever, and a check that cries wolf is one somebody switches
 * off.
 *
 * THE PROPERTY FORM IS HERE BECAUSE IT WAS MISSED, and the thing it hid was
 * real. `lib/markdown.ts` builds its blocks with `createElement`, so every
 * class it writes is `className: "…"` in a props object rather than an
 * attribute — and one of them, the fenced block's, named two classes this
 * stylesheet declares for something else and nothing declared for a `pre`. It
 * therefore had no recipe at all: no surface, no padding, and a `pre`'s own
 * `white-space: pre` with `overflow: visible`, so one long line pushed the
 * page 678px past its viewport. The forward check read every one of that
 * file's siblings — `md-table`, `md-tasks`, `md-task-body` — as covered,
 * because they happened to be declared, and said nothing about the one that
 * was not. A `className:` property with a string literal is as unambiguous a
 * class position as the attribute is; what it does NOT cover is a property
 * whose value is a call or a variable, which no file in this tree writes.
 *
 * THE INVERSE SIDE cannot afford that restraint, because a name it fails to
 * see is a recipe somebody deletes. A class reaches the DOM through more than
 * a `className` attribute — `containerClassName` on a uilet Input, a `cx(…)`
 * whose result is assigned to a variable, a `className:` in a `createElement`
 * props object, a bare `"caution"` returned from a helper and handed to
 * `cx("dot", tone)` — so it reads EVERY string literal in the tree, with
 * comments stripped on this side too. It is a floor rather than a proof, and
 * the cost is stated: a class whose name is also an ordinary word survives on
 * a string that has nothing to do with it. `.panel`, `.banner` and `.degraded`
 * were all dead and all invisible here, kept alive by `aria-label="Resize the
 * detail panel"`, `layout="banner"` and `case "degraded":`. Two of those three
 * are now caught — a string that is the value of a NAMED slot which is not a
 * className is not a class — and the third is not, and will not be.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));
const require_ = createRequire(import.meta.url);

interface Sheet {
  name: string;
  text: string;
}

/** Every stylesheet in the tree. */
function sheets(): Sheet[] {
  const dir = join(SRC, "styles");
  return readdirSync(dir)
    .filter((f) => f.endsWith(".css"))
    .map((name) => ({ name, text: readFileSync(join(dir, name), "utf8") }));
}

/** Every stylesheet in the tree, concatenated. */
function stylesheets(): string {
  return sheets()
    .map((s) => s.text)
    .join("\n");
}

/**
 * Class names the stylesheets declare, read out of SELECTORS and nothing else.
 *
 * EXPORTED SO THE MUTATION TEST BELOW RUNS THE SAME CODE, and written as a
 * walk rather than a regex because a regex is what the crude version was — see
 * the note above on `woff2`.
 *
 * The value is where it was first declared, so a failure names a line rather
 * than a word.
 */
export function declaredIn(all: Sheet[]): Map<string, string> {
  const names = new Map<string, string>();
  for (const { name: file, text } of all) {
    // Blanked rather than removed, so the line numbers stay true.
    const css = text.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "));
    let prelude = "";
    let at = 0;
    // Where the prelude's first NON-SPACE character is, so a selector on its
    // own line is reported at that line rather than at the newline before it.
    let started = false;
    for (let i = 0; i < css.length; i++) {
      const c = css[i]!;
      if (c === "{") {
        const sel = prelude.trim();
        // An at-rule's prelude is a condition, not a selector: `@media
        // (min-width: 861px)` has no class in it and `@supports` can carry one
        // that nothing is ever styled by.
        if (sel && !sel.startsWith("@")) {
          const line = css.slice(0, at).split("\n").length;
          for (const m of sel.matchAll(/\.(-?[_a-zA-Z][-\w]*)/g)) {
            if (!names.has(m[1]!)) names.set(m[1]!, `${file}:${line}`);
          }
        }
        prelude = "";
        at = i + 1;
        started = false;
      } else if (c === "}" || c === ";") {
        // A declaration ends at its `;`, which is what keeps `src:
        // url(inter.woff2)` and `clip-path: inset(50%)` out of the answer.
        prelude = "";
        at = i + 1;
        started = false;
      } else {
        if (!started && !/\s/.test(c)) {
          at = i;
          started = true;
        }
        prelude += c;
      }
    }
  }
  return names;
}

interface Use {
  name: string;
  where: string;
}

/** The balanced span opened by `text[from]`, without its delimiters. */
function balanced(text: string, from: number, open: string, close: string): string | null {
  let depth = 0;
  for (let i = from; i < text.length; i++) {
    if (text[i] === open) depth++;
    else if (text[i] === close) {
      depth--;
      if (depth === 0) return text.slice(from + 1, i);
    }
  }
  return null;
}

/** An argument list split on its own top-level commas. */
function args(list: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let cur = "";
  for (const ch of list) {
    if ("([{".includes(ch)) depth++;
    if (")]}".includes(ch)) depth--;
    if (ch === "," && depth === 0) {
      out.push(cur);
      cur = "";
      continue;
    }
    cur += ch;
  }
  out.push(cur);
  return out;
}

/**
 * Class names a source file hard-codes in a class POSITION.
 *
 * BALANCED RATHER THAN LINE-BY-LINE, which is the one thing this grew. The
 * reader it replaced matched `className={cx(` and then took `[^)]*` on that
 * one line, so every `cx(` prettier had broken across lines — `grid-th`,
 * `grid-row`, `phase-card`, `tool-row` and a hundred others — contributed
 * NOTHING, while the doc claimed "anything a screen hard-codes is covered".
 * Ten more names are checked now and none of them was undeclared, which is
 * luck rather than evidence: nothing had been looking.
 */
export function usesIn(file: string, text: string): Use[] {
  const found: Use[] = [];
  const add = (raw: string, index: number) => {
    const line = text.slice(0, index).split("\n").length;
    for (const name of raw.split(/\s+/).filter(Boolean)) {
      found.push({ name, where: `${file}:${line}` });
    }
  };
  // A `className:` PROPERTY, which is how `createElement` spells the
  // attribute — `lib/markdown.ts` writes every one of its block classes this
  // way. Only a string literal: a `className: cx(…)` would need the argument
  // reading the attribute arm below does, and nothing in this tree writes one.
  for (const m of text.matchAll(/\bclassName:\s*"([^"]*)"/g)) add(m[1]!, m.index);
  for (const m of text.matchAll(/className=(?:"([^"]*)"|\{)/g)) {
    if (m[1] !== undefined) {
      add(m[1], m.index);
      continue;
    }
    const body = balanced(text, m.index + m[0].length - 1, "{", "}");
    if (body === null) continue;
    let sawCx = false;
    for (const call of body.matchAll(/(?:^|[^\w.])cx\(/g)) {
      sawCx = true;
      const list = balanced(body, call.index + call[0].length - 1, "(", ")");
      if (list === null) continue;
      for (const arg of args(list)) {
        const bare = /^\s*"([^"]*)"\s*$/.exec(arg);
        if (bare) {
          add(bare[1]!, m.index);
          continue;
        }
        const guarded = /&&\s*"([^"]*)"\s*$/.exec(arg);
        if (guarded) add(guarded[1]!, m.index);
      }
    }
    // A plain string or a ternary over plain strings — `className={open ? "a
    // on" : "a"}`. Only where there is no `cx` to read instead, because inside
    // one the comparison operands are not classes.
    if (!sawCx) for (const s of body.matchAll(/"([^"]*)"/g)) add(s[1]!, m.index);
  }
  return found;
}

interface Written {
  /** Every token of every string literal the tree holds. */
  literal: Map<string, string>;
  /** The literal head of a template interpolation in a class position. */
  stems: Map<string, string>;
}

/** Where a `/` opens a regex literal rather than dividing. */
function regexPosition(prev: string): boolean {
  const p = prev.trimEnd();
  if (p === "") return true;
  if (/[([{,;:=!&|?+\-*%~^<>]$/.test(p)) return true;
  return /\b(return|typeof|case|in|of|do|else|yield|await|new|delete|void)$/.test(p);
}

interface Found {
  literals: { name: string; line: number; named: boolean }[];
  stems: { stem: string; line: number }[];
}

/**
 * Every string literal in one source file, and the class-position stems.
 *
 * A LEXER RATHER THAN A REGEX, and each rule in it is a bug it had:
 *
 *  - REGEX LITERALS ARE SKIPPED WHOLE. `lib/markdown.ts` alone holds a dozen
 *    carrying a backtick or a quote, and reading one as a string swallowed
 *    every real literal after it — which is how `md-table`, `md-tasks` and
 *    `md-task-body` first read as orphans.
 *  - A `'` OR `"` THAT DOES NOT CLOSE ON ITS OWN LINE IS NOT A STRING. It is
 *    an apostrophe in JSX text, and swallowing to the next one of its kind
 *    ate whole screens' worth of names.
 *  - A TEMPLATE'S `${…}` IS CODE, re-entered rather than blanked, because the
 *    classes in `` `work-cal-cell${cell.today ? " today" : ""}` `` are inside
 *    it. Blanking the interpolation is right for the forward direction, where
 *    a lost use costs a check; here it invents an orphan.
 *  - COMMENTS ARE STRIPPED, so prose naming a class it no longer writes
 *    cannot keep the recipe alive. Five of the dead names above survived a
 *    crude scan on nothing but a sentence remembering them.
 */
function scan(text: string, forced = false): Found {
  const literals: Found["literals"] = [];
  const stems: Found["stems"] = [];
  const stack: { klass: boolean }[] = [];
  let i = 0;
  let line = 1;
  let prev = "";
  const n = text.length;
  const classPosition = (at: number): boolean => {
    const before = text.slice(Math.max(0, at - 60), at);
    return forced || stack.some((e) => e.klass) || /[A-Za-z]*[Cc]lassName\s*[:=]\s*$/.test(before);
  };
  // A string that is the value of a NAMED slot which is not a className is not
  // a class: `layout="banner"`, `variant="brand"`, `aria-label="… panel"`. It
  // is what stops a component's prop vocabulary keeping a dead recipe alive.
  const namedSlot = (at: number): boolean => {
    const before = text.slice(Math.max(0, at - 60), at);
    const m = /([A-Za-z_$][\w$-]*)(=|:\s*)$/.exec(before);
    return m !== null && !/[Cc]lassName$/.test(m[1]!);
  };
  while (i < n) {
    const c = text[i]!;
    if (c === "\n") {
      line++;
      i++;
      prev = "";
      continue;
    }
    if (c === "/" && text[i + 1] === "/") {
      while (i < n && text[i] !== "\n") i++;
      continue;
    }
    if (c === "/" && text[i + 1] === "*") {
      i += 2;
      while (i < n && !(text[i] === "*" && text[i + 1] === "/")) {
        if (text[i] === "\n") line++;
        i++;
      }
      i += 2;
      continue;
    }
    if (c === "/" && regexPosition(prev)) {
      let j = i + 1;
      let inClass = false;
      let closed = false;
      while (j < n && text[j] !== "\n") {
        if (text[j] === "\\") {
          j += 2;
          continue;
        }
        if (inClass) {
          if (text[j] === "]") inClass = false;
        } else if (text[j] === "[") inClass = true;
        else if (text[j] === "/") {
          closed = true;
          j++;
          break;
        }
        j++;
      }
      if (closed) {
        i = j;
        prev = "/";
        continue;
      }
    }
    if (c === '"' || c === "'") {
      const quote = c;
      const at = line;
      const named = namedSlot(i);
      let j = i + 1;
      let body = "";
      let closed = false;
      while (j < n) {
        if (text[j] === "\\") {
          body += text[j + 1] ?? "";
          j += 2;
          continue;
        }
        if (text[j] === "\n") break;
        if (text[j] === quote) {
          closed = true;
          j++;
          break;
        }
        body += text[j];
        j++;
      }
      if (!closed) {
        i++;
        prev += c;
        continue;
      }
      i = j;
      prev = "s";
      for (const t of body.split(/\s+/).filter(Boolean))
        literals.push({ name: t, line: at, named });
      continue;
    }
    if (c === "`") {
      const klass = classPosition(i);
      const named = namedSlot(i);
      i++;
      let chunk = "";
      const flush = () => {
        for (const t of chunk.split(/\s+/).filter(Boolean)) literals.push({ name: t, line, named });
        chunk = "";
      };
      while (i < n && text[i] !== "`") {
        if (text[i] === "\\") {
          chunk += text[i + 1] ?? "";
          i += 2;
          continue;
        }
        if (text[i] === "$" && text[i + 1] === "{") {
          const stem = /(^|\s)([-\w]*[-_])$/.exec(chunk);
          if (stem && klass) stems.push({ stem: stem[2]!, line });
          // The partial token in front of the interpolation is a stem, never a
          // name: emitting it would report `is-` as an undeclared class.
          if (stem) chunk = chunk.slice(0, chunk.length - stem[2]!.length);
          flush();
          i += 2;
          let depth = 1;
          const sub: string[] = [];
          while (i < n && depth > 0) {
            const d = text[i]!;
            if (d === "\n") line++;
            if (d === "{") depth++;
            else if (d === "}") {
              depth--;
              if (depth === 0) {
                i++;
                break;
              }
            }
            sub.push(d);
            i++;
          }
          const inner = scan(sub.join(""), klass);
          for (const l of inner.literals) literals.push({ name: l.name, line, named: l.named });
          for (const s of inner.stems) stems.push({ stem: s.stem, line });
          continue;
        }
        if (text[i] === "\n") line++;
        chunk += text[i];
        i++;
      }
      i++;
      prev = "s";
      flush();
      continue;
    }
    if (c === "{" || c === "(" || c === "[") {
      const before = text.slice(Math.max(0, i - 40), i);
      stack.push({
        klass:
          (c === "{" && /[A-Za-z]*[Cc]lassName\s*=\s*$/.test(before)) ||
          (c === "(" && /\bcx\s*$/.test(before)),
      });
      i++;
      prev += c;
      continue;
    }
    if (c === "}" || c === ")" || c === "]") {
      stack.pop();
      i++;
      prev += c;
      continue;
    }
    prev += c;
    i++;
  }
  return { literals, stems };
}

/** Everything the tree writes, for the inverse direction. */
export function writtenIn(files: { name: string; text: string }[]): Written {
  const literal = new Map<string, string>();
  const stems = new Map<string, string>();
  for (const { name, text } of files) {
    const found = scan(text);
    for (const l of found.literals) {
      if (l.named) continue;
      if (!literal.has(l.name)) literal.set(l.name, `${name}:${l.line}`);
    }
    for (const s of found.stems) if (!stems.has(s.stem)) stems.set(s.stem, `${name}:${s.line}`);
  }
  return { literal, stems };
}

/**
 * A declared class with no literal writer that is nonetheless written.
 *
 * ONE ENTRY PER NAME, WITH A REASON, in the spirit of
 * `internal/skipgate/allowed.go`: a count goes green the moment one is fixed
 * and another appears, and a prefix hides the next dead recipe that happens to
 * start with the same word. It is two-sided — an entry naming a class no
 * stylesheet declares is stale and fails — so the list cannot outlive what it
 * excuses.
 */
interface Allowed {
  name: string;
  /** What writes it, and why no literal can be found. */
  why: string;
  /**
   * For a class the DESIGN SYSTEM owns: the installed stylesheet that must
   * still declare it. A uilet class is written by uilet, so this tree can
   * only ever style it from the outside — and a bump that renames it would
   * otherwise leave our rule pointing at nothing, silently.
   */
  pkg?: string;
}

const ALLOWED: Allowed[] = [
  {
    name: "is-positive",
    why: "routes/company/People.tsx builds the workload bar's tone as cx(\"wl-bar\", `is-${tone}`) over lib/workload.ts's LoadTone. The stem is a bare `is-`, which is deliberately not honoured by the collector: half a dozen unrelated components write is-open, is-add, is-remove, is-destructive and is-active literally, and a prefix here would hide the next one of those to die.",
  },
  {
    name: "is-caution",
    why: "The same interpolation, for a queue at or past the heavy-queue mark. See is-positive.",
  },
  {
    name: "is-critical",
    why: "The same interpolation, for a queue whose every open item is blocked. See is-positive.",
  },
  {
    name: "crewlet-card__title",
    why: "uilet's Card.Title writes it. styles/screens.css makes it ellipse rather than clip, which the package cannot do for itself: the title is a flex container and text-overflow only reaches a block one. Another rule about OUR composition of the package's component.",
    pkg: "@crewlethq/ui/styles.css",
  },
  {
    name: "crewlet-card--flush",
    why: "uilet's Card writes it for `padding=\"none\"` content that reaches the card's edges. styles/screens.css turns its `overflow: hidden` into `clip`, which is what the recipe's own comment asks for — `hidden` also makes the card a scroll container, and a sticky box confined to a scrollport that can never scroll never has `top` applied, so every DataGrid inside a flush card had a dead column head. Another rule about OUR composition of the package's component.",
    pkg: "@crewlethq/ui/styles.css",
  },
  {
    name: "crewlet-segmented",
    why: "uilet's SegmentedControl writes it. styles/frame.css hides the two in the rail's foot — theme and density — once the rail is a bottom bar, where they took 250px of a 390px phone and left 140px for eight destinations. Both settings are in the command palette's `>` scope, and the collapsed rail already drops them for the same reason. Another rule about OUR composition of the package's component.",
    pkg: "@crewlethq/ui/styles.css",
  },
  {
    name: "crewlet-disclosure__trigger",
    why: "uilet's Disclosure writes it. styles/screens.css widens its gap inside a .tool-row, which is a rule about OUR composition of the package's component — the one kind of class this tree declares and never writes.",
    pkg: "@crewlethq/ui/styles.css",
  },
  {
    name: "crewlet-tabs--pill",
    why: "uilet's Tabs writes it for its default variant. styles/screens.css bounds it at its container and makes it scroll, which the package already does for its underline row and not for this one: `.crewlet-tabs` is `display: inline-flex` and the pill variant is `width: fit-content`, and neither caps — a flex row of nowrap labels has a min-content width equal to the sum of them. Measured on the tracker at 390px, the view switcher stood 497px wide and took the document to 642, so the reader dragged the whole page to reach a tab past the edge. Another rule about OUR composition of the package's component.",
    pkg: "@crewlethq/ui/styles.css",
  },
];

/**
 * A class a screen writes that NO stylesheet declares, deliberately.
 *
 * THE MIRROR OF [ALLOWED], and it needs its own list for the same reason that
 * one does: the honest cases are few, each has a reason, and a blanket prefix
 * would hide the next `stack`. A handle is the one honest case — a name on an
 * element so a suite can find it, where the drawing is somebody else's and a
 * rule here would be this tree quietly restating the package's own.
 *
 * TWO-SIDED, like everything else in this file. An entry fails when nothing
 * writes the name any more (the handle went, the excuse outlived it) and when
 * a stylesheet DOES declare it (the excuse is a lie, and the rule it now
 * carries is unreviewed). So the list cannot drift away from what it excuses
 * in either direction.
 */
interface Handle {
  name: string;
  /** What finds it, and why nothing draws it. */
  why: string;
}

const HANDLES: Handle[] = [
  {
    name: "btable-name",
    why: "routes/org/builder/TableView.tsx puts it on the design system's OrgTableName so four suites can find a builder row by its NAME cell rather than by whichever cell happens to mention a name. The package's own `.crewlet-org-table__node` already declares every property it would carry, byte for byte, and routes/org/builder/builderStyles.test.ts asserts this tree adds nothing on top.",
  },
  {
    name: "btable-label",
    why: "The name span inside that cell, for the same suites. `min-width: 0` on it was inert — the span is not a flex item and the ellipsis lives on the package's `__name`. See btable-name.",
  },
];

/** Every declared class with nothing in the tree that writes it. */
export function orphans(
  declared: Map<string, string>,
  written: Written,
  allowed: Allowed[],
): string[] {
  const excused = new Set(allowed.map((a) => a.name));
  // A stem covers a class only when the stem, minus its trailing separators,
  // is itself a class this tree writes literally — `int-seat-tier--` is the
  // modifier form of `int-seat-tier`, and `is-` is the modifier form of
  // nothing at all.
  const owned = [...written.stems.keys()].filter((s) =>
    written.literal.has(s.replace(/[-_]+$/, "")),
  );
  const out: string[] = [];
  for (const [name, where] of declared) {
    if (written.literal.has(name)) continue;
    if (excused.has(name)) continue;
    if (owned.some((s) => name.startsWith(s) && name.length > s.length)) continue;
    out.push(`${name} — ${where}`);
  }
  return out.sort();
}

function sources(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) sources(p, out);
    else if (/\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry)) out.push(p);
  }
  return out;
}

describe("every class the dashboard names", () => {
  const css = declaredIn(sheets());
  const files = sources(SRC).map((p) => ({
    name: p.slice(SRC.length),
    text: readFileSync(p, "utf8"),
  }));
  const all = files.flatMap((f) => usesIn(f.name, f.text));

  test("is declared by a stylesheet, or is a handle that says it is not", () => {
    const handles = new Set(HANDLES.map((h) => h.name));
    const missing = all.filter((u) => !css.has(u.name) && !handles.has(u.name));
    expect(missing.map((u) => `${u.name} — ${u.where}`)).toEqual([]);
  });

  // BOTH WAYS ON THE EXCUSES THEMSELVES. A handle whose element went is an
  // excuse with nothing left to excuse, and a handle a stylesheet has since
  // picked up is a rule that got in without being read.
  test("every handle is still written, and still drawn by nothing", () => {
    const written = new Set(all.map((u) => u.name));
    expect(
      HANDLES.filter((h) => !written.has(h.name)).map((h) => h.name),
      "these are excused as handles and nothing writes them — delete the entry",
    ).toEqual([]);
    expect(
      HANDLES.filter((h) => css.has(h.name)).map((h) => h.name),
      "these are excused as drawing nothing and a stylesheet now declares them — " +
        "delete the entry, and read the rule it let in",
    ).toEqual([]);
  });

  test("is found at all, so the scan cannot pass by reading nothing", () => {
    // The check above is vacuous if `usesIn` returns an empty list, which is
    // exactly what a changed `className=` spelling or a moved source root
    // would do to it. These two are the shapes the tree actually writes.
    expect(all.length).toBeGreaterThan(500);
    expect(all.some((u) => u.name === "segmented")).toBe(true);
    expect(all.some((u) => u.name === "muted")).toBe(true);
  });

  // AND THE OTHER WAY. See the note at the top: this is the direction nothing
  // was checking while a rewrite left sixty-four recipes behind it.
  test("and every class a stylesheet declares is written by something", () => {
    expect(
      orphans(css, writtenIn(files), ALLOWED),
      "these recipes are declared and nothing in the tree writes them — delete " +
        "them, or, if one is written through something this scan cannot see, " +
        "add it to ALLOWED with the reason",
    ).toEqual([]);
  });

  test("the inverse can tell — a recipe nothing writes is caught", () => {
    // THE MUTATION, run against the functions the test above calls rather
    // than against a re-spelling of them: tokens.test.ts learned that lesson
    // first, where the "it can tell" case inlined its own regexes and so
    // stayed green through every possible break in the real check.
    const declared = declaredIn([
      {
        name: "fake.css",
        // `.woff2` and `.tsx` are inside a declaration and a url, which is
        // where the crude collector found its five phantom classes.
        text: [
          "@font-face {",
          "  src: url(/static/dashboard/fonts/inter.woff2) format('woff2');",
          "}",
          "/* .ghost-in-a-comment is not a declaration */",
          "@media (min-width: 861px) {",
          "  .kept { color: red; }",
          "}",
          ".kept { color: red; }",
          ".dropped { color: blue; }",
          ".wl-bar.is-caution { color: amber; }",
          ".int-seat-tier--review { color: grey; }",
        ].join("\n"),
      },
    ]);
    expect([...declared.keys()].sort()).toEqual([
      "dropped",
      "int-seat-tier--review",
      "is-caution",
      "kept",
      "wl-bar",
    ]);

    const written = writtenIn([
      {
        name: "fake.tsx",
        text: [
          "// .dropped is only remembered in a comment, and `.ghost-in-a-comment` too.",
          'const a = <div className="kept" />;',
          'const b = <div className={cx("wl-bar", `is-${tone}`)} />;',
          "const c = <div className={`int-seat-tier int-seat-tier--${tier}`} />;",
        ].join("\n"),
      },
    ]);
    expect(orphans(declared, written, ALLOWED)).toEqual(["dropped — fake.css:9"]);

    // Both excuses carry their weight, and neither is a prefix: the stem
    // covers the tier because `int-seat-tier` is written beside it, and
    // ALLOWED covers `is-caution` because `is-` is not a class.
    expect(orphans(declared, written, [])).toEqual([
      "dropped — fake.css:9",
      "is-caution — fake.css:10",
    ]);
  });

  test("every excuse in ALLOWED still has something to excuse", () => {
    // AN ENTRY THAT STOPPED APPLYING IS STALE, and a stale excuse is how a
    // list like this rots into a blanket. Two-sided in both senses a list
    // here can be: the class must still be declared by this tree, and a
    // package-owned one must still be declared by the package.
    const gone = ALLOWED.filter((a) => !css.has(a.name));
    expect(
      gone.map((a) => a.name),
      "ALLOWED names a class no stylesheet declares",
    ).toEqual([]);
    expect(ALLOWED.filter((a) => a.why.trim() === "").map((a) => a.name)).toEqual([]);
    for (const entry of ALLOWED) {
      if (!entry.pkg) continue;
      const text = readFileSync(require_.resolve(entry.pkg), "utf8");
      expect(
        text.includes(`.${entry.name}`),
        `${entry.pkg} no longer declares .${entry.name}, so the rule styling it from here matches nothing`,
      ).toBe(true);
    }
  });

  // THE TAB PANEL CARRIES A COLUMN, and nothing else in the suite can see it.
  //
  // A screen's switched sections used to be direct children of `.screen-inner`
  // — a flex column with a gap — and took their vertical rhythm from it.
  // Giving the tab widget a real `role="tabpanel"` put one plain element
  // between the column and them, so without these three declarations every
  // panel on the Seat screen renders with its cards butted together.
  //
  // It fails in the one way nothing catches: the markup stays correct, the
  // roles stay correct, every case in groups.test.tsx stays green, and jsdom
  // computes no layout to assert against. A reader would find it by opening
  // the screen. This is a properties assertion rather than a rendered one for
  // exactly that reason — it is the only place the rule can be checked at all.
  test("the tab panel keeps the column its children lost", () => {
    const block = /\.tabpanel\s*\{([^}]*)\}/.exec(stylesheets());
    expect(block, ".tabpanel is not declared at all").not.toBeNull();
    const body = block![1]!;
    expect(body).toMatch(/display:\s*flex/);
    expect(body).toMatch(/flex-direction:\s*column/);
    expect(body).toMatch(/gap:\s*var\(--space-\d+\)/);
  });
});

// AND EVERY STYLESHEET REACHES THE BROWSER.
//
// The gate above proves a class is DECLARED somewhere under `styles/`. It
// cannot prove the declaration is loaded — and a stylesheet nothing imports is
// invisible to the bundler, so the rules in it never ship. That is exactly how
// the frame's own stylesheet came to pass every check while the application
// rendered with none of it: `frame.css` existed, declared every class the rail,
// the sidebar and the page bar use, and `main.tsx` did not import it, so the
// dashboard loaded with a 1440px rail and a stacked page bar.
//
// A missing import has no other symptom: nothing errors, nothing warns, and
// the page renders — just wrongly, and only where somebody looks.
test("every stylesheet is imported by the entry point", () => {
  const entry = readFileSync(join(SRC, "main.tsx"), "utf8");
  const found = readdirSync(join(SRC, "styles"))
    .filter((f) => f.endsWith(".css"))
    .sort();
  expect(found.length).toBeGreaterThan(0);
  const missing = found.filter((f) => !entry.includes(`./styles/${f}`));
  expect(
    missing,
    "these stylesheets are in the tree and nothing imports them, so none of " +
      "their rules reach the browser",
  ).toEqual([]);
});
