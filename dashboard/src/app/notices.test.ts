// @vitest-environment node

/**
 * One notice system (docs/reference/dashboard-design.md, "Notices"): a
 * Callout's body is words. Its list is the kit's NoticeList, its one action
 * sits in `action`, a dismissal is `onDismiss`, and its glyph is its tone's.
 */

import { expect, test } from "vitest";
import { lineOf, modules, parse, walk, type Node } from "../test/source.ts";

const LISTS = new Set(["ul", "ol", "li"]);
const ACTIONS = new Set([
  "Button",
  "button",
  "a",
  "Link",
  "ScreenLink",
  "ButtonLink",
  "IconButton",
]);
/** Not the sentence: technical details folded away, and the kit list that places its own actions. */
const FOLDED = new Set(["Disclosure", "NoticeList"]);

const tagOf = (element: Node): string | null => {
  const name = (element.openingElement as Node).name as Node;
  return name.type === "JSXIdentifier" ? (name.name as string) : null;
};

const attributes = (element: Node): Node[] =>
  ((element.openingElement as Node).attributes as Node[]).filter((a) => a.type === "JSXAttribute");

const attrName = (attr: Node): string => (attr.name as Node).name as string;

/** Every JSX element under `root` that is not folded away, by its tag. */
function tagsUnder(root: Node): { tag: string; at: number }[] {
  const out: { tag: string; at: number }[] = [];
  walk(root, (node) => {
    if (node.type !== "JSXElement") return;
    const tag = tagOf(node);
    if (tag === null) return;
    if (FOLDED.has(tag)) return false;
    out.push({ tag, at: node.start });
  });
  return out;
}

const texts = (root: Node): string[] => {
  const out: string[] = [];
  walk(root, (node) => {
    if (node.type === "JSXText") out.push(String(node.value).trim());
  });
  return out;
};

interface Finding {
  rule: "list" | "action" | "icon" | "dismiss";
  where: string;
}

function findings(): Finding[] {
  const out: Finding[] = [];
  for (const { path, text } of modules()) {
    if (!path.endsWith(".tsx")) continue;
    if (!text.includes("<Callout")) continue;
    const line = lineOf(text);
    walk(parse(text, "tsx"), (node) => {
      if (node.type !== "JSXElement" || tagOf(node) !== "Callout") return;
      const where = `${path}:${line(node.start)}`;
      for (const attr of attributes(node)) {
        if (attrName(attr) === "icon") out.push({ rule: "icon", where });
        if (
          attrName(attr) === "action" &&
          attr.value &&
          texts(attr.value as Node).includes("Dismiss")
        )
          out.push({ rule: "dismiss", where });
      }
      for (const child of node.children as Node[]) {
        for (const { tag } of tagsUnder(child)) {
          if (LISTS.has(tag)) out.push({ rule: "list", where });
          if (ACTIONS.has(tag)) out.push({ rule: "action", where });
        }
      }
    });
  }
  return [...new Map(out.map((f) => [`${f.rule} ${f.where}`, f])).values()];
}

const all = findings();
if (process.env.NOTICES_DUMP) console.log("DUMP" + JSON.stringify(all));
const of = (rule: Finding["rule"]) => all.filter((f) => f.rule === rule).map((f) => f.where);

test("a callout's list is the kit's NoticeList, never a list built in its body", () => {
  expect(of("list")).toEqual([]);
});

test("a callout's one action sits in its action slot, never a link or a button in its sentence", () => {
  expect(of("action")).toEqual([]);
});

test("a callout's glyph is its tone's, so the tone is never carried by colour alone", () => {
  expect(of("icon")).toEqual([]);
});

test("a callout the reader may put away says so with onDismiss, not a Dismiss button", () => {
  expect(of("dismiss")).toEqual([]);
});
