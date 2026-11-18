// @vitest-environment node
/**
 * A time with a word beside it is in a sentence and says it in words (`ago`, `<AgoTime>`);
 * one standing alone or set off by a middle dot, comma or brackets keeps the short form.
 */

import { describe, expect, test } from "vitest";
import { lineOf, modules, parse, walk, type Module, type Node } from "~/test/source.ts";

type Form = "short" | "words";

interface Time {
  node: Node;
  form: Form;
  line: number;
  /** The enclosing function or component, which an exception names. */
  owner: string;
}

interface Reading {
  times: Time[];
  sentence: (time: Time) => boolean;
}

/** The producers, by the module each is imported from. */
const CALLS: { from: RegExp; name: string; form: Form }[] = [
  { from: /(^|\/)lib\/format(\.ts)?$|^\.\/format(\.ts)?$/, name: "relTime", form: "short" },
  { from: /(^|\/)lib\/format(\.ts)?$|^\.\/format(\.ts)?$/, name: "inTime", form: "short" },
  { from: /(^|\/)lib\/ago(\.ts)?$|^\.\/ago(\.ts)?$/, name: "ago", form: "words" },
];
const ELEMENTS: { from: RegExp; name: string; form: Form }[] = [
  { from: /^@crewlethq\/ui$/, name: "RelativeTime", form: "short" },
  {
    from: /(^|\/)components\/AgoTime(\.tsx)?$|^\.\/AgoTime(\.tsx)?$/,
    name: "AgoTime",
    form: "words",
  },
];
/** A call that hands its argument on as the same words, capitalised. */
const OPEN_SENTENCE = /(^|\/)lib\/format(\.ts)?$|^\.\/format(\.ts)?$/;

/** Each excused time, keyed by module and enclosing function, with its reason. */
const EXCEPTIONS: Record<string, { form: Form; why: string }> = {
  "routes/gitlet/ui.tsx#Byline": { form: "words", why: "the verb arrives as a prop" },
  "routes/gitlet/change/ChangePage.tsx#ChangeHeader": {
    form: "words",
    why: "the header sentence's time, after its verb, in a time element of its own",
  },
  "routes/gitlet/checks/marks.tsx#EndedAgo": {
    form: "words",
    why: "a check's or queue's time after its state word, which arrives as a prop",
  },
  "routes/gitlet/change/Merge.tsx#measured": {
    form: "words",
    why: "the Merges sentence's time, after its commit, in a time element of its own",
  },
  "routes/gitlet/change/Timeline.tsx#Said": {
    form: "words",
    why: "a byline whose face is on the timeline's rail: the verb arrives as a prop",
  },
  "routes/gitlet/commit/Provenance.tsx#Row": {
    form: "words",
    why: "a provenance row's time at its end, the close of the row's sentence",
  },
  "routes/gitlet/repo/Branches.tsx#MovedBy": {
    form: "words",
    why: "a byline: who moved the branch arrives as a prop",
  },
  "components/AgoTime.tsx#AgoTime": { form: "words", why: "the element itself" },
  "routes/settings/integrations/access/logic.ts#grantExpiry": {
    form: "short",
    why: "a grant tag's label",
  },
  "routes/settings/integrations/catalogue.tsx#syncWords": {
    form: "short",
    why: "part of the Last check figure",
  },
};

/** The producers' own homes. */
const HOMES = new Set(["lib/format.ts", "lib/ago.ts"]);

const CLOSERS = "·,().";
const BLANK = /^\s*$/;

type Dir = -1 | 1;
type Verdict = boolean | null;

function read(text: string, lang: Module["lang"]): Reading {
  const program = parse(text, lang);
  const line = lineOf(text);
  const up = new Map<Node, { parent: Node; key: string }>();
  const calls = new Map<string, Form>();
  const elements = new Map<string, Form>();
  const openers = new Set<string>();
  walk(program, (node, parent, key) => {
    if (parent) up.set(node, { parent, key });
    if (node.type !== "ImportDeclaration") return;
    const from = String((node.source as Node).value);
    for (const spec of node.specifiers as Node[]) {
      if (spec.type !== "ImportSpecifier") continue;
      const imported = String((spec.imported as Node).name);
      const local = String((spec.local as Node).name);
      for (const c of CALLS) if (c.name === imported && c.from.test(from)) calls.set(local, c.form);
      for (const e of ELEMENTS)
        if (e.name === imported && e.from.test(from)) elements.set(local, e.form);
      if (imported === "openSentence" && OPEN_SENTENCE.test(from)) openers.add(local);
    }
  });

  const parentOf = (node: Node) => up.get(node);
  const nameOf = (el: Node): string | null => {
    const name = (el.openingElement as Node | undefined)?.name as Node | undefined;
    return name?.type === "JSXIdentifier" ? String(name.name) : null;
  };
  const isIntrinsic = (el: Node) => el.type === "JSXFragment" || /^[a-z]/.test(nameOf(el) ?? "");
  const hasClass = (el: Node) =>
    el.type === "JSXElement" &&
    ((el.openingElement as Node).attributes as Node[]).some(
      (a) => a.type === "JSXAttribute" && (a.name as Node).name === "className",
    );
  /** A child that shows nothing: blank text, `{" "}`, or an empty container. */
  const isBlank = (child: Node) =>
    (child.type === "JSXText" && BLANK.test(String(child.value))) ||
    (child.type === "JSXExpressionContainer" &&
      ((child.expression as Node).type === "JSXEmptyExpression" ||
        BLANK.test(stringOf(child.expression as Node) ?? "x")));

  /** The node that stands for the time where it is read, past every transparent holder. */
  const climb = (start: Node): Node => {
    let unit = start;
    for (;;) {
      const at = parentOf(unit);
      if (!at) return unit;
      const { parent, key } = at;
      const t = parent.type;
      if (
        t === "JSXExpressionContainer" ||
        t === "TSAsExpression" ||
        t === "TSNonNullExpression" ||
        t === "ChainExpression" ||
        (t === "ConditionalExpression" && key !== "test") ||
        (t === "LogicalExpression" && key === "right")
      ) {
        if (t === "JSXExpressionContainer" && parentOf(parent)?.parent.type === "JSXAttribute") {
          const clocked = clockedOf(parent);
          if (!clocked) return unit;
          unit = clocked;
          continue;
        }
        unit = parent;
        continue;
      }
      if (t === "ArrowFunctionExpression" && key === "body") {
        const container = parentOf(parent)?.parent;
        const clocked = container && clockedOf(container);
        if (!clocked) return unit;
        unit = clocked;
        continue;
      }
      if (
        t === "CallExpression" &&
        key === "arguments" &&
        (parent.callee as Node).type === "Identifier" &&
        openers.has(String((parent.callee as Node).name))
      ) {
        unit = parent;
        continue;
      }
      if ((t === "JSXElement" || t === "JSXFragment") && key === "children") {
        const shown = (parent.children as Node[]).filter((c) => !isBlank(c));
        if (shown.length === 1 && isIntrinsic(parent) && !hasClass(parent)) {
          unit = parent;
          continue;
        }
      }
      return unit;
    }
  };

  /** The Clocked element whose `text` attribute is this container, if it is one. */
  const clockedOf = (container: Node): Node | null => {
    const attr = parentOf(container)?.parent;
    if (attr?.type !== "JSXAttribute" || (attr.name as Node).name !== "text") return null;
    const opening = parentOf(attr)?.parent;
    const el = opening && parentOf(opening)?.parent;
    return el && nameOf(el) === "Clocked" ? el : null;
  };

  /** A constant string, or a template's text on the side facing the time. */
  function stringOf(node: Node, dir: Dir = 1): string | null {
    if (node.type === "Literal") return typeof node.value === "string" ? node.value : null;
    if (node.type !== "TemplateLiteral") return null;
    const quasis = node.quasis as Node[];
    const q = dir === -1 ? quasis[quasis.length - 1] : quasis[0];
    return String((q?.value as { cooked?: string } | undefined)?.cooked ?? "");
  }

  /** What one piece of text says about the side it faces: a word, a closer, or nothing yet. */
  const decide = (text: string, dir: Dir): Verdict => {
    const s = dir === -1 ? text.trimEnd() : text.trimStart();
    if (s === "") return null;
    const facing = dir === -1 ? s[s.length - 1]! : s[0]!;
    if (CLOSERS.includes(facing)) return false;
    const word = dir === -1 ? /\S+$/.exec(s)![0] : /^\S+/.exec(s)![0];
    return /\p{L}/u.test(word);
  };

  /** A text expression's verdict; a conditional's word in any branch makes a sentence. */
  const alongText = (node: Node, dir: Dir): Verdict => {
    if (node.type === "ConditionalExpression" || node.type === "LogicalExpression") {
      const branches = (
        node.type === "ConditionalExpression"
          ? [node.consequent, node.alternate]
          : node.operator === "&&"
            ? [node.right]
            : [node.left, node.right]
      ) as Node[];
      const vs = branches.map((b) => alongText(b, dir));
      return vs.includes(true) ? true : vs.includes(false) ? false : null;
    }
    const s = stringOf(node, dir);
    return s === null ? null : decide(s, dir);
  };

  /** Reads a run of JSX children from one edge; null when none of them decides. */
  const alongChildren = (children: Node[], from: number, dir: Dir): Verdict => {
    for (let i = from; i >= 0 && i < children.length; i += dir) {
      const child = children[i]!;
      if (child.type === "JSXText") {
        const v = decide(String(child.value), dir);
        if (v !== null) return v;
      } else if (child.type === "JSXExpressionContainer") {
        const v = alongText(child.expression as Node, dir);
        if (v !== null) return v;
      } else if (child.type === "JSXElement" || child.type === "JSXFragment") {
        if (!isIntrinsic(child) || hasClass(child)) return false;
        const inner = child.children as Node[];
        const v = alongChildren(inner, dir === -1 ? inner.length - 1 : 0, dir);
        if (v !== null) return v;
      }
    }
    return null;
  };

  /** Whether a word stands beside the unit on one side. */
  const side = (start: Node, dir: Dir): boolean => {
    const unit = climb(start);
    const at = parentOf(unit);
    if (!at) return false;
    const { parent } = at;
    if (parent.type === "TemplateLiteral") {
      const quasis = parent.quasis as Node[];
      const index = (parent.expressions as Node[]).indexOf(unit);
      for (let q = dir === -1 ? index : index + 1; q >= 0 && q < quasis.length; q += dir) {
        const v = decide(String((quasis[q]!.value as { cooked?: string }).cooked ?? ""), dir);
        if (v !== null) return v;
      }
      return side(parent, dir);
    }
    if (parent.type === "JSXElement" || parent.type === "JSXFragment") {
      const children = parent.children as Node[];
      return alongChildren(children, children.indexOf(unit) + dir, dir) ?? false;
    }
    return false;
  };

  const ownerOf = (node: Node): string => {
    for (let at = parentOf(node); at; at = parentOf(at.parent)) {
      const p = at.parent;
      if (p.type === "FunctionDeclaration" && p.id) return String((p.id as Node).name);
      if (
        (p.type === "ArrowFunctionExpression" || p.type === "FunctionExpression") &&
        parentOf(p)?.parent.type === "VariableDeclarator"
      )
        return String(((parentOf(p)!.parent.id as Node) ?? {}).name);
    }
    return "";
  };

  const times: Time[] = [];
  walk(program, (node) => {
    let form: Form | undefined;
    if (node.type === "CallExpression" && (node.callee as Node).type === "Identifier")
      form = calls.get(String((node.callee as Node).name));
    else if (node.type === "JSXElement") form = elements.get(nameOf(node) ?? "");
    if (form) times.push({ node, form, line: line(node.start), owner: ownerOf(node) });
  });
  return { times, sentence: (time) => side(time.node, -1) || side(time.node, 1) };
}

describe("a relative time", () => {
  test("is said in words inside a sentence and short where it stands alone", () => {
    const found: string[] = [];
    const used = new Set<string>();
    const count = { short: 0, words: 0 };
    for (const module of modules()) {
      if (HOMES.has(module.path) || module.path.startsWith("test/")) continue;
      const { times, sentence } = read(module.text, module.lang);
      for (const time of times) {
        count[time.form]++;
        const inSentence = sentence(time);
        const wrong = time.form === "short" ? inSentence : !inSentence;
        const key = `${module.path}#${time.owner}`;
        const excused = EXCEPTIONS[key];
        if (!wrong) continue;
        if (excused && excused.form === time.form) {
          used.add(key);
          continue;
        }
        found.push(
          time.form === "short"
            ? `${module.path}:${time.line} says a time inside a sentence in the short form; say it with ago (or <AgoTime>), or set it off with a middle dot, a comma or brackets if it is a label`
            : `${module.path}:${time.line} says a time in words where it stands alone; a cell, chip or label keeps relTime`,
        );
      }
    }
    for (const key of Object.keys(EXCEPTIONS))
      if (!used.has(key)) found.push(`stale exception ${key}: it excuses no time`);
    expect(found).toEqual([]);
    expect(count.short, "the gate read too few short-form times").toBeGreaterThan(40);
    expect(count.words, "the gate read too few times in words").toBeGreaterThan(40);
  });

  const HEADER = [
    'import { relTime } from "~/lib/format.ts";',
    'import { ago } from "~/lib/ago.ts";',
    'import { RelativeTime } from "@crewlethq/ui";',
    'import { Clocked } from "./Clocked.tsx";',
  ].join("\n");
  const reads = (snippet: string): boolean => {
    const { times, sentence } = read(`${HEADER}\n${snippet}\n`, "tsx");
    expect(times).toHaveLength(1);
    return sentence(times[0]!);
  };

  test.each([
    "const x = <p>Paused by {name} {relTime(at, now)}</p>;",
    "const s = `Last saved ${relTime(at, now)} by ${who}`;",
    'const x = <li>Last written{" "}{at ? <time dateTime={at}><b>{relTime(at, now)}</b></time> : null}</li>;',
    'const x = <>Turned off at {lane}{at && <>{" "}<Clocked text={(now) => relTime(at, now)} /></>}</>;',
    "const x = <><strong>Paused by {name}</strong> {relTime(at, now)}</>;",
    'const x = <Stat sub={`set by ${name}${at ? ` ${relTime(at, now)}` : ""}`} />;',
    "const x = <p>Checked <RelativeTime value={at} />.</p>;",
    'const x = <>{compact ? "back " : `${w}, back `}<RelativeTime value={at} /></>;',
  ])("can tell a sentence: %s", (snippet) => {
    expect(reads(snippet)).toBe(true);
  });

  test.each([
    'const x = <Item trailing={<span className="t-caption">{ago(at, now)}</span>} />;',
    'const s = [name, relTime(at, now)].join(" · ");',
    'const x = <>Shared by {name} ·{" "}{relTime(at, now)}</>;',
    "const x = <Row hint={`${who}, ${relTime(at, now)}`} />;",
    "const x = <>started {fmtDateTime(at)} ({relTime(at, now)})</>;",
    'const f = { label: "Last seen", value: relTime(at, now) };',
    'const x = <div><h3>Plan</h3><time className="phase-meta">{relTime(at, now)}</time></div>;',
    "const when = <Clocked text={(now) => relTime(at, now)} />;",
  ])("can tell a label: %s", (snippet) => {
    expect(reads(snippet)).toBe(false);
  });
});
