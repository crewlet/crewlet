// @vitest-environment node
/**
 * A control's words are drawn unless the same words are already on screen
 * beside it: every `hideLabel` and `labelHidden` is declared here with where.
 */

import { expect, test } from "vitest";
import { lineOf, modules, parse, walk, type Node } from "~/test/source.ts";

/** A hidden label, and where its words are drawn instead. */
interface Declared {
  /** The module, relative to `src/`. */
  where: string;
  /** The element that hides its label. */
  element: string;
  why: string;
}

const DECLARED: Declared[] = [
  {
    where: "routes/settings/integrations/GooglePane.tsx",
    element: "Switch",
    why: "Each product's switch ends its row, whose first line is the product's name.",
  },
  {
    where: "ui/FacetRail.tsx",
    element: "FilterChipGroup",
    why: "The rail sits under the control it narrows, which names the dimension.",
  },
  {
    where: "routes/gitlet/queue/Box.tsx",
    element: "Meter",
    why: "The elapsed words beside the bar say how long the checks have run and of what.",
  },
  {
    where: "routes/home/Kpis.tsx",
    element: "Meter",
    why: "The tile's label and figure head the bar, and its caption says what it is out of.",
  },
  {
    where: "routes/live/LiveNow.tsx",
    element: "FilterChipGroup",
    why: "Its one chip says what it filters to.",
  },
  {
    where: "routes/artifacts/DashboardWidget.tsx",
    element: "StatCard",
    why: "The widget's frame title names the number.",
  },
  {
    where: "routes/artifacts/DashboardWidget.tsx",
    element: "Meter",
    why: "The stat beside it draws the meter's words.",
  },
  {
    where: "routes/inbox/InboxColumn.tsx",
    element: "InboxGroup",
    why: "The column's own heading names the group.",
  },
  {
    where: "routes/agents/SeatPeek.tsx",
    element: "Meter",
    why: "The term beside it (`dt`) names the budget.",
  },
  {
    where: "routes/chatlet/DirectMessages.tsx",
    element: "InboxGroup",
    why: "The list has one group, named by the page.",
  },
  {
    where: "routes/chatlet/Dialogs.tsx",
    element: "FormField",
    why: "The dialog's title names the one field it asks for.",
  },
  {
    where: "routes/atlas/Review.tsx",
    element: "Checkbox",
    why: "A card's pick: the card's own head names what is picked.",
  },
  {
    where: "routes/spend/ByAgent.tsx",
    element: "Meter",
    why: "The Today column head and the row's seat name it.",
  },
  {
    where: "routes/spend/Budgets.tsx",
    element: "Meter",
    why: "The tile or row heads it with whose budget it is and the figure.",
  },
  {
    where: "routes/settings/mcp-tools/server/ToolsTab.tsx",
    element: "FilterChipGroup",
    why: "The chips name the kinds they show.",
  },
  {
    where: "routes/settings/mcp-tools/wizard/steps.tsx",
    element: "KeyValueRows",
    why: "The column heads name the list.",
  },
  {
    where: "routes/settings/mcp-tools/builtin/Catalogue.tsx",
    element: "FilterChipGroup",
    why: "The chips name the capabilities they show.",
  },
  {
    where: "routes/settings/FileStorage.tsx",
    element: "Meter",
    why: "The tile's label and figure head the bar.",
  },
  {
    where: "routes/settings/principles/edit/PolicyListField.tsx",
    element: "ListInput",
    why: "The card's title is the list's name, right above it.",
  },
  {
    where: "routes/settings/principles/edit/PartEditor.tsx",
    element: "FormField",
    why: "The card's title is the field's name, right above it.",
  },
  {
    where: "routes/gitlet/change/Viewed.tsx",
    element: "Meter",
    why: "The words beside the bar say how many files are viewed of how many.",
  },
  {
    where: "routes/advisor/Run.tsx",
    element: "Meter",
    why: "The run line's note above the bar says how many questions are answered of how many.",
  },
];

/** The props that keep a name and draw no words. */
const HIDING = new Set(["hideLabel", "labelHidden"]);

/** A condition that reads the room a control has, which is never a reason to drop its words. */
const BY_WIDTH = /narrow|tight|compact|wide|width|phone|mobile|small/i;

interface Found {
  element: string;
  line: number;
  /** The hiding depends on a value read at run time, spelled as it is in the source. */
  condition: string | null;
}

/** Every element in `text` that hides its label. */
export function hiddenLabels(text: string): Found[] {
  const at = lineOf(text);
  const out: Found[] = [];
  walk(parse(text, "tsx"), (node) => {
    if (node.type !== "JSXOpeningElement") return;
    for (const attr of node.attributes as Node[]) {
      const name = (attr.name as (Node & { name?: string }) | undefined)?.name;
      if (attr.type !== "JSXAttribute" || !name || !HIDING.has(name)) continue;
      const value = attr.value as Node | null;
      const expr = value?.type === "JSXExpressionContainer" ? (value.expression as Node) : null;
      if (expr?.type === "Literal" && expr.value === false) continue;
      out.push({
        element: String((node.name as Node & { name?: string }).name),
        line: at(node.start),
        condition: expr && expr.type !== "Literal" ? text.slice(expr.start, expr.end) : null,
      });
    }
  });
  return out;
}

const read = modules().filter((m) => m.lang === "tsx");
const found = read.flatMap((m) => hiddenLabels(m.text).map((f) => ({ ...f, where: m.path })));
const declared = (f: { where: string; element: string }) =>
  DECLARED.some((d) => d.where === f.where && d.element === f.element);

test("every hidden label says where its words are drawn instead", () => {
  expect(read.length, "the gate read too few of the dashboard's modules").toBeGreaterThan(200);
  const unsaid = found.filter((f) => !declared(f)).map((f) => `${f.where}:${f.line} ${f.element}`);
  expect(unsaid, "draw the words, or declare where they are drawn instead").toEqual([]);
});

test("no label is hidden to make room", () => {
  const squeezed = found
    .filter((f) => f.condition !== null && BY_WIDTH.test(f.condition))
    .map((f) => `${f.where}:${f.line} ${f.element} (${f.condition})`);
  expect(
    squeezed,
    "a control too wide for its place needs a shorter word or a smaller control",
  ).toEqual([]);
});

test("an excuse cannot outlive the label it excuses", () => {
  const stale = DECLARED.filter(
    (d) => !found.some((f) => f.where === d.where && f.element === d.element),
  ).map((d) => `${d.where} ${d.element}`);
  expect(stale, "these no longer hide a label; drop the entry").toEqual([]);
});

test("the scan sees a hidden label, its condition, and a label drawn", () => {
  expect(hiddenLabels(`const a = <Switch label="Unread" labelHidden />;`)).toEqual([
    { element: "Switch", line: 1, condition: null },
  ]);
  expect(hiddenLabels(`const a = <Switch label="Unread"\n  labelHidden={narrow} />;`)).toEqual([
    { element: "Switch", line: 1, condition: "narrow" },
  ]);
  expect(hiddenLabels(`const a = <Meter label="Budget" hideLabel={false} />;`)).toEqual([]);
  expect(hiddenLabels(`const a = <Meter label="Budget" />;`)).toEqual([]);
  expect(BY_WIDTH.test("narrow")).toBe(true);
  expect(BY_WIDTH.test("!alone")).toBe(false);
});
