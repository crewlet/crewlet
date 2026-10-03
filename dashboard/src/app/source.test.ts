// @vitest-environment node

/**
 * Rules about the source that no type and no runtime check catches.
 *
 * There is no ESLint in this tree — the gates are prettier, `tsc` and this
 * suite — so a rule that would be a lint elsewhere is a test here, written the
 * way `styles/classes.test.ts` is: read the source, apply the rule, name the
 * file and line.
 */

import { describe, expect, test } from "vitest";

import {
  type Lang,
  type Node,
  isLocalSource,
  lineOf,
  memberName,
  modules,
  parse,
  stringValue,
  walk,
} from "../test/source.ts";
import { ACTIONS } from "~/contract/actions.ts";
import { AGENT_TABS } from "~/routes/agents/seat/profile.ts";
import { FRAMELESS, WORKSPACES } from "./nav.ts";
import { resolves } from "./routes.ts";

/** Every source file of the given extensions, excluding the suites. */
function sources(exts: string[] = [".tsx"]): { path: string; text: string }[] {
  return modules().filter(({ path }) => exts.some((ext) => path.endsWith(ext)));
}

/**
 * A NUMBER IS NOT A GUARD.
 *
 * `{rows.length && <Panel/>}` renders the string "0" when the list is empty,
 * because `0 && x` is `0` and React renders a zero. It is the one JSX mistake
 * that produces no error, no warning and no type failure — the page simply
 * grows a stray digit, unlabelled, in the middle of the content.
 *
 * It shipped: the fleet screen drew a bare `0` beneath its panels whenever
 * nothing was unplaceable, which on a screen an operator reads *looking for a
 * number that is wrong* is the worst possible place for one.
 *
 * The rule is `> 0`, or a ternary. This scans for the guard shapes that are
 * numeric by their own text — anything ending in `.length`, and the count-ish
 * fields this wire format actually carries.
 */
test("no JSX guard is a bare number", () => {
  // `{ <expr> && (` or `{ <expr> && <`, where <expr> ends in a numeric-looking
  // member. `> 0`, `=== 0`, `!== 0` and `> 1` are all fine and excluded by the
  // absence of a comparison in the captured text.
  const guard =
    /\{\s*\(?[^}<>=!]*?\.(length|count|calls|rounds|seats|total|used|unread|primary)\s*(\)\s*)?&&/g;
  const offenders: string[] = [];
  for (const { path, text } of sources()) {
    // COMMENTS BLANKED, LINE NUMBERS KEPT. A comment quoting the bad shape in
    // order to explain why the line beneath it avoids one is not the bad shape
    // — and this gate caught exactly that, so the only way to keep the rule was
    // to stop writing down what it is for.
    const lines = text
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length))
      .split("\n");
    lines.forEach((line, i) => {
      guard.lastIndex = 0;
      if (!guard.test(line)) return;
      // A comparison anywhere in the guard makes it a boolean.
      if (/[<>]=?\s*\d|===|!==/.test(line.slice(0, line.indexOf("&&")))) return;
      offenders.push(`${path}:${i + 1} — ${line.trim().slice(0, 90)}`);
    });
  }
  expect(
    offenders,
    'these render the string "0" when the count is zero: compare with `> 0` or use a ternary',
  ).toEqual([]);
});

/**
 * A HANDLE IS PRINTED THROUGH `handleLabel`, NEVER AS A BARE `@`.
 *
 * `@{seat.handle}` in markup prints a lone `@` for a seat the engine reported
 * no handle for — a claim that the seat HAS a handle and it is empty, which is
 * a statement about the seat rather than about what this client was told. It
 * shipped twice (a common chip and the seat peek) before `lib/seats.ts` grew
 * `handleLabel`, which prints `@pm` or nothing, and the only thing that kept
 * the next one out was memory.
 *
 * Read off the syntax tree, in both spellings markup has: JSX text ending in
 * `@` directly before an expression (`@{handle}`), and a template literal
 * inside a JSX expression whose text before a substitution ends in `@`
 * (`` title={`@${handle}`} ``). A template built OUTSIDE markup — a mention
 * the composer inserts into a draft, a sentence a dialog assembles — is not
 * this rule's subject: it is text a person edits or a value with its own
 * guard, and it never reaches a reader as a lone character in a layout.
 */
function bareAts(text: string, lang: Lang): number[] {
  // A SET, because a template inside a container nested in another container
  // is reached from both, and one offence is one line of the report.
  const out = new Set<number>();
  walk(parse(text, lang), (n) => {
    if (n.type === "JSXElement" || n.type === "JSXFragment") {
      const children = n.children as Node[];
      children.forEach((child, i) => {
        const next = children[i + 1];
        if (
          child.type === "JSXText" &&
          String(child.value).endsWith("@") &&
          next?.type === "JSXExpressionContainer"
        ) {
          out.add(child.end - 1);
        }
      });
    }
    if (n.type === "JSXExpressionContainer") {
      walk(n, (inner) => {
        if (inner.type !== "TemplateLiteral") return;
        const quasis = inner.quasis as Node[];
        quasis.forEach((q, i) => {
          if (i === quasis.length - 1) return;
          const value = q.value as { cooked?: string | null; raw: string };
          if ((value.cooked ?? value.raw).endsWith("@")) out.add(q.end - 1);
        });
      });
    }
  });
  return [...out].sort((a, b) => a - b);
}

test("no markup prints a handle behind a bare @", () => {
  const offenders: string[] = [];
  for (const mod of modules()) {
    if (mod.lang !== "tsx") continue;
    const line = lineOf(mod.text);
    for (const at of bareAts(mod.text, mod.lang)) offenders.push(`${mod.path}:${line(at)}`);
  }
  expect(offenders, "print a handle with handleLabel from lib/seats.ts").toEqual([]);
});

test("the bare-@ reading sees both markup spellings and nothing outside markup", () => {
  const count = (text: string) => bareAts(text, "tsx").length;
  expect(count("const a = <span>@{seat.handle}</span>;")).toBe(1);
  expect(count("const a = <a title={`@${handle}`}>x</a>;")).toBe(1);
  expect(count("const a = <a title={t ?? `@${handle}`}>x</a>;")).toBe(1);
  // Markup INSIDE an expression is still markup, and a template nested two
  // containers deep is one offence, not two.
  expect(count("const a = <div>{open && <span>@{handle}</span>}</div>;")).toBe(1);
  expect(count("const a = <div>{open && <b title={`@${h}`} />}</div>;")).toBe(1);
  // THE CONTROLS: a handle printed through the label, an address that merely
  // contains an @, and a mention a composer builds outside any markup.
  expect(count("const a = <span>{handleLabel(seat.handle)}</span>;")).toBe(0);
  expect(count("const a = <span>ops@example.com</span>;")).toBe(0);
  expect(count("const draft = text.replace(/@\\w*$/, `@${handle} `);")).toBe(0);
});

/**
 * EVERY INTERNAL LINK NAMES A LIVE ROUTE.
 *
 * `href(["seats", handle])` compiles, renders, and takes the reader to a
 * screen that says "there is no such screen" — a dead link with no error, no
 * warning and no type failure, and the only way to find one is to click it.
 *
 * Nine files carried them after the routes moved: an event's link from a phase
 * card, a seat's from a colleague chip, a page's from a search hit, a turn's
 * from an item's history, a project's from an item. Every one of them
 * is a way OUT of the screen a reader is on, which is the half of navigation
 * the sidebar cannot provide.
 *
 * The check is on the FIRST SEGMENT, because that is what route dispatch
 * switches on: a literal that names a segment no workspace owns cannot reach
 * a screen whatever follows it.
 *
 * EVERY SPELLING AND BOTH EXTENSIONS. A link is written as `href([...])`, as
 * `nav.to([...])` for one a control performs rather than one a reader points
 * at, or as a bare `path: [...]` on a value some other component turns into
 * one — and the last lives in plain `.ts`, which is how the ENTIRE attention
 * queue kept pointing at `spend`, `fleet`, `runs`, `config` and `seats` after
 * every one of those moved. That is the Inbox's "needs a person" band: ten
 * rows, all of them dead, in a file this gate was not reading.
 *
 * THEN THE SAME THING HAPPENED AGAIN THROUGH THE SPELLING THIS GATE DID NOT
 * READ. `nav.to` was outside the pattern, so the workspace move left eighteen
 * of them behind — every "Trace" button in the product, every "the turn" from
 * an event, a run and a seat's own list, "Its seat" from a turn and a phase,
 * "the run" from a seat, "spend" from Live now and "back to people" from two
 * screens. Each rendered normally and each landed on Not Found, because the
 * failure of a moved route is a screen that says nothing is there rather than
 * a build that says the link is wrong.
 *
 * AND `path:` IS NOT ONLY A ROUTE. It is also the ENGINE's word for a pointer
 * into the company document — `ConfigProblem.path`, `ConfigWarning.path`,
 * `ConfigReference.path` all carry it, and the org builder's model writes the
 * same shape for a field it sets or forbids: `{ path: ["llm"], credential:
 * false }`, `set: [{ path: ["goal"], value }]`. Those heads are FIELD names
 * and can never be route segments, so reading them here reports twenty-two
 * dead links that are not links at all — and a gate that cries wolf is one
 * somebody switches off, which costs the eighteen real ones above.
 *
 * The two are told apart by what the pointer is FOR, which is on the line:
 * a document pointer names the `value` it sets or marks itself a `credential`
 * field, and a route never does either. The exemption is asserted in both
 * directions below rather than trusted — it has to still fire, and a real nav
 * row has to still be read — because an exemption nobody checks is how a gate
 * quietly stops covering the thing it was written for.
 */

/** Whether a `path:` on this line points into the company document, not at a screen. */
const documentPointer = (line: string) => /\bvalue:|\bcredential:/.test(line);

/**
 * A route written as a literal array, in every spelling one is written in.
 *
 * `href(` and `nav.to(` MAKE a link; a bare `path:` hands one to a component
 * that will; and `useIsCurrent(` and `samePath(` COMPARE against one — which
 * is the spelling that stayed behind when the event log moved under Live: a
 * phase card's "am I on my own event's page" guard kept asking about
 * `["events", id]`, never matched, and drew "event →" to the page it was on.
 * A comparison against an address nothing resolves is a dead link turned
 * inside out, and it hides the same way: no error, no warning, no type
 * failure. `samePath` takes the literal in either position.
 */
const LINK_LITERAL =
  /(?:href\(|nav\.to\(|useIsCurrent\(|samePath\((?:[\w.?!]+\s*,\s*)?|\bpath:\s*)\[\s*"([a-z0-9_-]+)"([^\]]*)\]/g;

test("the link pattern reads every spelling a route literal is written in", () => {
  const heads = (line: string) => [...line.matchAll(LINK_LITERAL)].map((m) => m[1]);
  expect(heads('<a href={href(["live", "turns"])}>')).toEqual(["live"]);
  expect(heads('nav.to(["agents", "roster"])')).toEqual(["agents"]);
  expect(heads('  path: ["settings", "config"],')).toEqual(["settings"]);
  expect(heads('const here = useIsCurrent(["events", record.eventId]);')).toEqual(["events"]);
  expect(heads('samePath(route.path, ["events", id])')).toEqual(["events"]);
  expect(heads('samePath(["events", id], route.path)')).toEqual(["events"]);
});

/** Every link in one module's text that reaches no screen, as `line — #/path`. */
function deadLinks(text: string): string[] {
  // OR A SCREEN OUTSIDE THE FRAME, which no workspace owns and the router
  // draws all the same — the sign-in screens a refusal sends a reader to.
  const owned = new Set<string>([...WORKSPACES.map((w) => w.path[0] ?? ""), ...FRAMELESS]);
  const literal = new RegExp(LINK_LITERAL.source, LINK_LITERAL.flags);
  const whole = /^(?:\s*,\s*"[^"]*")*\s*,?\s*$/;
  const dead: string[] = [];
  text.split("\n").forEach((line, i) => {
    literal.lastIndex = 0;
    let m: RegExpExecArray | null;
    while ((m = literal.exec(line))) {
      const head = m[1]!;
      const tail = m[2] ?? "";
      // `href(` and `nav.to(` are unambiguous; only the bare `path:`
      // spelling collides with the document pointer.
      if (m[0].startsWith("path") && documentPointer(line)) continue;
      if (!owned.has(head)) {
        dead.push(`${i + 1} — #/${head}`);
        continue;
      }
      // A PATH WRITTEN WHOLLY IN LITERALS is checked against the resolver
      // itself, because a known head is not a known route: `["live",
      // "traces"]` has one and drew the turns list under a traces title.
      if (whole.test(tail)) {
        const segs = [head, ...[...tail.matchAll(/"([^"]*)"/g)].map((x) => x[1]!)];
        if (!resolves(segs)) dead.push(`${i + 1} — #/${segs.join("/")}`);
      }
    }
  });
  return dead;
}

test("every link names a segment a workspace owns, and a whole literal path resolves", () => {
  const dead: string[] = [];
  for (const { path, text } of sources([".tsx", ".ts"])) {
    for (const hit of deadLinks(text)) dead.push(`${path}:${hit}`);
  }
  expect(dead, "these links go to a screen that does not exist").toEqual([]);
});

/**
 * THE HEADS THE REBUILD RENAMED ARE DEAD, AND THE RULE SAYS SO.
 *
 * Every workspace moved once: the event log from `#/activity` to
 * `#/live/events`, spend from `#/cost` to `#/spend`, the fleet, the runs, the
 * configuration and the seats under Settings, Live and Agents, and pages from
 * `#/pages` to `#/knowledge/pages`. A link still written against one of those
 * resolves to nothing, and the rule above is only as good as its reading of
 * that — so each retired head is fed through it here, beside the address it
 * became, which has to come back clean. A rule that stopped reporting the old
 * head, or started reporting the new one, fails here rather than letting a
 * whole family of dead links through.
 */
test("a link to a head the rebuild renamed is reported, and its new address is not", () => {
  const moves: [retired: string, current: string][] = [
    ['href(["activity"])', 'href(["live", "events"])'],
    ['href(["cost"])', 'href(["spend"])'],
    ['nav.to(["fleet"])', 'nav.to(["settings", "nodes"])'],
    ['nav.to(["runs"])', 'nav.to(["live", "runs"])'],
    ['  path: ["config"],', '  path: ["settings", "config"],'],
    ['href(["seats", handle])', 'href(["agents", "seats", handle])'],
    ['href(["pages", id])', 'href(["knowledge", "pages", id])'],
    ['href(["company"])', 'href(["home"])'],
    ['href(["admin"])', 'href(["settings"])'],
  ];
  for (const [retired, current] of moves) {
    expect(deadLinks(retired), retired).toHaveLength(1);
    expect(deadLinks(current), current).toEqual([]);
  }
});

/**
 * A LINK'S FILTERS ARE READ BY THE SCREEN IT OPENS.
 *
 * A link that names a live route and carries a query the screen there never
 * reads resolves, renders, and silently drops what it was for. Four did after
 * the event log moved from `#/activity` to `#/live/events`: "Its events" on a
 * seat, "In the log" on a trace, "Read this channel's events" on an
 * agent-to-agent channel and Live's own "Event log" button were rewritten to
 * the workspace's head, `#/live` — which is Live's landing screen, not the log
 * — so each opened the running seats with its `actor=`, `q=` and `category=`
 * thrown away.
 *
 * Held for the screens whose every parameter is read in ONE module, which is
 * what makes "reads" exact rather than a guess over an import graph: the table
 * names the module, and the parameters are its own `useParam("…")` literals.
 */
const READS_ITS_PARAMS: { path: string[]; module: string }[] = [
  { path: ["live"], module: "routes/live/LiveNow.tsx" },
  { path: ["live", "events"], module: "routes/live/Activity.tsx" },
];

/** Every link written as a literal path with a literal query, in one module. */
function linksWithQuery(
  text: string,
  lang: Lang,
): { path: string[]; keys: string[]; at: number }[] {
  const out: { path: string[]; keys: string[]; at: number }[] = [];
  walk(parse(text, lang), (n) => {
    if (n.type !== "CallExpression") return;
    const callee = n.callee as Node;
    const named =
      (callee.type === "Identifier" && callee.name === "href") ||
      (memberName(callee) === "to" && (callee.object as Node).type === "Identifier");
    if (!named) return;
    const [first, second] = n.arguments as Node[];
    if (first?.type !== "ArrayExpression") return;
    const path = (first.elements as Node[]).map((e) => stringValue(e));
    if (path.some((seg) => seg === null)) return;
    const keys =
      second?.type === "ObjectExpression"
        ? (second.properties as Node[]).flatMap((prop) => {
            if (prop.type !== "Property") return [];
            const key = prop.key as Node;
            return key.type === "Identifier" ? [String(key.name)] : [stringValue(key) ?? ""];
          })
        : [];
    out.push({ path: path as string[], keys, at: n.start });
  });
  return out;
}

test("a link's query names only what the screen it opens reads", () => {
  const byPath = new Map(
    READS_ITS_PARAMS.map((row) => {
      const mod = modules().find((m) => m.path === row.module);
      expect(mod, `${row.module} is not in the tree — this row is asserting nothing`).toBeDefined();
      const read = [...(mod?.text ?? "").matchAll(/useParam\(\s*"([^"]+)"/g)].map((m) => m[1]!);
      return [row.path.join("/"), new Set(read)] as const;
    }),
  );
  const dropped: string[] = [];
  let held = 0;
  for (const mod of modules()) {
    if (mod.lang === "dts") continue;
    const line = lineOf(mod.text);
    for (const link of linksWithQuery(mod.text, mod.lang)) {
      const reads = byPath.get(link.path.join("/"));
      if (!reads) continue;
      held++;
      for (const key of link.keys) {
        if (!reads.has(key)) {
          dropped.push(
            `${mod.path}:${line(link.at)} — #/${link.path.join("/")} never reads ${key}=`,
          );
        }
      }
    }
  }
  expect(dropped, "these links carry a filter the screen they open throws away").toEqual([]);
  // NOT VACUOUS: the table's screens are linked to with a query somewhere.
  expect(held).toBeGreaterThan(0);
});

test("the query reading sees both link spellings", () => {
  const read = (text: string) => linksWithQuery(text, "tsx").map((l) => [l.path, l.keys]);
  expect(read('const a = href(["live"], { actor: who });')).toEqual([[["live"], ["actor"]]]);
  expect(read('nav.to(["live", "events"], { q: id, "category": "a2a" });')).toEqual([
    [
      ["live", "events"],
      ["q", "category"],
    ],
  ]);
  // A path that is not wholly literal is not read, rather than guessed at.
  expect(read("href(pathOf(ref), { actor: who });")).toEqual([]);
});

/*
 * BOTH SIDES OF THAT EXEMPTION. It must still let a document pointer through
 * — otherwise it has stopped mattering and the next reader deletes it — and
 * it must NOT swallow a nav destination, which is the failure that would make
 * the check above silently cover less than it claims.
 */
test("the document-pointer exemption fires, and spares no real link", () => {
  // The shapes routes/org/builder/model writes.
  expect(documentPointer('  { path: ["llm"], credential: false },')).toBe(true);
  expect(documentPointer('    set: [{ path: ["goal"], value: "Ship" }],')).toBe(true);
  // The shapes a nav destination is written in, across all four tables.
  expect(documentPointer('      path: ["settings", "config"],')).toBe(false);
  expect(documentPointer('        { label: "Work", path: ["work"] },')).toBe(false);
  expect(documentPointer('    path: ["live", "turns"],')).toBe(false);
  expect(documentPointer('      href(["agents", "seats", handle])')).toBe(false);
});

test("something in the tree is actually exempted, so the rule is not dead weight", () => {
  const exempted = sources([".tsx", ".ts"]).flatMap(({ path, text }) =>
    text
      .split("\n")
      .map((line, i) => ({ line, at: `${path}:${i + 1}` }))
      .filter(({ line }) => /\bpath:\s*\[\s*"/.test(line) && documentPointer(line)),
  );
  expect(
    exempted.length,
    "nothing writes a document pointer any more — delete documentPointer and read the `path:` spelling straight",
  ).toBeGreaterThan(0);
});

/**
 * Every `<Tag …/>` element's own source text, tag to closing `/>`.
 *
 * BRACE-COUNTED rather than line-matched, because the captions this exists for
 * are multi-line ternaries: a per-line scan reads `sub={` and `.join(` as
 * unrelated lines and reports none of them.
 */
function elements(text: string, tag: string): { at: number; text: string }[] {
  const out: { at: number; text: string }[] = [];
  let i = 0;
  while ((i = text.indexOf(`<${tag}`, i)) >= 0) {
    // `<StatCardish` is not `<StatCard`.
    if (!/[\s/>]/.test(text[i + tag.length + 1] ?? "")) {
      i += 1;
      continue;
    }
    let depth = 0;
    let j = i + tag.length + 1;
    for (; j < text.length; j++) {
      const c = text[j];
      if (c === "{") depth++;
      else if (c === "}") depth--;
      else if (depth === 0 && c === "/" && text[j + 1] === ">") {
        j += 2;
        break;
      } else if (depth === 0 && c === ">") {
        j += 1;
        break;
      }
    }
    out.push({ at: i, text: text.slice(i, j) });
    i = j;
  }
  return out;
}

/**
 * A STAT TILE'S CAPTION QUALIFIES ITS NUMBER; IT NEVER LISTS.
 *
 * `StatCard`'s `sub` is ONE line — the design system draws it `white-space:
 * nowrap` with `text-overflow: ellipsis` — so what goes there has to be short by
 * CONSTRUCTION, not short in today's data. Four tiles joined names a founder
 * writes: the goals screen's at-risk tile, both of the schedules screen's, and
 * Live now's "working now". The goals one shipped reading "Console reads
 * honestly under uncertainty, Console reads hon…" — cut mid-word, naming an
 * outcome that does not exist, beside neighbours reading "not archived" and
 * "across every goal on screen". (That screen left with goals; the gate is
 * what stops the next tile repeating it.)
 *
 * BOUNDING THE COUNT IS THE WRONG FIX and is why this is a gate rather than four
 * corrected lines: a name a founder writes is bounded at 256 characters
 * (`tracker.MaxTitle`), so ONE of them overflows a third-of-a-column tile and
 * "first two, +N more" keeps the same cut. The caption has to be a derived
 * qualifier — a split, a relative time, a pointer at the list that does hold the
 * names — which is what the other fifty-odd tiles in this tree write.
 */
test("no stat tile builds its caption by joining a list", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources()) {
    for (const el of elements(text, "StatCard")) {
      seen++;
      if (!el.text.includes(".join(")) continue;
      const line = text.slice(0, el.at).split("\n").length;
      offenders.push(`${path}:${line} — ${el.text.split("\n")[2]?.trim() ?? ""}`);
    }
  }
  expect(
    offenders,
    "a `sub` is one ellipsized line: say what the number counts, not which rows are in it",
  ).toEqual([]);
  // THE OTHER SIDE. A renamed component or a broken scan makes this rule vacuous
  // while still reporting a pass — the failure `skipgate` exists to stop
  // elsewhere. Fifty-odd tiles today; a runaway parse collapses to one per file
  // and a rename to zero, so thirty separates both from reality.
  expect(
    seen,
    "nothing here reads as a StatCard any more — this rule covers nothing",
  ).toBeGreaterThan(30);
});

/**
 * A QUERYSTATE HANDED A FAILURE IS HANDED WHY.
 *
 * `QueryState` turns an `unauthorized` into the grant that would admit the
 * reader, and an `unavailable` the state log will not lift into that fact —
 * but only from the `refusal` beside the code, which `useQuery` answers and a
 * screen has to pass on. Five did not (a container's pages, a tool's holders,
 * a schedule's runs twice, the fleet's table), so each drew the generic banner
 * where the engine had said exactly what would change the answer. Nothing else
 * catches it: `refusal` is optional because a surface with no query behind it
 * has none, so an omission type-checks and renders.
 *
 * `error={null}` is exempt, being no failure at all.
 */
test("every QueryState handed an error is handed its refusal", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources()) {
    for (const el of elements(text, "QueryState")) {
      const error = /\berror=\{([^}]*)\}/.exec(el.text);
      if (!error || error[1]!.trim() === "null") continue;
      seen++;
      if (/\brefusal=\{/.test(el.text)) continue;
      const line = text.slice(0, el.at).split("\n").length;
      offenders.push(`${path}:${line} — error={${error[1]!.trim()}}`);
    }
  }
  expect(offenders, "pass the read's `refusal` beside its `error`").toEqual([]);
  // THE OTHER SIDE: a renamed component or a broken scan makes the rule
  // vacuous and still green. Seventy-odd of them today.
  expect(seen, "nothing here reads as a QueryState any more").toBeGreaterThan(40);
});

/*
 * AND THE SCANNER ACTUALLY READS THE SHAPE THE RULE IS FOR. A gate that only
 * ever saw single-line props would pass a reverted multi-line ternary — which is
 * three of the four captions above.
 */
test("the element scanner reads a multi-line prop", () => {
  const src =
    '<StatCard\n  label="x"\n  sub={\n    a.length ? a.map((x) => x.n).join(", ") : "none"\n  }\n/>\n<StatCard label="y" sub="fixed" />';
  const found = elements(src, "StatCard");
  expect(found.length).toBe(2);
  expect(found[0]?.text.includes(".join(")).toBe(true);
  expect(found[1]?.text.includes(".join(")).toBe(false);
});

/**
 * A COLUMN WHOSE HEAD IS A GLYPH STILL HAS A NAME.
 *
 * `GridColumn.header` is drawn in the head row, so a column twenty pixels wide
 * carries a mark or nothing at all — a work item's type, a row's actions, a
 * pair of state tags. That is right for the wide table and wrong for the card a
 * grid becomes below 860px, where the head is gone and every value draws its
 * own name beside it: a cell with no name draws none, and the card gets a bare
 * mark floating on a line of its own between two labelled ones. Measured on the
 * tracker at 390px, it reads as a rendering fault rather than as a value.
 *
 * So the column says the word separately, in `label`. NOTHING ELSE CATCHES
 * THIS: the field is optional by necessity — a column with a real head must not
 * repeat it — so a new glyph column type-checks, renders, and is wrong only on
 * a phone, which no suite in this tree has a viewport for.
 *
 * THE SIBLINGS ARE FOUND BY INDENTATION, which is a real invariant here rather
 * than a guess: prettier is a gate (`make dashboard-lint`), so a literal's own
 * properties share a column and everything nested under one is indented past
 * it. Brace-counting would be the obvious alternative and is the worse one — a
 * cell is JSX with children, template strings and comments in it, and a counter
 * walking through those answers confidently and wrongly. `typescript` is not
 * the alternative either: at 7.x the package exports its syntax tree only under
 * `unstable/`, and a permanent gate does not rest on that.
 */
test("a column with no word in its head declares one", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources([".tsx"])) {
    const lines = text.split("\n");
    for (let at = 0; at < lines.length; at++) {
      // A PROPERTY, NOT A TYPE MEMBER. `GridColumn`'s own `header: ReactNode;`
      // declares the contract rather than filling it in, and prettier ends one
      // in a semicolon and the other in a comma.
      const head = /^(\s*)header:\s*(.*[^;])$/.exec(lines[at]!);
      if (!head) continue;
      seen++;
      // A WORD IS A NON-EMPTY STRING LITERAL. `""`, a glyph and any expression
      // are all the same thing to `attr()`: nothing to read.
      if (/^(["'])(?!\1)\S/.test(head[2]!)) continue;
      const indent = head[1]!;
      const siblings: string[] = [];
      for (const step of [-1, 1]) {
        for (let n = at + step; n >= 0 && n < lines.length; n += step) {
          const line = lines[n]!;
          if (!line.trim()) continue;
          if (!line.startsWith(indent)) break;
          if (line[indent.length] === " ") continue;
          siblings.push(line);
        }
      }
      if (siblings.some((line) => /^\s*label:\s*["']\S/.test(line))) continue;
      offenders.push(`${path}:${at + 1}`);
    }
  }
  expect(
    offenders,
    "a glyph or empty head leaves the phone's card layout an unnamed line: add `label`",
  ).toEqual([]);
  // THE OTHER SIDE, as above: a renamed field or a broken scan makes the rule
  // vacuous and still green. Well over a hundred columns today.
  expect(seen, "nothing here declares a column head any more").toBeGreaterThan(80);
});

/**
 * EVERY `Intl` FORMATTER IS BUILT IN `lib/format.ts`, AND KEPT.
 *
 * `d.toLocaleString(locale, options)`, `toLocaleDateString`,
 * `toLocaleTimeString`, `n.toLocaleString()` and `a.localeCompare(b, locale,
 * options)` each build an `Intl` object PER CALL — ECMA-402 defines them as
 * that construction followed by one format or compare — and building one is
 * the expensive half. Spelled inline they were most of what formatting cost
 * on a busy screen: a hundred-row audit built three hundred date formatters a
 * render, and a grid sorted by a text column built a collator per comparison.
 * `lib/format.ts` keeps one per locale and options (`dateFormatter`), one
 * number formatter and one collator (`naturalCompare`); `lib/prefs.ts` is the
 * other file allowed one, to ask `Intl` which zone the browser is in and
 * whether a zone exists.
 *
 * A bare `a.localeCompare(b)` is allowed: with no locale and no options the
 * engine compares through its own default collator rather than building one.
 */
const INTL_BUILT =
  /\.toLocale(Date|Time)?String\(|\bnew Intl\.|\bIntl\.[A-Z]\w*\(|\.localeCompare\([^\n]*?,\s*(undefined|["'[{])/;

test("every Intl formatter is built in lib/format.ts, and kept", () => {
  const allowed = new Set(["lib/format.ts", "lib/prefs.ts"]);
  const offenders: string[] = [];
  let read = 0;
  for (const { path, text } of sources([".ts", ".tsx"])) {
    read += 1;
    if (allowed.has(path)) continue;
    const lines = text
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length))
      .split("\n");
    lines.forEach((line, i) => {
      if (INTL_BUILT.test(line)) offenders.push(`${path}:${i + 1} — ${line.trim().slice(0, 90)}`);
    });
  }
  // A WALK THAT READ NOTHING passes with no offence found.
  expect(read).toBeGreaterThan(100);
  expect(
    offenders,
    "format through lib/format.ts (dateFormatter, fmtExact, plural, naturalCompare), which keeps the formatter",
  ).toEqual([]);
});

test("the Intl gate fires on each spelling it is for", () => {
  for (const line of [
    "d.toLocaleString(undefined, { hour: '2-digit' })",
    "d.toLocaleDateString(undefined, { day: 'numeric' })",
    "d.toLocaleTimeString()",
    "n.toLocaleString()",
    "new Intl.NumberFormat()",
    "Intl.DateTimeFormat().resolvedOptions()",
    "String(a).localeCompare(String(b), undefined, { numeric: true })",
  ]) {
    expect(INTL_BUILT.test(line), line).toBe(true);
  }
  expect(INTL_BUILT.test("a.name.localeCompare(b.name)")).toBe(false);
  expect(INTL_BUILT.test("dateFormatter(undefined, { day: 'numeric' }).format(d)")).toBe(false);
});

/**
 * AN ITEM IS OPENED BY ITS ADDRESS, NEVER BY ITS KEY.
 *
 * A key two tasks hold opens the one that claimed it first — the engine
 * resolves a key through its directory before it reads a row — and the other
 * is flagged `key_collision` and reached only by its id. Every screen that
 * opened an item built the link, the peek, the stepper's list, the "is this
 * the open one" check and the copied call out of `row.key`: the board, the
 * list, the table, the timeline, the calendar, the search, the palette, the
 * feeds, the inbox and My work all drew two `ENG-7`s that opened one task,
 * with no error anywhere, because a key is a perfectly good address for every
 * row but the flagged one.
 *
 * `itemAddress` (in `lib/work.ts`) is the one place the rule is written, and
 * this is what keeps a NEW screen from writing it again by hand. Four shapes
 * are refused anywhere in the source, each a way the key leaks into an
 * address:
 *
 * - a `["work", …]` route whose segment reads a key field — `row.key`,
 *   `record.subject_key`, `item.task_key`, `link.key || link.other`. An item's
 *   route is `itemPath(row)`, and a project's is `projectPath(key)` precisely
 *   so that no route into the tracker is spelled with a key at the call site;
 * - an `{ kind: "item", id: … }` reference — a peek, a stepper entry, a copied
 *   call's subject — whose id is anything but `itemAddress(…)`, in either
 *   order of its two properties;
 * - a comparison of a key field against `selected` or `peek.id`, which is how
 *   a list draws the open row — the peek holds an ADDRESS, so a row matched
 *   on its key lit up both holders of a shared one;
 * - a tool call naming its `item:` by a key field.
 *
 * Comments are blanked first, so a comment quoting the wrong shape to explain
 * the right one is not an offence.
 */
const KEY_READ = String.raw`[\w$\])?]\.(?:key|subject_key|task_key)\b`;
const ADDRESS_RULES: { rule: RegExp; says: string }[] = [
  {
    rule: new RegExp(String.raw`\[\s*"work"\s*,[^\]]*?` + KEY_READ),
    says: "routes to an item by its key — use itemPath(row)",
  },
  {
    // THE LOOKAHEAD SITS ON THE COLON, not after the spaces: placed after
    // them, the engine backtracks the spaces away and the lookahead then sees
    // " itemAddress(", which is not `itemAddress(`, so every right answer was
    // refused as well.
    rule: /\bkind:\s*"item"(?:\s+as\s+const)?\s*,\s*id:(?!\s*itemAddress\()/,
    says: "names an item by something other than itemAddress(row)",
  },
  {
    rule: /\bid:(?!\s*itemAddress\()[^,{}]*,\s*kind:\s*"item"(?!\s*\|)/,
    says: "names an item by something other than itemAddress(row)",
  },
  {
    rule: new RegExp(
      String.raw`(?:\bselected|\bpeek\??\.id)\s*[!=]==\s*[\w$?.\[\]]*` +
        KEY_READ +
        String.raw`|[\w$?.\[\]]*` +
        KEY_READ +
        String.raw`\s*[!=]==\s*(?:\bselected\b|\bpeek\??\.id\b)`,
    ),
    says: "matches the open item on its key — compare itemAddress(row)",
  },
  {
    rule: new RegExp(String.raw`\bitem:\s*[\w$?.\[\]]*` + KEY_READ),
    says: "names a tool call's item by its key — use itemAddress(row)",
  },
];

/** A file's code with its comments blanked and every offset kept. */
function codeOf(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
    .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length));
}

/** Every place one file's code addresses an item by hand. */
function addressOffences(code: string): { line: number; says: string; text: string }[] {
  const out: { line: number; says: string; text: string }[] = [];
  const lines = code.split("\n");
  for (const { rule, says } of ADDRESS_RULES) {
    // ACROSS LINES, because the shapes are written across lines: a
    // multi-line `{ kind: "item", id: … }` is how a copied call's subject is
    // spelled, and a per-line scan reads its `id:` as an unrelated line.
    const global = new RegExp(rule.source, "g");
    let m: RegExpExecArray | null;
    while ((m = global.exec(code))) {
      const line = code.slice(0, m.index).split("\n").length;
      out.push({ line, says, text: (lines[line - 1] ?? "").trim().slice(0, 90) });
      global.lastIndex = m.index + Math.max(1, m[0].length);
    }
  }
  return out;
}

/**
 * Every `<DataGrid …>` element's own source text — its generic argument
 * stepped over, so `<DataGrid<Row>` is read as the element it is rather than
 * ended at the generic's `>`, which is where [elements] would end it.
 */
function grids(text: string): { at: number; text: string }[] {
  const out: { at: number; text: string }[] = [];
  const tag = "<DataGrid";
  let i = 0;
  while ((i = text.indexOf(tag, i)) >= 0) {
    let j = i + tag.length;
    if (text[j] === "<") {
      for (let angle = 0; j < text.length; j++) {
        if (text[j] === "<") angle++;
        else if (text[j] === ">" && --angle === 0) {
          j++;
          break;
        }
      }
    } else if (!/[\s/>]/.test(text[j] ?? "")) {
      i = j;
      continue;
    }
    let depth = 0;
    for (; j < text.length; j++) {
      const c = text[j];
      if (c === "{") depth++;
      else if (c === "}") depth--;
      else if (depth === 0 && c === "/" && text[j + 1] === ">") {
        j += 2;
        break;
      } else if (depth === 0 && c === ">") {
        j += 1;
        break;
      }
    }
    out.push({ at: i, text: text.slice(i, j) });
    i = j;
  }
  return out;
}

test("no screen opens an item by its key", () => {
  const offenders: string[] = [];
  let read = 0;
  let addressed = 0;
  for (const { path, text } of sources([".ts", ".tsx"])) {
    read += 1;
    const code = codeOf(text);
    addressed += (code.match(/\bitem(?:Address|Path)\(/g) ?? []).length;
    for (const o of addressOffences(code)) {
      offenders.push(`${path}:${o.line} ${o.says} — ${o.text}`);
    }
  }
  // A WALK THAT READ NOTHING passes with no offence found.
  expect(read).toBeGreaterThan(100);
  expect(
    offenders,
    "a key another task claimed first opens that task: address an item with itemAddress / itemPath from lib/work.ts",
  ).toEqual([]);
  // AND SO DOES A TREE IN WHICH NOTHING ADDRESSES AN ITEM AT ALL — which is
  // what a rename of the function would leave behind, every rule above then
  // refusing the new spelling's absence rather than anything a screen does.
  expect(addressed, "nothing in the tree calls itemAddress or itemPath").toBeGreaterThan(20);
});

test("the address gate fires on each way a key leaks into an address, and on nothing else", () => {
  for (const bad of [
    'href(["work", row.key])',
    'nav.to(["work", item.key])',
    'path: ["work", record.subject_key],',
    'href(["work", item.task_key])',
    ': ["work", link.key || link.other],',
    'openPeek({ kind: "item", id: row.key })',
    'rows.map((r) => ({ kind: "item" as const, id: r.key }))',
    'peekHref({ kind: "item", id: hit.key || hit.id })',
    '{\n  kind: "item",\n  id: detail.task.key,\n}',
    '({ id: r.key, kind: "item" })',
    "selected={selected === row.key}",
    "isSelected={(row) => row.key === selected}",
    "isSelected={(r) => peek?.id === r.key}",
    'cx("tl-bar", selected === bar.row.key && "selected")',
    "args: { item: row.key },",
  ]) {
    expect(addressOffences(bad).length, bad).toBeGreaterThan(0);
  }
  for (const good of [
    "href(itemPath(row))",
    'href(["work", itemAddress(row)])',
    "href(projectPath(p.key))",
    'path: ["work", projectKey]',
    'openPeek({ kind: "item", id: itemAddress(row) })',
    '({ kind: "item" as const, id: itemAddress(r) })',
    '{\n  kind: "item",\n  id: itemAddress(detailItem(detail)),\n}',
    'kind: "item" | "page" | "seat";',
    "isSelected={(row) => itemAddress(row) === selected}",
    'selected === itemAddress(bar.row) && "selected"',
    "group.key === key",
    'openPeek({ kind: "project", id: row.key })',
    "args: { item: itemAddress(row) },",
  ]) {
    expect(addressOffences(good), good).toEqual([]);
  }
});

/** What an element hands `columns={…}`, brace-counted, or null where it hands none. */
function columnsOf(element: string): string | null {
  const start = element.indexOf("columns={");
  if (start < 0) return null;
  let depth = 0;
  for (let j = start + "columns=".length; j < element.length; j++) {
    if (element[j] === "{") depth++;
    else if (element[j] === "}" && --depth === 0) {
      return element.slice(start + "columns={".length, j);
    }
  }
  return null;
}

/**
 * Why a grid's `columns` is not a value that holds still, or null where it is.
 *
 * HELD STILL means a name declared once at module scope or with `useMemo` in
 * the file that hands it over; anything else is a new list on every render.
 */
export function columnsOffence(source: string, value: string): string | null {
  const name = value.trim();
  if (!/^[A-Za-z_$][\w$]*$/.test(name)) return "an expression, built again on every render";
  const declared = new RegExp(
    String.raw`^([ \t]*)(?:export\s+)?const\s+${name}\b[^=\n]*=\s*(.*)$`,
    "gm",
  );
  const found = [...source.matchAll(declared)];
  if (found.length === 0) return `\`${name}\` is not declared in this file as a held value`;
  for (const [, indent, rest] of found) {
    if (indent === "") continue;
    if (/^useMemo\s*[(<]/.test(rest ?? "")) continue;
    return `\`${name}\` is declared without useMemo, so it is a new list on every render`;
  }
  return null;
}

/**
 * A GRID'S COLUMNS ARE A VALUE THAT HOLDS STILL.
 *
 * Every row of a grid is memoised on the column list it is handed
 * (`app/frame/DataGrid.tsx`), and rightly: a column closing over something new
 * may draw something new. So a list built inline — `columns={[…]}`, or a
 * `const` declared in the render without `useMemo` — is a new list on every
 * render of the screen, and every such render draws every row: a seat's live
 * state moving, a clock-driven header, a filter typed above the grid, a poll
 * that changed one row. Twenty-seven grids on twenty screens were built that
 * way, the turns list among them, where one changed turn drew all two hundred.
 *
 * NOTHING ELSE CATCHES IT: an inline list type-checks, renders correctly and
 * is wrong only in how much work every render does. So the value a grid is
 * handed must be a name this file declares at module scope or with `useMemo`.
 * A `useMemo` whose dependencies change on every render passes this and is
 * the same defect — which is what the screens' own poll cases are for.
 */
test("every grid is handed a column list that holds still", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources([".tsx"])) {
    for (const grid of grids(text)) {
      const value = columnsOf(grid.text);
      if (value === null) continue;
      seen++;
      const offence = columnsOffence(text, value);
      if (offence === null) continue;
      const line = text.slice(0, grid.at).split("\n").length;
      offenders.push(`${path}:${line} — ${offence}`);
    }
  }
  expect(
    offenders,
    "hold the columns in a useMemo on what they read, or at module scope: an inline list draws every row on every render",
  ).toEqual([]);
  // THE OTHER SIDE: a renamed component or a broken scan makes the rule vacuous
  // and still green. Thirty-odd grids today.
  expect(seen, "nothing here reads as a DataGrid handed columns any more").toBeGreaterThan(25);
});

test("the column gate fires on each spelling it is for, and passes a held list", () => {
  const offence = (source: string) => {
    const [grid] = grids(source);
    return columnsOffence(source, columnsOf(grid!.text) ?? "");
  };
  // INLINE, GENERIC OR NOT.
  expect(offence('<DataGrid<Row>\n  rows={rows}\n  columns={[{ key: "a" }]}\n/>')).not.toBeNull();
  expect(offence('<DataGrid rows={rows} columns={[{ key: "a" }]} />')).not.toBeNull();
  // AN EXPRESSION IS BUILT EVERY RENDER TOO — a call, a ternary.
  expect(offence("<DataGrid columns={build(ctx)} />")).not.toBeNull();
  expect(offence("<DataGrid columns={wide ? a : b} />")).not.toBeNull();
  // A NAME DECLARED IN THE RENDER WITHOUT `useMemo` is the same list inline.
  expect(
    offence(
      "function S() {\n  const columns = [{ key: 'a' }];\n  return <DataGrid columns={columns} />;\n}",
    ),
  ).not.toBeNull();
  // AND WHAT HOLDS STILL PASSES: a module constant, and a `useMemo`.
  expect(offence("const COLUMNS = [];\nconst g = <DataGrid columns={COLUMNS} />;")).toBeNull();
  expect(
    offence(
      "function S() {\n  const columns = useMemo<GridColumn<Row>[]>(() => [], []);\n  return <DataGrid<Row> rows={r} columns={columns} />;\n}",
    ),
  ).toBeNull();
});

/**
 * EVERY READ NAMES ITS QUESTION.
 *
 * The first argument of a call that asks the engine something — `useQuery(`
 * and `query(`, bare or as a method — is a string literal: never a variable,
 * an expression, or a template with something substituted into it.
 *
 * THE ENGINE'S GATES READ THESE CALLS BY NAME, and a call they cannot read is
 * one they do not see. `internal/api/queries` holds this tree against the
 * registry in both directions — `TestEveryQueryARoomMakesIsAnswered` and
 * `TestEveryQueryThisServerAnswersHasAReader` — and takes the kinds it asks
 * from `clientsource.Calls`, which reads a first argument only when it is a
 * constant string. So a kind passed through a variable is a question neither
 * gate knows about: the engine could stop answering it and the build would
 * stay green, and a kind whose last literal reader became a variable would be
 * reported unread and deleted from under the screen that still asks it. Held
 * here, that blindness costs nothing, because there is nothing for it to miss.
 *
 * `useQuery` AND `query` ARE READ BY SPELLING, because that is how
 * `clientsource.Calls` reads them: any call to either name, bare or as a
 * method, whatever it is bound to. Reading less than that here would leave a
 * call both sides skip — `const { query } = socket; query(kind)` is a bare
 * call to the socket's method, and a rule that knew only `.query(` passed it
 * while the Go reader, finding no literal, dropped it. Reading the same set is
 * what makes "the Go gates can read every kind" a fact rather than a hope.
 * And a method named by a computed key (`socket["query"](…)`) is refused even
 * with a literal kind, because `Calls` reads a NAME, and a string in brackets
 * is not one.
 *
 * `act` AND `useAct` ARE READ BY BINDING: the functions imported from the
 * module of this tree that DEFINES each ([BOUND]) — not from any module of
 * it, because the test door (`test/inCase.ts`) re-exports the testing
 * library's `act` under the same name, and every suite and test kit calls
 * that one with a callback. `act` is the
 * write surface's entry (`protocol/act.ts`) and `useAct` the hook every
 * screen reaches it through (`lib/useAct.ts`); each names a TOOL the way a
 * query names a kind, and `internal/api/operator` holds the tools the
 * dashboard may name (`contract/actions.ts`) against the engine's catalogue.
 * An ALIAS of any of them is refused (`import { useQuery as ask }`), because
 * every call behind it is invisible to a reader that looks for the name.
 *
 * TWO CALLS FORWARD A KIND, and each is named in `FORWARDS` with its reason
 * rather than exempted by its shape. An entry that stops matching is stale and
 * fails, because an exemption nobody checks is how a gate quietly stops
 * covering what it was written for.
 */

/** Names the engine's gates read by spelling: `clientsource.Calls` is asked for exactly these. */
const SPELLED = new Set(["useQuery", "query"]);
/**
 * Names held by binding, to the modules under `src/` that define them —
 * because a package exports one of the same spelling, and so does the test
 * door that hands every suite the testing library.
 */
const BOUND: ReadonlyMap<string, readonly string[]> = new Map([
  ["act", ["protocol/act.ts", "protocol/index.ts"]],
  ["useAct", ["lib/useAct.ts"]],
]);

/** Where a `~/` or relative import from the module at `from` lands, under `src/`. */
function importTarget(from: string, source: string): string {
  if (source.startsWith("~/")) return source.slice(2);
  const parts = from.split("/").slice(0, -1);
  for (const segment of source.split("/")) {
    if (segment === "..") parts.pop();
    else if (segment !== ".") parts.push(segment);
  }
  return parts.join("/");
}

/** Whether `name`, imported from `target`, is the definition [BOUND] holds. */
const boundTo = (name: string, target: string | undefined) =>
  target !== undefined && (BOUND.get(name)?.includes(target) ?? false);

/** The sites that hand on a kind somebody else named, and why. */
const FORWARDS: readonly { path: string; callee: string; argument: string; why: string }[] = [
  {
    path: "lib/useQuery.ts",
    callee: "query",
    argument: "what",
    why: "useQuery's own body sends the socket the kind its caller named, and every caller is held to a literal here",
  },
  {
    path: "lib/useAct.ts",
    callee: "act",
    argument: "tool",
    why: "useAct's own body sends the tool its caller named, and every useAct( is held to a literal here",
  },
  {
    path: "routes/org/builder/testkit.tsx",
    callee: "query",
    argument: "what",
    why: "the builder test kit's stub socket answers whatever kind a screen asks it, through the fixture the suite supplies; every screen asking is held to a literal here",
  },
];

interface NamedCall {
  line: number;
  callee: string;
  /** The kind the call names, or null when the engine's gates cannot read one. */
  name: string | null;
  /** The call as written, up to its first argument, for the report. */
  written: string;
  /** The first argument as written. */
  argument: string;
}

interface Alias {
  line: number;
  imported: string;
  local: string;
}

/**
 * Every call in one module that names a question, and every import that
 * renames one. `path` is the module's, under `src/`, which a relative import
 * is resolved against; a snippet is read as a module at the root.
 */
function namedCalls(
  source: string,
  lang: Lang,
  path = "snippet.tsx",
): { calls: NamedCall[]; aliases: Alias[] } {
  const line = lineOf(source);
  const program = parse(source, lang);
  const bound = new Map<string, string>();
  /** Each namespace import, by its local name, to the module it is. */
  const namespaces = new Map<string, string>();
  const aliases: Alias[] = [];
  for (const statement of program.body as Node[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    const from = String((statement.source as Node).value);
    if (!isLocalSource(from)) continue;
    const target = importTarget(path, from);
    for (const spec of statement.specifiers as Node[]) {
      const local = String((spec.local as Node).name);
      if (spec.type === "ImportNamespaceSpecifier") namespaces.set(local, target);
      if (spec.type !== "ImportSpecifier" || spec.importKind === "type") continue;
      const imported = spec.imported as Node;
      const name = String(imported.type === "Identifier" ? imported.name : imported.value);
      if (!SPELLED.has(name) && !boundTo(name, target)) continue;
      bound.set(local, name);
      if (local !== name) aliases.push({ line: line(spec.start), imported: name, local });
    }
  }
  const calls: NamedCall[] = [];
  walk(program, (node) => {
    if (node.type !== "CallExpression") return;
    const callee = node.callee as Node;
    // What the call is to, and whether the engine's gates can read that NAME
    // at all: a computed key names the method in a string they do not read.
    let named: string | null = null;
    let readable = true;
    if (callee.type === "Identifier") {
      const name = String(callee.name);
      named = SPELLED.has(name) ? name : (bound.get(name) ?? null);
    } else {
      const member = memberName(callee);
      const object = callee.object as Node | undefined;
      if (member !== null && SPELLED.has(member)) {
        named = member;
        readable = !callee.computed;
      } else if (
        member !== null &&
        object?.type === "Identifier" &&
        boundTo(member, namespaces.get(String(object.name)))
      ) {
        named = member;
      }
    }
    if (named === null) return;
    const first = (node.arguments as Node[])[0];
    calls.push({
      line: line(node.start),
      callee: named,
      name: readable ? stringValue(first) : null,
      written: source.slice(callee.start, first ? first.start : node.end).replace(/\s+/g, " "),
      argument: first ? source.slice(first.start, first.end) : "",
    });
  });
  return { calls, aliases };
}

/** Whether a call is the named forward in `FORWARDS`. */
const forwarded = (path: string, call: NamedCall) =>
  FORWARDS.some((f) => f.path === path && f.callee === call.callee && f.argument === call.argument);

describe("every read names its question", () => {
  const tree = modules().map((m) => ({ path: m.path, ...namedCalls(m.text, m.lang, m.path) }));

  test("with a string literal, which is what the engine's gates can read", () => {
    const offenders = tree.flatMap(({ path, calls }) =>
      calls
        .filter((call) => call.name === null && !forwarded(path, call))
        .map((call) => `${path}:${call.line} — ${call.written}${call.argument}`),
    );
    expect(
      offenders,
      "write the kind as a string literal, to a call by its own name: internal/api/queries " +
        "reads it there, and a kind it cannot read is one it cannot hold against the registry",
    ).toEqual([]);
  });

  test("under its own name, never an alias", () => {
    const renamed = tree.flatMap(({ path, aliases }) =>
      aliases.map((a) => `${path}:${a.line} — ${a.imported} as ${a.local}`),
    );
    expect(renamed, "import it as itself: the engine's gates find these calls by name").toEqual([]);
  });

  test("and the rule reads the tree, so it cannot pass by reading nothing", () => {
    // Over a hundred `useQuery` calls today and a pager's `socket.query` in three
    // screens. A parser that stopped seeing calls, or a hook that was renamed,
    // collapses both counts, and the rule above would pass over nothing.
    const literal = (callee: string) =>
      tree.flatMap(({ calls }) => calls).filter((c) => c.callee === callee && c.name !== null)
        .length;
    expect(literal("useQuery")).toBeGreaterThan(80);
    expect(literal("query")).toBeGreaterThanOrEqual(2);
    expect(literal("useAct")).toBeGreaterThanOrEqual(4);
  });

  // THE WRITE VOCABULARY IS EXACTLY WHAT THE SCREENS PRESS. `ACTIONS` is
  // held against the engine's catalogue by a Go gate, and that gate can only
  // certify a row's arguments as ones the tool TAKES — never that a control
  // sends them. A row no control uses is a certified write nothing makes, and
  // a control naming a tool with no row does not compile; so the rows are
  // held to the useAct( literals outside the suites, both ways.
  test("and every write the vocabulary allows is one a screen makes", () => {
    const pressed = new Set(
      tree
        .filter(({ path }) => !/\.test\.tsx?$/.test(path))
        .flatMap(({ calls }) => calls)
        .filter((c) => c.callee === "useAct" && c.name !== null)
        .map((c) => c.name!),
    );
    expect(
      Object.keys(ACTIONS).sort(),
      "contract/actions.ts carries a row per tool a control presses, and no other: " +
        "add the row with the control that sends it",
    ).toEqual([...pressed].sort());
  });

  test("and every forward it names still forwards", () => {
    const stale = FORWARDS.filter(
      (f) =>
        !tree.some(
          ({ path, calls }) =>
            path === f.path && calls.some((call) => call.name === null && forwarded(path, call)),
        ),
    ).map((f) => `${f.path} — ${f.callee}(${f.argument}`);
    expect(stale, "this forward is gone: delete its FORWARDS entry").toEqual([]);
  });

  // THE RULE'S RED HALF: every way a kind stops being readable, each of which
  // must come back as a call with no name.
  const USE_QUERY = 'import { useQuery } from "~/lib/useQuery.ts";\n';
  test.each([
    ["a variable", `${USE_QUERY}const kind = "work_item";\nuseQuery(kind, { id });`],
    ["a template", `${USE_QUERY}useQuery(\`work_\${shape}\`, {});`],
    ["a concatenation", `${USE_QUERY}useQuery("work_" + shape, {});`],
    ["a conditional", `${USE_QUERY}useQuery(mine ? "work_my_work" : "work_items");`],
    ["no argument at all", `${USE_QUERY}useQuery();`],
    ["a relative import", 'import { useQuery } from "./useQuery.ts";\nuseQuery(kind);'],
    ["a namespace import", 'import * as q from "~/lib/useQuery.ts";\nq.useQuery(kind);'],
    ["a socket's method", "socket.query(kind, params);"],
    ["an optional call", "client?.socket.query(kind);"],
    // The shape a rule that knew only `.query(` passed and the Go reader
    // dropped: the socket's method, taken off it and called bare.
    ["a method taken off the socket", "const { query } = socket;\nvoid query(kind);"],
    ["a bare call of any binding", "function query(what: string) {}\nquery(what);"],
    [
      "a hook of the same name from a package",
      'import { useQuery } from "@tanstack/react-query";\nuseQuery(key);',
    ],
    // A literal kind the engine still cannot read, because the method is named
    // in a string rather than by a name.
    ["a computed key", 'socket["query"]("events", params);'],
    ["the write surface", 'import { act } from "~/protocol/act.ts";\nact(tool, { args });'],
    ["the write hook", 'import { useAct } from "~/lib/useAct.ts";\nuseAct(tool);'],
  ])("the rule catches %s", (_name, source) => {
    const { calls } = namedCalls(source, "tsx");
    expect(calls.length, `nothing read in ${source}`).toBe(1);
    expect(calls[0]?.name).toBeNull();
  });

  test.each([
    ["useQuery", 'import { useQuery as ask } from "~/lib/useQuery.ts";\nask("work_item");'],
    ["act", 'import { act as write } from "~/protocol/act.ts";\nwrite("set_pins", {});'],
    ["useAct", 'import { useAct as change } from "~/lib/useAct.ts";\nchange("set_pins");'],
  ])("the rule catches an alias of %s", (name, source) => {
    const { aliases, calls } = namedCalls(source, "tsx");
    expect(aliases.map((a) => a.imported)).toEqual([name]);
    // And the call behind it is still held to a literal, under the name it renames.
    expect(calls.map((c) => c.callee)).toEqual([name]);
  });

  // AND ITS GREEN HALF: the shapes the tree writes, which must read as named,
  // and the calls that share a name without being one of these.
  test.each([
    ["a literal", `${USE_QUERY}useQuery("work_item", { id });`, "work_item"],
    [
      "a literal prettier broke over lines",
      `${USE_QUERY}const a = useQuery(\n  "work_project",\n  key ? { key } : undefined,\n  { enabled: key !== "" },\n);`,
      "work_project",
    ],
    ["a template with nothing in it", `${USE_QUERY}useQuery(\`fleet\`);`, "fleet"],
    ["type arguments", `${USE_QUERY}useQuery<"fleet">("fleet");`, "fleet"],
    ["a pager's own call", 'const page = await socket.query("events", params);', "events"],
    ["a bare call", 'const { query } = socket;\nvoid query("events");', "events"],
    ["an optional call", 'client?.socket.query?.("events");', "events"],
    [
      "the write surface",
      'import { act } from "~/protocol/act.ts";\nact("set_pins", {});',
      "set_pins",
    ],
    [
      "the write surface through the protocol's barrel",
      'import { act } from "~/protocol/index.ts";\nact("set_pins", {});',
      "set_pins",
    ],
    [
      "the write hook",
      'import { useAct } from "~/lib/useAct.ts";\nconst pin = useAct("set_pins");',
      "set_pins",
    ],
  ])("the rule reads %s", (_name, source, name) => {
    expect(namedCalls(source, "tsx").calls.map((c) => c.name)).toEqual([name]);
  });

  test.each([
    ["React's own act", 'import { act } from "@testing-library/react";\nact(() => {});'],
    ["a local function", "function ask(what: string) {}\nask(kind);"],
    ["a type-only import", 'import type { act } from "~/protocol/act.ts";\nact(tool);'],
    ["a namespace's own act", 'import * as rtl from "@testing-library/react";\nrtl.act(() => {});'],
    // THE TEST DOOR RE-EXPORTS THE LIBRARY'S `act` under its own name, from a
    // module of this tree — which is why a bound name is held to the module
    // that DEFINES it rather than to any local import.
    ["the test door's act", 'import { act } from "~/test/inCase.ts";\nact(() => {});'],
    [
      "the test door's act, as a namespace",
      'import * as t from "~/test/inCase.ts";\nt.act(() => {});',
    ],
    ["a method of another name", "const hits = el.querySelector(selector);"],
  ])("the rule leaves %s alone", (_name, source) => {
    expect(namedCalls(source, "tsx").calls).toEqual([]);
  });

  // A RELATIVE IMPORT IS RESOLVED FROM THE MODULE THAT MAKES IT: the same
  // line is the write surface one directory down and nothing two down.
  test("the rule resolves a relative import from the module that makes it", () => {
    const source = 'import { act } from "../protocol/act.ts";\nact(tool, {});';
    expect(namedCalls(source, "tsx", "lib/useAct.ts").calls.map((c) => c.name)).toEqual([null]);
    expect(namedCalls(source, "tsx", "routes/work/Board.tsx").calls).toEqual([]);
  });
});

/**
 * A LINK TO A SEAT NAMES A TAB THE PROFILE HAS.
 *
 * `tab=` is a string off a URL, and a profile resolves one it does not have to
 * Overview rather than to a blank page (`useTab`) — which is exactly why a
 * stale one is invisible: the Model activity screen and the attention queue
 * went on sending readers to `tab=model` and `tab=cost` for a whole rewrite,
 * and every one of those links quietly opened the wrong tab.
 *
 * Both spellings a seat link is written in are read: `href`/`nav.to` over a
 * literal `["agents", "seats", …]` path with a literal query, and a row's
 * `{ path: ["agents", "seats", …], query: { tab } }`.
 */
function seatTabLinks(text: string, lang: Lang): { tab: string; at: number }[] {
  const out: { tab: string; at: number }[] = [];
  const toSeats = (node: Node | undefined): boolean => {
    if (node?.type !== "ArrayExpression") return false;
    const [a, b] = node.elements as Node[];
    return stringValue(a) === "agents" && stringValue(b) === "seats";
  };
  const tabOf = (node: Node | undefined): void => {
    if (node?.type !== "ObjectExpression") return;
    for (const prop of node.properties as Node[]) {
      if (prop.type !== "Property") continue;
      const key = prop.key as Node;
      const name = key.type === "Identifier" ? String(key.name) : stringValue(key);
      const value = stringValue(prop.value as Node);
      if (name === "tab" && value !== null) out.push({ tab: value, at: prop.start });
    }
  };
  walk(parse(text, lang), (n) => {
    if (n.type === "CallExpression") {
      const callee = n.callee as Node;
      const named =
        (callee.type === "Identifier" && callee.name === "href") || memberName(callee) === "to";
      const [first, second] = n.arguments as Node[];
      if (named && toSeats(first)) tabOf(second);
    }
    if (n.type === "ObjectExpression") {
      const props = (n.properties as Node[]).filter((p) => p.type === "Property");
      const field = (name: string) =>
        props.find((p) => {
          const key = p.key as Node;
          return (key.type === "Identifier" ? String(key.name) : stringValue(key)) === name;
        })?.value as Node | undefined;
      if (toSeats(field("path"))) tabOf(field("query"));
    }
  });
  return out;
}

test("every link to a seat names a tab its profile has", () => {
  const tabs = new Set<string>(AGENT_TABS);
  const stale: string[] = [];
  let links = 0;
  for (const mod of modules()) {
    if (mod.lang === "dts") continue;
    const line = lineOf(mod.text);
    for (const link of seatTabLinks(mod.text, mod.lang)) {
      links++;
      if (!tabs.has(link.tab)) stale.push(`${mod.path}:${line(link.at)} — tab=${link.tab}`);
    }
  }
  expect(stale, "these links open a tab no seat has, which lands on Overview").toEqual([]);
  // NOT VACUOUS: seats are linked to on a named tab somewhere.
  expect(links).toBeGreaterThan(0);
});

test("the seat-tab reading sees both link spellings", () => {
  const read = (text: string) => seatTabLinks(text, "tsx").map((l) => l.tab);
  expect(read('href(["agents", "seats", h], { tab: "memory" });')).toEqual(["memory"]);
  expect(read('nav.to(["agents", "seats", r.role], { tab: "model" });')).toEqual(["model"]);
  expect(read('const row = { path: ["agents", "seats", h], query: { tab: "cost" } };')).toEqual([
    "cost",
  ]);
  // Another object's tab is not a seat's.
  expect(read('href(["work", key], { tab: "turns" });')).toEqual([]);
});
