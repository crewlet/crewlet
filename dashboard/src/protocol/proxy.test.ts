// @vitest-environment node

/**
 * The dev server forwards exactly the engine paths the dashboard reaches —
 * every one it calls or names in markup, and nothing else.
 *
 * A PATH IT DOES NOT FORWARD FAILS SILENTLY AND LOOKS LIKE THE ENGINE'S OWN
 * ANSWER. A prefix Vite has no proxy entry for is served by Vite itself, and
 * Vite answers 404 for a path it has no file for, so the screen sees a refusal
 * it cannot tell from the engine refusing: the Integrations page rendered "not
 * configured" for a company that had configured things, and the degraded-mode
 * snapshot poll (the one read that exists for when everything else is already
 * down) returned null forever. A SOCKET it forwards without `ws: true` never
 * upgrades, which in `npm run dev` is a dashboard that says it is offline.
 *
 * AN ENTRY NOTHING REACHES IS THE OTHER HALF, and it is the half nobody
 * notices: it forwards a path no screen asks for, reads to the next person as a
 * dependency somebody relied on, and outlives the screen that needed it. Seven
 * of the thirteen entries were that when this became two-way — `/api`,
 * `/health`, `/org`, `/agents`, `/events`, `/tools` and `/schedules`, each
 * left behind when its reads moved onto the socket.
 *
 * What the dashboard reaches is read from the SOURCE, parsed rather than
 * matched as text, everywhere a URL is handed to the browser:
 *
 *  - the REST transport: `rest.get/post/put/patch/del/putText(path, …)` and
 *    `rest.request(method, path, …)`, where `rest` is the one imported from
 *    this tree under whatever local name — so a module built on it, a screen
 *    or another piece of the protocol layer, is read at its own call;
 *  - the primitives themselves: `fetch(url)`, `new WebSocket(url)`,
 *    `new EventSource(url)` and `navigator.sendBeacon(url)`, which only
 *    `src/protocol/` may read (`transport.test.ts`);
 *  - MARKUP: a JSX `src`, `href` or `poster` holding a path, and the same
 *    attributes in the shell (`index.html`). The browser fetches those itself,
 *    with no call anywhere to read — which is how the brand mark in the rail
 *    and the tab icon in the shell both answered 404 under `npm run dev` while
 *    every call in the tree was forwarded. The shell's own module entry
 *    (`/src/main.tsx`) names a file under the dev server's root, which Vite
 *    serves itself, and is not the engine's.
 *
 * A URL is read up to its first substitution: a string, a template's leading
 * text, a `+` chain's leading string, a module constant, and any of those after
 * `location.origin` or `location.host`. A call whose URL has no readable path
 * is a finding, because a path this scanner cannot read is a path it cannot
 * check — unless the call is the transport forwarding what its own caller
 * named, which is listed in `FORWARDS` with its reason and must still be there.
 * An ATTRIBUTE that holds no path is not: nearly every `href` in the tree is a
 * `#/` route or another site, which the dev server never sees.
 */

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
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

/** The `rest` methods that take the path first. */
const REST_PATH_FIRST = new Set(["get", "post", "put", "patch", "del", "putText"]);
/** Constructors whose first argument is the URL they dial. */
const DIALS = new Set(["WebSocket", "EventSource"]);
/** The objects `fetch` can be called off as a property. */
const GLOBALS = new Set(["window", "globalThis", "self"]);
/** Markup attributes the browser fetches the value of by itself. */
const FETCHED = new Set(["src", "href", "poster"]);

/**
 * The calls that hand on a URL their caller named, and why each is not a
 * path of its own.
 */
const FORWARDS: readonly { path: string; call: string; why: string }[] = [
  {
    path: "protocol/rest.ts",
    call: "fetch",
    why: "the REST transport's one request: every path it sends is a caller's, read at that caller",
  },
];

/** One URL the dashboard reaches, or a call whose URL cannot be read. */
interface Reach {
  line: number;
  /** What made the call: `rest.get`, `rest.request`, `fetch`, `new WebSocket`, `<img src>`. */
  call: string;
  /** The literal path, up to its first substitution; null when none can be read. */
  path: string | null;
  /** Whether it is a socket, which the proxy has to upgrade. */
  socket: boolean;
}

/** Whether a node is the page's own origin or host. */
function isOrigin(node: Node): boolean {
  const name = memberName(node);
  if (name !== "origin" && name !== "host") return false;
  const object = node.object as Node;
  return (
    (object.type === "Identifier" && object.name === "location") ||
    memberName(object) === "location"
  );
}

/** Module-level `const NAME = "…"`, by name: the constants a URL can be built from. */
function constantsOf(program: Node): Map<string, string> {
  const out = new Map<string, string>();
  for (let statement of program.body as Node[]) {
    if (statement.type === "ExportNamedDeclaration" && statement.declaration) {
      statement = statement.declaration as Node;
    }
    if (statement.type !== "VariableDeclaration" || statement.kind !== "const") continue;
    for (const declarator of statement.declarations as Node[]) {
      const id = declarator.id as Node;
      const value = stringValue(declarator.init as Node | undefined);
      if (id.type === "Identifier" && value !== null) out.set(String(id.name), value);
    }
  }
  return out;
}

/** A URL expression as the run of constant text and values it is assembled from. */
function partsOf(node: Node, constants: Map<string, string>): (string | Node)[] {
  if (node.type === "BinaryExpression" && node.operator === "+") {
    return [...partsOf(node.left as Node, constants), ...partsOf(node.right as Node, constants)];
  }
  if (node.type === "TemplateLiteral") {
    const quasis = node.quasis as Node[];
    const expressions = node.expressions as Node[];
    const out: (string | Node)[] = [];
    quasis.forEach((quasi, i) => {
      const value = quasi.value as { cooked?: string | null; raw: string };
      out.push(value.cooked ?? value.raw);
      const expression = expressions[i];
      if (expression) out.push(...partsOf(expression, constants));
    });
    return out;
  }
  if (node.type === "Identifier" && constants.has(String(node.name))) {
    return [constants.get(String(node.name))!];
  }
  const value = stringValue(node);
  return [value ?? node];
}

/**
 * The literal path a URL names, up to its first substitution or query string;
 * null when it names none this scanner can read.
 */
function pathOf(node: Node | undefined, constants: Map<string, string>): string | null {
  if (!node) return null;
  const parts = partsOf(node, constants);
  const origin = parts.findIndex((part) => typeof part !== "string" && isOrigin(part));
  let text = "";
  for (const part of parts.slice(origin + 1)) {
    if (typeof part !== "string") break;
    text += part;
  }
  if (!text.startsWith("/") || text.startsWith("//")) return null;
  return text.split("?")[0]!;
}

/** Every URL one module reaches. */
function reaches(source: string, lang: Lang): Reach[] {
  const line = lineOf(source);
  const program = parse(source, lang);
  const constants = constantsOf(program);
  const rests = new Set<string>();
  for (const statement of program.body as Node[]) {
    if (statement.type !== "ImportDeclaration") continue;
    if (!isLocalSource(String((statement.source as Node).value))) continue;
    for (const spec of statement.specifiers as Node[]) {
      if (spec.type !== "ImportSpecifier") continue;
      const imported = spec.imported as Node;
      if ((imported.type === "Identifier" ? imported.name : imported.value) === "rest") {
        rests.add(String((spec.local as Node).name));
      }
    }
  }
  const found: Reach[] = [];
  walk(program, (node, parent) => {
    if (node.type === "JSXAttribute") {
      const name = node.name as Node;
      const value = node.value as Node | null;
      if (name.type !== "JSXIdentifier" || !FETCHED.has(String(name.name)) || !value) return;
      const url = value.type === "JSXExpressionContainer" ? (value.expression as Node) : value;
      const path = pathOf(url, constants);
      if (path === null) return;
      const element = parent?.name as Node | undefined;
      const tag = element?.type === "JSXIdentifier" ? String(element.name) : "element";
      found.push({
        line: line(node.start),
        call: `<${tag} ${String(name.name)}>`,
        path,
        socket: false,
      });
      return;
    }
    if (node.type !== "CallExpression" && node.type !== "NewExpression") return;
    const callee = node.callee as Node;
    const args = node.arguments as Node[];
    const reach = (call: string, url: Node | undefined, socket = false) =>
      found.push({ line: line(node.start), call, path: pathOf(url, constants), socket });

    if (node.type === "NewExpression") {
      const name = callee.type === "Identifier" ? String(callee.name) : (memberName(callee) ?? "");
      if (DIALS.has(name)) reach(`new ${name}`, args[0], name === "WebSocket");
      return;
    }
    if (callee.type === "Identifier" && callee.name === "fetch") {
      reach("fetch", args[0]);
      return;
    }
    const method = memberName(callee);
    if (method === null) return;
    const object = callee.object as Node;
    if (method === "fetch") {
      // `window.fetch(…)` and its kin; `transport.test.ts` is what refuses
      // them outside src/protocol/, and here they are read like any fetch.
      // Any other `.fetch(` is a method that shares the name.
      if (object.type === "Identifier" && GLOBALS.has(String(object.name))) reach("fetch", args[0]);
      return;
    }
    if (method === "sendBeacon") {
      reach("sendBeacon", args[0]);
      return;
    }
    if (object.type !== "Identifier" || !rests.has(String(object.name))) return;
    if (method === "request") reach("rest.request", args[1]);
    else if (REST_PATH_FIRST.has(method)) reach(`rest.${method}`, args[0]);
  });
  return found;
}

/**
 * Every path the shell's own markup names, by the same attributes — the one
 * part of the dashboard that is HTML rather than a module. A path naming a file
 * under the dev server's root is Vite's to serve, not the engine's.
 */
function shellReaches(html: string): Reach[] {
  const line = lineOf(html);
  const text = html.replace(/<!--[\s\S]*?-->/g, (m) => m.replace(/[^\n]/g, " "));
  const found: Reach[] = [];
  for (const tag of text.matchAll(/<([a-z][a-z0-9-]*)\b[^>]*>/gi)) {
    for (const attribute of tag[0].matchAll(/\s([a-z-]+)\s*=\s*(["'])(.*?)\2/gi)) {
      const [, name = "", , value = ""] = attribute;
      if (!FETCHED.has(name.toLowerCase())) continue;
      if (!value.startsWith("/") || value.startsWith("//")) continue;
      const path = value.split(/[?#]/)[0]!;
      if (existsSync(join(process.cwd(), path))) continue;
      found.push({
        line: line(tag.index + attribute.index),
        call: `<${tag[1]} ${name}>`,
        path,
        socket: false,
      });
    }
  }
  return found;
}

/** One entry of the dev server's proxy, as vite.config.ts writes it. */
interface Entry {
  prefix: string;
  ws: boolean;
}

/** vite.config.ts, parsed. */
const config = () => parse(readFileSync(join(process.cwd(), "vite.config.ts"), "utf8"), "ts");

/** The `base` the dev server serves the dashboard's own modules under. */
function base(): string | null {
  let found: string | null = null;
  walk(config(), (node) => {
    if (node.type !== "Property" || (node.key as Node).type !== "Identifier") return;
    if ((node.key as Node).name === "base") found = stringValue(node.value as Node);
  });
  return found;
}

/** The `server.proxy` entries vite.config.ts declares, parsed. */
function proxied(): Entry[] {
  const entries: Entry[] = [];
  walk(config(), (node) => {
    if (node.type !== "Property" || (node.key as Node).type !== "Identifier") return;
    if ((node.key as Node).name !== "proxy" || (node.value as Node).type !== "ObjectExpression") {
      return;
    }
    for (const property of (node.value as Node).properties as Node[]) {
      if (property.type !== "Property") continue;
      const prefix = stringValue(property.key as Node);
      if (prefix === null) continue;
      const options = property.value as Node;
      const ws =
        options.type === "ObjectExpression" &&
        (options.properties as Node[]).some(
          (p) =>
            p.type === "Property" &&
            (p.key as Node).type === "Identifier" &&
            (p.key as Node).name === "ws" &&
            (p.value as Node).type === "Literal" &&
            (p.value as Node).value === true,
        );
      entries.push({ prefix, ws });
    }
    return false;
  });
  return entries;
}

/**
 * Whether an entry forwards a path — Vite's own rule for a string key, which
 * is a plain prefix.
 */
const forwards = (entry: Entry, path: string) => path.startsWith(entry.prefix);

describe("the dev server proxy", () => {
  const shell = shellReaches(readFileSync(join(process.cwd(), "index.html"), "utf8")).map(
    (reach) => ({ file: "index.html", ...reach }),
  );
  const tree = [
    ...modules().flatMap(({ path, text, lang }) =>
      reaches(text, lang).map((reach) => ({ file: path, ...reach })),
    ),
    ...shell,
  ];
  const read = tree.filter((r) => r.path !== null);
  const entries = proxied();

  test("forwards every engine path the dashboard reaches", () => {
    const missing = read
      .filter((r) => !entries.some((entry) => forwards(entry, r.path!)))
      .map((r) => `${r.file}:${r.line} — ${r.call} ${r.path}`);
    expect(missing, "add a server.proxy entry for this prefix to vite.config.ts").toEqual([]);
  });

  test("upgrades every socket the dashboard dials", () => {
    const flat = read
      .filter((r) => r.socket && !entries.some((e) => e.ws && forwards(e, r.path!)))
      .map((r) => `${r.file}:${r.line} — ${r.path}`);
    expect(flat, "give this proxy entry `ws: true`, or the socket never upgrades").toEqual([]);
  });

  test("forwards nothing the dashboard does not reach", () => {
    const dead = entries
      .filter((entry) => !read.some((r) => forwards(entry, r.path!)))
      .map((entry) => entry.prefix);
    expect(dead, "nothing fetches under this prefix: remove it from vite.config.ts").toEqual([]);
  });

  test("forwards nothing the dev server serves itself", () => {
    // Vite serves the dashboard's own modules under `base`, and an entry whose
    // prefix covers it hands every one of them to the engine: the dev server
    // then runs whatever bundle the engine last embedded rather than the
    // source being edited. Which is why the brand mark's entry is its one
    // file and not `/static`.
    const own = base();
    expect(own, "vite.config.ts no longer declares a base").not.toBeNull();
    const captured = entries.filter((e) => own!.startsWith(e.prefix)).map((e) => e.prefix);
    expect(captured, "this entry forwards the dashboard's own modules to the engine").toEqual([]);
  });

  test("reads the path of every call that reaches the engine", () => {
    const unread = tree
      .filter((r) => r.path === null)
      .filter((r) => !FORWARDS.some((f) => f.path === r.file && f.call === r.call))
      .map((r) => `${r.file}:${r.line} — ${r.call}`);
    expect(
      unread,
      "start the URL with its path as a literal (or a module constant) so this gate can " +
        "check it is proxied; a transport forwarding its caller's path goes in FORWARDS",
    ).toEqual([]);
  });

  test("and every forward it names still forwards", () => {
    const stale = FORWARDS.filter(
      (f) => !tree.some((r) => r.path === null && r.file === f.path && r.call === f.call),
    ).map((f) => `${f.path} — ${f.call}`);
    expect(stale, "this forward is gone: delete its FORWARDS entry").toEqual([]);
  });

  test("finds the paths it is meant to be checking", () => {
    // Without this every test above passes on an empty set, which is exactly
    // how a scanner that stops matching reports everything as fine — and the
    // two-way test would then demand an empty proxy.
    expect(entries.length).toBeGreaterThan(3);
    expect(read.length).toBeGreaterThan(15);
    const paths = new Set(read.map((r) => r.path));
    for (const path of ["/secrets", "/setup/integrations", "/config", "/stream/snapshot"]) {
      expect(paths, `nothing reached ${path}`).toContain(path);
    }
    expect(read.some((r) => r.socket && r.path === "/ws/stream")).toBe(true);
    // And the markup halves: the shell names at least its tab icon, and a JSX
    // attribute is read at all.
    expect(shell.length, "the shell's markup names no path any more").toBeGreaterThan(0);
    expect(read.some((r) => r.call.startsWith("<") && r.file !== "index.html")).toBe(true);
  });

  // THE SCANNER'S OWN CASES: each shape the tree writes a URL in, read to the
  // path it names — and the shapes it must report as unreadable.
  const REST = 'import { rest } from "~/protocol/index.ts";\n';
  test.each([
    ["a literal", `${REST}rest.get("/secrets");`, "/secrets"],
    ["a query string", `${REST}rest.get("/config?format=yaml");`, "/config"],
    [
      "a template",
      `${REST}rest.post(\`/setup/integrations/\${key}/app\`, {});`,
      "/setup/integrations/",
    ],
    [
      "a concatenation",
      `${REST}rest.putText("/secrets/" + encodeURIComponent(n), v);`,
      "/secrets/",
    ],
    [
      "the whole answer",
      `${REST}rest.request("GET", \`/config/revisions/\${id}\`);`,
      "/config/revisions/",
    ],
    ["the origin in front", 'fetch(location.origin + "/stream/snapshot");', "/stream/snapshot"],
    ["a module constant", 'const PATH = "/ws/stream";\nfetch(PATH, {});', "/ws/stream"],
    [
      "a socket's URL",
      'const PATH = "/ws/stream";\nnew WebSocket(`${proto}://${location.host}${PATH}${qs}`);',
      "/ws/stream",
    ],
    ["a beacon", 'navigator.sendBeacon("/stream/leave", body);', "/stream/leave"],
    [
      "a renamed transport",
      'import { rest as http } from "./rest.ts";\nhttp.del("/secrets/x");',
      "/secrets/x",
    ],
  ])("the scan reads %s", (_name, source, path) => {
    expect(reaches(source, "tsx").map((r) => r.path)).toEqual([path]);
  });

  test.each([
    ["a variable", `${REST}rest.get(path);`],
    ["a computed head", `${REST}rest.get(base + "/x");`],
    ["a relative URL", 'fetch("secrets");'],
    ["another origin", 'fetch("//cdn.example.com/x");'],
    ["the transport's own forward", "fetch(location.origin + withQuery(path, query));"],
  ])("the scan reports %s as unreadable", (_name, source) => {
    expect(reaches(source, "tsx").map((r) => r.path)).toEqual([null]);
  });

  test.each([
    ["a local list called rest", "const [first, ...rest] = parts;\nrest.slice(1);"],
    ["a method called fetch", 'loader.fetch("/x");'],
    ["refetch", "refetch();"],
    ["a route link", '<a href={href(["work"])}>Work</a>;'],
    ["a hash link", '<a href="#/work">Work</a>;'],
    ["another site", '<a href="https://example.com/docs">Docs</a>;'],
    ["an attribute nothing fetches", '<div title="/not/a/url" />;'],
  ])("the scan leaves %s alone", (_name, source) => {
    expect(reaches(source, "tsx")).toEqual([]);
  });

  test.each([
    ["an image", '<img src="/static/crewlet-icon.svg" alt="" />;', "/static/crewlet-icon.svg"],
    ["a link", '<a href="/secrets/export">Export</a>;', "/secrets/export"],
    ["a poster", "<video poster={`/media/${id}.png`} />;", "/media/"],
    [
      "a module constant",
      'const MARK = "/static/mark.svg";\n<img src={MARK} />;',
      "/static/mark.svg",
    ],
  ])("the scan reads markup: %s", (_name, source, path) => {
    expect(reaches(source, "tsx").map((r) => r.path)).toEqual([path]);
  });

  test("the scan reads the shell's markup, and leaves Vite's own files to Vite", () => {
    const html = [
      '<!-- <link rel="icon" href="/commented/out.svg" /> -->',
      '<link rel="icon" type="image/svg+xml" href="/static/crewlet-icon.svg" />',
      "<link rel='manifest' href='/static/manifest.json?v=1' />",
      '<script type="module" src="/src/main.tsx"></script>',
      '<a href="https://example.com/">elsewhere</a>',
    ].join("\n");
    expect(shellReaches(html).map((r) => `${r.line} ${r.call} ${r.path}`)).toEqual([
      "2 <link href> /static/crewlet-icon.svg",
      "3 <link href> /static/manifest.json",
    ]);
  });
});
