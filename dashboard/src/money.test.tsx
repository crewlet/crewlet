/**
 * NOTHING ON A SCREEN IS MONEY — rule 19, held from the source and the page.
 *
 * The dashboard measures spend in TOKENS and never in a currency
 * (`docs/reference/dashboard-design.md`, rule 19). The engine records a price
 * where one is reported — a subscription coding CLI quotes what a run cost and
 * nothing else does — and that figure is on the wire. On a screen it is a lie
 * of omission: a currency covering the minority of calls that quote one, beside
 * a token count covering all of them, reads as the company's spend and is a
 * fraction of it. So the client declares no price field, parses none and draws
 * none, and three gates hold that, each over a different artefact:
 *
 *  1. **The source** (below): every shipped module, parsed rather than grepped,
 *     so a comment explaining the rule is free to name what it forbids and a
 *     string that renders is not free to.
 *  2. **The screens** (below): the screens that draw spend, rendered over wire
 *     fixtures that carry a price everywhere the engine could put one, and
 *     read back for any trace of it — the half that sees a price arriving by a
 *     route no pattern anticipated.
 *  3. **The bundle** — `TestTheDashboardRendersNoPrice` in
 *     `internal/api/dashboardjs_test.go`, over every module the engine SERVES,
 *     which is what a browser actually runs.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { ReactElement } from "react";
import { parseAst } from "vite";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { Rollup } from "~/protocol/index.ts";
import { TurnScreen } from "~/routes/activity/Turn.tsx";
import { SeatScreen } from "~/routes/company/Seat.tsx";
import { Spend } from "~/routes/cost/Spend.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { WorkItem } from "~/routes/work/WorkItem.tsx";

// ---------------------------------------------------------------------------
// 1. The source
// ---------------------------------------------------------------------------

/** One way a module could put a price on a screen. */
interface Finding {
  line: number;
  what: string;
}

/**
 * The engine's price fields, however a client would spell them: `cost_usd`,
 * `total_cost_usd` and `estimated_cost_usd` off the wire, `priced_calls`, and
 * the camel-cased copy a parser makes (`costUSD`, `pricedCalls`).
 */
const PRICE_FIELD = /cost_?usd|priced_?calls/i;
/** A currency named by its code, as a word — never inside another. */
const CURRENCY_CODE = /\b(?:USD|EUR|GBP)\b/;
/**
 * The euro and the pound mean nothing but money, and nothing in this product
 * spells either, so they are refused wherever text is. The dollar sign is not:
 * it opens every `${VAR}` reference a company config carries, so it is refused
 * only where it stands in front of a value.
 */
const CURRENCY_SIGN = /[€£]/;
/**
 * Intl's currency formatting — the `style: "currency"` value and the three
 * option keys that only exist to go with it.
 */
const INTL_CURRENCY = new Set(["currency", "currencyDisplay", "currencySign"]);
/** Text that ends in a dollar sign, which is what a price is written behind. */
const ENDS_IN_DOLLAR = /\$\s*$/;

interface Node {
  type: string;
  start: number;
  end: number;
  [key: string]: unknown;
}

function isNode(value: unknown): value is Node {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { type?: unknown }).type === "string"
  );
}

/** A node's direct children, in source order. */
function childrenOf(node: Node): Node[] {
  const out: Node[] = [];
  for (const [key, value] of Object.entries(node)) {
    if (key === "type" || key === "start" || key === "end") continue;
    if (Array.isArray(value)) {
      for (const item of value) if (isNode(item)) out.push(item);
    } else if (isNode(value)) {
      out.push(value);
    }
  }
  return out.sort((a, b) => a.start - b.start);
}

/**
 * The text a node is when it is CONSTANT — a string, a template with nothing
 * substituted, a JSX text run — or null when it is a value.
 *
 * A string literal inside a substitution is constant too, which is how the
 * braced reference is written in JSX: `${"{VAR}"}` is a `$` beside the
 * constant `{VAR}`, and it names a variable rather than pricing one.
 */
function constantText(node: Node): string | null {
  switch (node.type) {
    case "Literal":
      return typeof node.value === "string" ? node.value : null;
    case "JSXText":
      return String(node.value);
    case "TemplateLiteral": {
      const quasis = node.quasis as Node[];
      const expressions = node.expressions as Node[];
      if (expressions.length > 0) return null;
      const value = quasis[0]?.value as { cooked?: string | null; raw: string } | undefined;
      return value?.cooked ?? value?.raw ?? "";
    }
    case "JSXExpressionContainer":
    case "ParenthesizedExpression":
      return isNode(node.expression) ? constantText(node.expression) : null;
    default:
      return null;
  }
}

/** A JSX `{/* comment *\/}`: neither text nor a value, so it breaks nothing. */
function transparent(node: Node): boolean {
  return (
    node.type === "JSXEmptyExpression" ||
    (node.type === "JSXExpressionContainer" &&
      isNode(node.expression) &&
      node.expression.type === "JSXEmptyExpression")
  );
}

/**
 * The first VALUE in a run of parts that stands right behind a dollar sign.
 *
 * The parts are what one piece of text is assembled from, in order — a
 * template's quasis and substitutions, an element's children, the operands of
 * a `+` chain. Constant parts join into one run of text; a value ends it, and
 * a run that ends in `$` puts that value on screen as a price.
 */
function valueBehindDollar(parts: (string | Node)[]): Node | null {
  let run = "";
  for (const part of parts) {
    if (typeof part === "string") {
      run += part;
      continue;
    }
    if (transparent(part)) continue;
    const text = constantText(part);
    if (text !== null) {
      run += text;
      continue;
    }
    if (ENDS_IN_DOLLAR.test(run)) return part;
    run = "";
  }
  return null;
}

/** The operands of a `+` chain, flattened left to right. */
function operands(node: Node): Node[] {
  if (node.type === "BinaryExpression" && node.operator === "+") {
    return [...operands(node.left as Node), ...operands(node.right as Node)];
  }
  if (node.type === "ParenthesizedExpression" && isNode(node.expression)) {
    return operands(node.expression);
  }
  return [node];
}

/** Every text a node holds as constant source text, for the word rules. */
function textsOf(node: Node): string[] {
  switch (node.type) {
    case "Literal":
      return typeof node.value === "string" ? [node.value] : [];
    case "JSXText":
      return [String(node.value)];
    case "TemplateElement": {
      const value = node.value as { cooked?: string | null; raw: string };
      return [value.cooked ?? value.raw];
    }
    default:
      return [];
  }
}

/**
 * Every place a module's CODE could put a price on a screen.
 *
 * Parsed, never grepped: the comments are not in the tree, so a note saying
 * why there is no price may name the field it is about, while a string, a
 * JSX text run, an identifier and a property are all read. The same function
 * reads the tree and the certification cases below, so what those cases prove
 * about it is true of the scan that guards the tree.
 */
function pricesIn(source: string, lang: "ts" | "tsx" | "dts"): Finding[] {
  const lineStarts = [0];
  for (let i = 0; i < source.length; i++) if (source[i] === "\n") lineStarts.push(i + 1);
  const lineOf = (offset: number): number => {
    let lo = 0;
    let hi = lineStarts.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if ((lineStarts[mid] ?? 0) <= offset) lo = mid;
      else hi = mid - 1;
    }
    return lo + 1;
  };
  const found: Finding[] = [];
  const report = (node: Node, what: string) => found.push({ line: lineOf(node.start), what });

  const visit = (node: Node, insidePlus: boolean): void => {
    // NAMES: an identifier, a property, a JSX attribute or a type member.
    if (
      (node.type === "Identifier" ||
        node.type === "JSXIdentifier" ||
        node.type === "PrivateIdentifier") &&
      typeof node.name === "string"
    ) {
      if (PRICE_FIELD.test(node.name)) report(node, `names the price field \`${node.name}\``);
      if (INTL_CURRENCY.has(node.name)) report(node, `names Intl's \`${node.name}\` option`);
    }

    // TEXT: what a string, a template or a JSX run would put on screen.
    for (const text of textsOf(node)) {
      if (PRICE_FIELD.test(text)) report(node, `spells the price field in ${JSON.stringify(text)}`);
      if (CURRENCY_CODE.test(text)) report(node, `names a currency in ${JSON.stringify(text)}`);
      if (CURRENCY_SIGN.test(text))
        report(node, `carries a currency sign: ${JSON.stringify(text)}`);
      if (node.type === "Literal" && INTL_CURRENCY.has(text.trim())) {
        report(node, `asks Intl for ${JSON.stringify(text)}`);
      }
    }

    // A DOLLAR IN FRONT OF A VALUE, in each way text is assembled.
    if (node.type === "TemplateLiteral") {
      const quasis = node.quasis as Node[];
      const expressions = node.expressions as Node[];
      const parts: (string | Node)[] = [];
      quasis.forEach((quasi, i) => {
        parts.push(textsOf(quasi)[0] ?? "");
        const expression = expressions[i];
        if (expression) parts.push(expression);
      });
      const value = valueBehindDollar(parts);
      if (value) report(value, "puts a value behind a dollar sign in a template");
    }
    if (node.type === "JSXElement" || node.type === "JSXFragment") {
      const value = valueBehindDollar(node.children as Node[]);
      if (value) report(value, "puts a value behind a dollar sign in JSX");
    }
    const plus = node.type === "BinaryExpression" && node.operator === "+";
    if (plus && !insidePlus) {
      const value = valueBehindDollar(operands(node));
      if (value) report(value, "puts a value behind a dollar sign by concatenation");
    }

    for (const child of childrenOf(node)) visit(child, plus);
  };

  visit(parseAst(source, { lang }) as unknown as Node, false);
  return found;
}

// `process.cwd()`, not `import.meta.url`: under the jsdom environment the
// screens below need, a module's URL is the test server's rather than a file,
// and Vitest runs from `dashboard/`.
const SRC = join(process.cwd(), "src");

/** Every module that ships, with the language it is parsed as. */
function shippedModules(): { path: string; text: string; lang: "ts" | "tsx" | "dts" }[] {
  const out: { path: string; text: string; lang: "ts" | "tsx" | "dts" }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry) || entry.includes(".test.")) continue;
      const lang = entry.endsWith(".d.ts") ? "dts" : entry.endsWith(".tsx") ? "tsx" : "ts";
      out.push({
        path: relative(SRC, full).split("\\").join("/"),
        text: readFileSync(full, "utf8"),
        lang,
      });
    }
  })(SRC);
  return out;
}

describe("the source", () => {
  test("no shipped module names, formats or spells a price", () => {
    const modules = shippedModules();
    // A FLOOR, because a walk that found nothing passes every assertion
    // below it — and a moved `src/` is exactly what would make it find
    // nothing. The tree holds about two hundred modules.
    expect(modules.length).toBeGreaterThan(150);
    const offenders = modules.flatMap(({ path, text, lang }) =>
      pricesIn(text, lang).map((f) => `${path}:${f.line} — ${f.what}`),
    );
    expect(
      offenders,
      "the dashboard renders tokens and never money (rule 19 in docs/reference/dashboard-design.md): " +
        "draw the token count instead, and declare no price field for a screen to reach for",
    ).toEqual([]);
  });

  // THE SCANNER'S OWN RED HALF: every form it exists to catch, one per case,
  // each of which must come back as exactly the finding it names. A scanner
  // that stopped reading one of these reports a clean tree over a screen that
  // prices something, which is the failure this whole file is against.
  test.each([
    ["a price field on a type", "interface B { cost_usd: number }", "names the price field"],
    ["a CLI's own total", "const t = p.total_cost_usd;", "names the price field"],
    ["a parsed copy", "const r = { costUSD: 0 };", "names the price field"],
    ["the priced-call count", "const n = totals.priced_calls;", "names the price field"],
    ["a field read by a string key", 'const c = p["cost_usd"];', "spells the price field"],
    ["Intl's currency style", 'const f = { style: "currency" };', "asks Intl for"],
    ["Intl's currency option", "const f = { currency: code };", "names Intl's `currency` option"],
    ["a currency code", 'const label = "USD";', "names a currency"],
    ["a code in JSX", "const e = <span>{n} EUR</span>;", "names a currency"],
    ["the euro sign", "const e = <span>{n} €</span>;", "carries a currency sign"],
    ["the pound sign", "const s = `£${n}`;", "carries a currency sign"],
    ["a dollar in a template", "const s = `$${n.toFixed(2)}`;", "in a template"],
    ["a spaced dollar in a template", "const s = `US$ ${n}`;", "in a template"],
    ["a dollar in JSX", "const e = <b>${n}</b>;", "in JSX"],
    ["a dollar before a component", "const e = <b>$<Num v={n} /></b>;", "in JSX"],
    ["a dollar concatenated", 'const s = "$" + n;', "by concatenation"],
    ["a dollar deep in a chain", 'const s = a + " at $" + n;', "by concatenation"],
    ["a dollar split across literals", 'const s = `${"$"}${n}`;', "in a template"],
  ])("the scan catches %s", (_name, source, what) => {
    const found = pricesIn(source, "tsx");
    expect(found.length, `nothing found in ${source}`).toBeGreaterThan(0);
    expect(found.map((f) => f.what).join("\n")).toContain(what);
  });

  // AND ITS GREEN HALF: the dollar sign this product DOES spell, each as it
  // is written in the tree today, and a comment that names what it forbids.
  // A scanner that cried wolf here is one somebody switches off.
  test.each([
    ["a braced reference in JSX", 'const e = <code>${"{VAR}"}</code>;'],
    ["a reference's opening sign", 'const ok = typed.trimStart().startsWith("$");'],
    ["a comparison with the sign", 'if (ch === "$") open();'],
    ["an escaped reference in a template", "const r = `\\${${name}}`;"],
    [
      "a comment naming the field",
      "// the engine sends cost_usd; nothing here reads it\nconst x = 1;",
    ],
    ["a doc comment naming the style", '/** never `style: "currency"` */\nconst x = 1;'],
    ["a sum that is not text", "const n = a + b;"],
    ["prose about currencies", 'const s = "a currency shown beside tokens misleads";'],
  ])("the scan leaves %s alone", (_name, source) => {
    expect(pricesIn(source, "tsx")).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// 2. The screens
// ---------------------------------------------------------------------------

/**
 * The price every fixture carries, and the token count beside it.
 *
 * 12.34 because it is formatted into nothing else a screen draws — no count,
 * duration or percentage here produces it — so finding it at all is finding
 * the price. The token count is under 10,000 so every screen writes it in
 * full (`fmtCount` abbreviates above that), which lets each case prove its
 * fixture was READ before it proves the price was not drawn.
 */
const PRICE = 12.34;
const TOKENS = 4_321;
const DRAWN = "4,321";

/**
 * Every name a price travels under, all on one object: the engine's own
 * `cost_usd` and `priced_calls`, and the coding CLIs' `total_cost_usd` and
 * `estimated_cost_usd` that it is read from — so a screen that learned to
 * reach for any of them, wherever a fixture carries it, is caught.
 */
const PRICED = {
  cost_usd: PRICE,
  total_cost_usd: PRICE,
  estimated_cost_usd: PRICE,
  priced_calls: 3,
};

/** A spend bucket as the engine sends it: tokens, and a price beside them. */
function bucket(total = TOKENS) {
  return {
    input_tokens: total - 321,
    output_tokens: 321,
    total_tokens: total,
    calls: 9,
    ...PRICED,
  };
}

/** A spend rollup with a price on every bucket in it, nested ones included. */
function rollup(): Rollup {
  const phase = { ...bucket(), phase: "execute" };
  return {
    since: "2026-09-12T10:00:00Z",
    until: "2026-09-13T10:00:00Z",
    agent_role: "",
    totals: bucket(),
    by_phase: [phase],
    by_model: [{ ...bucket(), model: "claude-sonnet-5" }],
    by_worker: [{ ...bucket(), worker: "researcher" }],
    by_agent: [
      { ...bucket(), role: "CEO", handle: "ceo", agent_id: "a-1", by_phase: { execute: bucket() } },
    ],
    by_turn: [
      {
        ...bucket(),
        turn_id: "t-1",
        work_key: "w-1",
        role: "CEO",
        handle: "ceo",
        agent_id: "a-1",
        started_at: "2026-09-13T09:00:00Z",
        ended_at: "2026-09-13T09:05:00Z",
        by_phase: { execute: bucket() },
      },
    ],
    aggregated_through: "2026-09-13T10:00:00Z",
    // A wire answer, which is what the engine sends; the client's own type
    // declares none of the price, and that is the point.
  } as unknown as Rollup;
}

/** The time axis over the same spend, priced the same way. */
function series() {
  return {
    group: "phase",
    bucket: "hour",
    since: "2026-09-12T10:00:00Z",
    until: "2026-09-13T10:00:00Z",
    series: [
      {
        ...bucket(),
        at: "2026-09-13T09:00:00Z",
        groups: { execute: bucket() },
        other: bucket(0),
      },
    ],
    by_group: [{ ...bucket(), group: "execute", other: false, folded: 0 }],
    totals: bucket(),
    grouped: bucket(),
  };
}

/** A finished phase as the engine publishes it, sandbox price and all. */
function pricedPhase(at: string) {
  return {
    id: `phase-${at}`,
    type: "agent_phase_completed",
    source: "engine",
    actor: "CEO",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    timestamp: at,
    payload: {
      turn_id: "t-1",
      phase: "execute",
      iteration: 1,
      role: "CEO",
      model: "claude-sonnet-5",
      backend: "e2b",
      coding_agent: "claude-code",
      sandbox_id: "sbx-1",
      input_tokens: TOKENS - 321,
      output_tokens: 321,
      total_tokens: TOKENS,
      rounds_used: 2,
      duration_ms: 90_000,
      ...PRICED,
    },
  };
}

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "";
  vi.unstubAllGlobals();
});

/**
 * Mount one screen at one address, over a store holding the priced rollup and
 * a socket answering every question from `answers`.
 *
 * The real store and the real query path, rather than a mocked hook: a price
 * that reached a screen would travel exactly this way, and a harness that
 * handed the component its props would skip the parser that decides what a
 * record carries.
 */
function mount(hash: string, view: ReactElement, answers: Record<string, unknown> = {}) {
  location.hash = hash;
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  store.applyTokens(rollup());
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(answers[what] ?? {});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{view}</Router>
    </ClientContext.Provider>,
  );
}

/**
 * Every trace of a price in the document: its text AND its attributes, since
 * a `title` or an `aria-label` is read as surely as a cell.
 */
function pricesOnScreen(): string[] {
  const html = document.body.innerHTML;
  const found: string[] = [];
  for (const [what, pattern] of [
    ["the fixture's price", /12[.,]34/],
    ["a sign before a figure", /[$€£]\s?\d/],
    ["a figure before a sign", /\d\s?[€£]/],
    ["a currency code", /\b(?:USD|EUR|GBP)\b/],
  ] as const) {
    const match = pattern.exec(html);
    if (match) {
      found.push(`${what}: …${html.slice(Math.max(0, match.index - 60), match.index + 40)}…`);
    }
  }
  return found;
}

describe("the screens", () => {
  // Each case waits for the fixture's TOKEN count first: a screen that never
  // read the answer draws no price either, and that pass would prove nothing.
  test.each([
    ["the landing screen", "#/inbox", <Inbox />, {}],
    ["spend", "#/cost", <Spend />, { token_series: series() }],
    [
      "the task page",
      "#/work/ENG-42",
      <WorkItem id="ENG-42" />,
      {
        work_item: {
          task: {
            id: "t-1",
            key: "ENG-42",
            project: "ENG",
            title: "Fix the login race",
            status: "in_progress",
            version: 1,
            spend: { turns: 2, tokens: TOKENS, wall_ms: 90_000, ...PRICED },
            ...PRICED,
          },
          complete: true,
        },
        work_project: { key: "ENG", name: "Engineering", complete: true },
      },
    ],
    [
      "the profile",
      "#/company/people/ceo?tab=cost",
      <SeatScreen handle="ceo" />,
      { tokens: rollup() },
    ],
    [
      "the trace",
      "#/activity/turns/t-1",
      <TurnScreen turnId="t-1" />,
      {
        turn: {
          turn_id: "t-1",
          truncated: false,
          events: [
            pricedPhase("2026-09-13T10:01:30Z"),
            // THE TURN'S OWN RECORD CARRIES NO PRICE, because the engine
            // puts none there: this screen shows it verbatim, and a verbatim
            // record is the one place rule 19 does not reach (see its text).
            {
              ...pricedPhase("2026-09-13T10:02:00Z"),
              id: "done",
              type: "turn_completed",
              payload: { turn_id: "t-1", outcome: "delivered" },
            },
          ],
        },
      },
    ],
  ] as const)("%s draws the tokens and no price", async (_name, hash, view, answers) => {
    mount(hash, view, answers);
    await waitFor(() => expect(screen.getAllByText(new RegExp(DRAWN)).length).toBeGreaterThan(0));
    expect(pricesOnScreen()).toEqual([]);
  });
});
