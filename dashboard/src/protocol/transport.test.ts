// @vitest-environment node

/**
 * EVERY WRITE GOES THROUGH THE ONE TRANSPORT
 * (`TestEveryWriteGoesThroughTheOneTransport`).
 *
 * The dashboard reaches the engine from `src/protocol/` and from nowhere else:
 * `rest.ts` for every REST route, `socket.ts` for the live socket and its
 * handshake probe, `api.ts` for the degraded-mode snapshot poll. What that buys
 * is everything those modules decide ONCE — the operator token on every
 * request, the deadline that ends a request that never settles, a body read to
 * its end before a write counts as answered, no browser cache in front of a
 * guarded answer, and a refusal a screen can branch on. A screen that reached
 * for `fetch` itself would get none of it, and nothing would say so: it type
 * checks, it works on the happy path, and it is the one write in the product
 * that sends no credential, waits for ever, or reads a dropped connection as a
 * success. That is a real history here: `rest.ts` exists because the Fleet
 * screen once took its client from a context field the shell never populated
 * and shipped dead.
 *
 * So this suite refuses, in every module outside `src/protocol/`, any READ of
 * the browser's network primitives:
 *
 *  - `fetch`, `XMLHttpRequest`, `WebSocket`, `EventSource` and `WebTransport`,
 *    bare or off a global object (`window`, `globalThis`, `self`), by a dotted
 *    or a constant computed key;
 *  - `navigator.sendBeacon`;
 *  - a form that posts to a URL on its own (`<form action="/…">`, a
 *    `formAction`), which is a write no script is even involved in.
 *
 * A READ, NOT ONLY A CALL. `const send = fetch` or `{ fetch }` handed to a
 * helper is how a call escapes a scan of calls. What is NOT a read of the
 * primitive: `refetch()` and `loader.fetch()`, which only share letters with
 * it; a type (`WebSocket | null`, `typeof fetch`); a key or a member that
 * happens to be called `fetch`; a string or a comment saying `fetch(`; a React
 * form action that is a function; and a stub a test kit ASSIGNS
 * (`globalThis.fetch = …`), which replaces the primitive rather than reaching
 * the network through it.
 *
 * Parsed through `test/source.ts` rather than matched as text — which is how
 * the first two of those stay green without a list of exemptions — and certified
 * both ways below, because a scanner that quietly stopped reading one form
 * reports a clean tree over exactly the module it was written to catch.
 */

import { describe, expect, test } from "vitest";

import { type Lang, type Node, lineOf, memberName, modules, parse, walk } from "../test/source.ts";

/** The browser's own ways of reaching a server, by the global each is. */
const PRIMITIVES = new Set(["fetch", "XMLHttpRequest", "WebSocket", "EventSource", "WebTransport"]);
/** The objects a global can be read off as a property. */
const GLOBALS = new Set(["window", "globalThis", "self"]);
/** A member whose read reaches the network whatever it is read off. */
const MEMBER_PRIMITIVES = new Set(["sendBeacon"]);
/** The attributes that make an element submit to a URL of its own. */
const SUBMITS_TO = new Set(["action", "formAction"]);

/**
 * TypeScript nodes that hold a runtime expression. Every OTHER `TS*` node is
 * type-only, and nothing under it runs: `typeof fetch` in a type is not a read
 * of `fetch`.
 */
const TS_EXPRESSIONS = new Set([
  "TSAsExpression",
  "TSSatisfiesExpression",
  "TSNonNullExpression",
  "TSInstantiationExpression",
  "TSTypeAssertion",
  "TSEnumDeclaration",
  "TSEnumBody",
  "TSEnumMember",
  "TSModuleDeclaration",
  "TSModuleBlock",
  "TSExportAssignment",
  "TSParameterProperty",
]);

/**
 * Whether an identifier held by `parent` under `key` is a VALUE being read,
 * rather than a name being declared, a label, or the name half of a property.
 */
function isRead(parent: Node | null, key: string): boolean {
  if (!parent) return true;
  switch (parent.type) {
    case "MemberExpression":
      return key !== "property" || parent.computed === true;
    case "Property":
    case "MethodDefinition":
    case "PropertyDefinition":
    case "AccessorProperty":
      return key !== "key" || parent.computed === true;
    case "VariableDeclarator":
      return key !== "id";
    case "FunctionDeclaration":
    case "FunctionExpression":
    case "ArrowFunctionExpression":
    case "ClassDeclaration":
    case "ClassExpression":
      return key !== "id" && key !== "params";
    case "AssignmentExpression":
      // A stub replacing the primitive, not a read of it.
      return key !== "left";
    case "LabeledStatement":
    case "BreakStatement":
    case "ContinueStatement":
    case "ImportSpecifier":
    case "ImportDefaultSpecifier":
    case "ImportNamespaceSpecifier":
      return false;
    default:
      return true;
  }
}

/**
 * Whether an expression is TEXT — a string, a template with or without
 * substitutions, or a `+` chain with one in it — which is what a URL written
 * into an attribute is, and what a function never is.
 */
function isText(node: Node): boolean {
  if (node.type === "Literal") return typeof node.value === "string";
  if (node.type === "TemplateLiteral") return true;
  if (node.type === "BinaryExpression" && node.operator === "+") {
    return isText(node.left as Node) || isText(node.right as Node);
  }
  return false;
}

/** One place a module reaches the network itself. */
interface NetworkRead {
  line: number;
  /** The primitive, as it was reached: `fetch`, `window.fetch`, `<form action>`. */
  what: string;
}

/**
 * Every place the module's code reaches the network without the protocol
 * layer. The same function reads the tree and the certification cases, so what
 * the cases prove about it is true of the scan that guards the tree.
 */
function networkReads(source: string, lang: Lang): NetworkRead[] {
  const line = lineOf(source);
  const found: NetworkRead[] = [];
  walk(parse(source, lang), (node, parent, key) => {
    if (node.type.startsWith("TS") && !TS_EXPRESSIONS.has(node.type)) return false;

    if (node.type === "Identifier" && PRIMITIVES.has(String(node.name)) && isRead(parent, key)) {
      found.push({ line: line(node.start), what: String(node.name) });
    }

    if (node.type === "MemberExpression") {
      const name = memberName(node);
      const object = node.object as Node;
      const assigned = parent?.type === "AssignmentExpression" && key === "left";
      if (name !== null && !assigned) {
        if (
          PRIMITIVES.has(name) &&
          object.type === "Identifier" &&
          GLOBALS.has(String(object.name))
        ) {
          found.push({ line: line(node.start), what: `${String(object.name)}.${name}` });
        }
        if (MEMBER_PRIMITIVES.has(name)) {
          found.push({ line: line(node.start), what: name });
        }
      }
    }

    // A URL in an attribute — a string, or a braced one — not a React form
    // action, which is a function and never leaves the page.
    if (node.type === "JSXAttribute") {
      const name = node.name as Node;
      const value = node.value as Node | null;
      const url =
        value !== null &&
        (isText(value) ||
          (value.type === "JSXExpressionContainer" && isText(value.expression as Node)));
      if (name.type === "JSXIdentifier" && SUBMITS_TO.has(String(name.name)) && url) {
        const element = parent?.name as Node | undefined;
        const tag = element?.type === "JSXIdentifier" ? String(element.name) : "";
        // A lower-case tag is an HTML element; `<EmptyState action={…}>` is a
        // prop of a component and submits nothing.
        if (/^[a-z]/.test(tag)) {
          found.push({ line: line(node.start), what: `<${tag} ${String(name.name)}>` });
        }
      }
    }
    return true;
  });
  return found;
}

describe("every write goes through the one transport", () => {
  test("no module outside src/protocol/ reaches the network itself", () => {
    const all = modules();
    // A FLOOR, because a walk that found nothing passes the assertion below —
    // and a moved `src/` is exactly what would make it find nothing.
    expect(all.length).toBeGreaterThan(150);
    const offenders = all
      .filter(({ path }) => !path.startsWith("protocol/"))
      .flatMap(({ path, text, lang }) =>
        networkReads(text, lang).map((read) => `${path}:${read.line} — ${read.what}`),
      );
    expect(
      offenders,
      "the dashboard reaches the engine through src/protocol/ only: call `rest` for a REST " +
        "route and `useQuery` for a question, so the token, the deadline and the refusal are " +
        "the ones every other request gets",
    ).toEqual([]);
  });

  test("the scan reads the transport's own calls, so it cannot pass by reading nothing", () => {
    // THE OTHER SIDE, over the real tree: the protocol layer is where these
    // reads legitimately are, so a scanner that still reads the tree finds
    // them there. One that stopped (a parser upgrade that renamed a node, a
    // walk that skipped a field) finds nothing anywhere and reports the
    // screens clean.
    const inside = modules()
      .filter(({ path }) => path.startsWith("protocol/"))
      .flatMap(({ path, text, lang }) => networkReads(text, lang).map((r) => `${path} ${r.what}`));
    expect(inside).toContain("protocol/rest.ts fetch");
    expect(inside).toContain("protocol/socket.ts WebSocket");
  });

  // THE SCANNER'S RED HALF: every way a module could reach the network,
  // each of which must come back as exactly the primitive it names.
  test.each([
    ["a bare call", 'fetch("/x");', "fetch"],
    ["an awaited write", 'const r = await fetch("/x", { method: "POST" });', "fetch"],
    ["a call in a callback", 'useEffect(() => { void fetch("/x"); }, []);', "fetch"],
    ["a call off window", 'window.fetch("/x");', "window.fetch"],
    ["a call off globalThis", 'globalThis.fetch("/x");', "globalThis.fetch"],
    ["a call off self", 'self.fetch("/x");', "self.fetch"],
    ["a computed key", 'window["fetch"]("/x");', "window.fetch"],
    ["an optional call", 'globalThis.fetch?.("/x");', "globalThis.fetch"],
    ["an alias", 'const send = fetch;\nsend("/x");', "fetch"],
    ["a bound copy", "const send = fetch.bind(window);", "fetch"],
    ["a sequence", '(0, fetch)("/x");', "fetch"],
    ["a shorthand property", "const deps = { fetch };", "fetch"],
    ["an argument", "withRetry(fetch);", "fetch"],
    ["an XMLHttpRequest", "const r = new XMLHttpRequest();", "XMLHttpRequest"],
    ["a socket", "const s = new WebSocket(url);", "WebSocket"],
    ["a socket off window", "const s = new window.WebSocket(url);", "window.WebSocket"],
    ["an event stream", 'const e = new EventSource("/events");', "EventSource"],
    ["a WebTransport", "const t = new WebTransport(url);", "WebTransport"],
    ["a beacon", 'navigator.sendBeacon("/x", body);', "sendBeacon"],
    ["a form posting to a URL", 'const e = <form action="/x" method="post" />;', "<form action>"],
    ["a braced URL", "const e = <form action={`/x/${id}`} />;", "<form action>"],
    ["a button's own target", 'const e = <button formAction="/x" />;', "<button formAction>"],
  ])("the scan catches %s", (_name, source, what) => {
    expect(networkReads(source, "tsx").map((r) => r.what)).toContain(what);
  });

  // AND ITS GREEN HALF: what shares a name with a primitive without being
  // one. A gate that cried wolf over `refetch()` is one somebody switches off,
  // and every `useQuery` in the tree hands back a `refetch`.
  test.each([
    ["refetch", 'const { refetch } = useQuery("x");\nrefetch();'],
    ["a method called refetch", "state.refetch();"],
    ["a method called fetch", "loader.fetch();"],
    ["a comment", '// fetch("/x") would skip the token\nconst x = 1;'],
    ["a string", 'const s = "fetch(";'],
    ["a template", "const s = `fetch(${x})`;"],
    ["a type", "let s: WebSocket | null = null;\ntype F = typeof fetch;"],
    ["a cast", "const s = raw as unknown as WebSocket;"],
    ["an interface member", "interface L { fetch(): void; WebSocket: number }"],
    ["a class member", "class A { fetch() {} }"],
    ["an object key", "const o = { fetch: load };"],
    ["a stub a test kit assigns", "globalThis.fetch = stub;"],
    ["a stub by name", 'Object.defineProperty(globalThis, "WebSocket", { value: Inert });'],
    ["a class that is not the primitive", "class InertWebSocket {}\nnew InertWebSocket();"],
    ["a form handled in React", "const e = <form onSubmit={submit} />;"],
    ["a React form action", "const e = <form action={submit} />;"],
    ["a component's action prop", 'const e = <EmptyState action="Retry" />;'],
  ])("the scan leaves %s alone", (_name, source) => {
    expect(networkReads(source, "tsx")).toEqual([]);
  });
});
