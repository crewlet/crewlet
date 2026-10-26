// @vitest-environment node
/**
 * The dashboard reaches no injection sink, which is what lets its policy
 * enforce Trusted Types with no policy at all.
 *
 * The engine serves the dashboard with `require-trusted-types-for 'script';
 * trusted-types 'none'` (internal/api/pagepolicy `Dashboard`): every string
 * handed to a sink that parses markup or loads script throws, and no code on
 * the origin may create a policy to bless one. That is safe to enforce only
 * because nothing here writes to such a sink: the Markdown renderer builds
 * React elements and never parses markup (lib/markdown.ts), an artifact's page
 * is framed on its own origin rather than written into this one, and React
 * reaches `innerHTML` only for `dangerouslySetInnerHTML` and for a rendered
 * `<script>` element, neither of which the dashboard draws.
 *
 * So the rule is read off the SOURCE: a sink introduced here would be a
 * screen that throws under the policy the moment it runs, and one that worked
 * would be one somebody "fixed" by loosening the policy for the whole origin.
 */

import { describe, expect, test } from "vitest";
import { type Node, lineOf, memberName, modules, parse, walk } from "~/test/source.ts";

/** Properties whose write or call parses markup or loads script, on any object. */
const SINK_MEMBERS = new Set([
  "innerHTML",
  "outerHTML",
  "insertAdjacentHTML",
  "createContextualFragment",
  "parseFromString",
  "srcdoc",
  "createPolicy",
  "importScripts",
]);

/** Constructors and calls that compile or load script from a string. */
const SINK_NEW = new Set(["Function", "Worker", "SharedWorker"]);
const SINK_CALLS = new Set(["eval", "importScripts"]);

/** JSX props that hand React markup to write, or a frame a document to parse. */
const SINK_PROPS = new Set(["dangerouslySetInnerHTML", "srcDoc"]);

/** Every sink a module reaches, as `line: what`. */
function sinksIn(text: string, lang: Parameters<typeof parse>[1]): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    const at = line(node.start);
    const member = memberName(node);
    if (member !== null && SINK_MEMBERS.has(member)) out.push(`${at}: .${member}`);
    if (member === "write" || member === "writeln") {
      const object = node.object as Node;
      if (object.type === "Identifier" && object.name === "document") {
        out.push(`${at}: document.${member}`);
      }
    }
    if (node.type === "NewExpression" || node.type === "CallExpression") {
      const callee = node.callee as Node;
      if (callee.type === "Identifier") {
        const name = String(callee.name);
        if (node.type === "NewExpression" && SINK_NEW.has(name)) out.push(`${at}: new ${name}`);
        if (node.type === "CallExpression" && SINK_CALLS.has(name)) out.push(`${at}: ${name}()`);
      }
    }
    if (node.type === "JSXAttribute") {
      const name = node.name as Node;
      if (name.type === "JSXIdentifier" && SINK_PROPS.has(String(name.name))) {
        out.push(`${at}: ${String(name.name)}`);
      }
    }
  });
  return out;
}

describe("the dashboard under Trusted Types", () => {
  test("no module reaches a sink the policy refuses", () => {
    const found: string[] = [];
    for (const mod of modules()) {
      if (mod.lang === "dts") continue;
      for (const sink of sinksIn(mod.text, mod.lang)) found.push(`${mod.path}:${sink}`);
    }
    expect(
      found,
      "the dashboard enforces Trusted Types with no policy: build the element instead",
    ).toEqual([]);
  });

  // THE ASSISTANT DRAWS A MODEL'S WORDS, the one text in the product written
  // by something that may have read anything. Every module of its panel and
  // its transport is read by the rule above, and this says so by name, so a
  // move that took the panel out of the walk is a failure rather than a pass.
  test("the assistant's panel, its prose and its transport are among the modules read", () => {
    const paths = new Set(modules().map((m) => m.path));
    for (const path of [
      "app/assistant/chunk/Drawer.tsx",
      "app/assistant/chunk/Thread.tsx",
      "app/assistant/chunk/blocks.tsx",
      "lib/markdown.ts",
      "protocol/assistant.ts",
    ]) {
      expect(paths.has(path), path).toBe(true);
      const mod = modules().find((m) => m.path === path)!;
      expect(sinksIn(mod.text, mod.lang), path).toEqual([]);
    }
  });

  test("the rule finds every kind of sink it names", () => {
    const sample = [
      "el.innerHTML = s;",
      "el.outerHTML = s;",
      "el.insertAdjacentHTML('beforeend', s);",
      "range.createContextualFragment(s);",
      "new DOMParser().parseFromString(s, 'text/html');",
      "frame.srcdoc = s;",
      "trustedTypes.createPolicy('p', {});",
      "document.write(s);",
      "document.writeln(s);",
      "new Function(s);",
      "new Worker(u);",
      "eval(s);",
      "el['innerHTML'] = s;",
      "const a = <div dangerouslySetInnerHTML={{ __html: s }} />;",
      "const b = <iframe srcDoc={s} />;",
    ].join("\n");
    expect(sinksIn(sample, "tsx")).toEqual([
      "1: .innerHTML",
      "2: .outerHTML",
      "3: .insertAdjacentHTML",
      "4: .createContextualFragment",
      "5: .parseFromString",
      "6: .srcdoc",
      "7: .createPolicy",
      "8: document.write",
      "9: document.writeln",
      "10: new Function",
      "11: new Worker",
      "12: eval()",
      "13: .innerHTML",
      "14: dangerouslySetInnerHTML",
      "15: srcDoc",
    ]);
  });

  test("a word in a comment or a string is not a sink", () => {
    expect(
      sinksIn(
        '// never innerHTML\nconst note = "no dangerouslySetInnerHTML here";\nlog.write(s);',
        "ts",
      ),
    ).toEqual([]);
  });
});
