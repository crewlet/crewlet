/**
 * EVERY PAGE ACTION'S ARGUMENTS ARE WHAT ITS OWN PARSER TAKES.
 *
 * A screen declares what each of its page actions takes (`args`), the model
 * reads it, and the engine holds every proposal to it before a card is drawn
 * (`internal/assistant/wire.ActionArgs`). A declaration the parser disagrees
 * with is worse than none: the engine would card a call the screen refuses,
 * or refuse one it takes. So each declaration is built from the declaration
 * its parser reads, and this suite feeds what each one says (its examples,
 * and every value it lists) through the REAL parser:
 *
 *  - a URL action's keys ([urlArgs]): every example and every listed value of
 *    every key parses, and the arguments name exactly the keys the parser reads;
 *  - the Budgets ceiling ([ceilingPrefillArgs]): every seat and window it lists
 *    parses, and a member it does not declare is refused by the parser too;
 *  - the org builder's operations ([orgOpsArgs]): its example applies to a real
 *    draft, and every operation it declares is one the builder makes;
 *  - an open draft ([draftSetArgs]): its example sets the draft's own field.
 *
 * THE ENGINE READS WHAT THE DASHBOARD EMITS. The same declarations are written
 * to `internal/assistant/wire/testdata/action_args.json`, compared here, and
 * decoded by the engine's own strict reader in Go
 * (`TestTheEngineReadsEverySchemaTheDashboardDeclares`), so a keyword the
 * engine does not read, or an example its checker refuses, fails one side or
 * the other. `make assistant-fixtures` rewrites the file.
 */

// @vitest-environment node

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import type { AssistantActionArgs, AssistantPageDraft } from "~/contract/assistant.ts";
import {
  spendGroupKeys,
  spendRangeKeys,
  ceilingPrefillArgs,
  parseCeilingPrefill,
} from "~/routes/spend/assistantActions.ts";
import { spendOffer } from "~/routes/spend/model.ts";
import { workKeys } from "~/routes/worklet/assistantActions.ts";
import { orgOpsArgs, parseOrgOps } from "~/routes/org/builder/assistantActions.ts";
import { builderReducer, INITIAL_BUILDER } from "~/routes/org/builder/model/reducer.ts";
import { countingKeys, fixtureCompany } from "~/routes/org/builder/model/testkit.ts";
import { draftSetArgs, parseDraftChange, refuse, type AssistantDraftSpec } from "./draft.ts";
import { choiceKey, parseUrlPatch, urlArgs, type UrlActionKey } from "./urlAction.ts";

const NOW = Date.parse("2026-10-26T12:00:00Z");

/** Each URL action's keys, built by the builders the screens use. */
const URL_KEYS: Record<string, Readonly<Record<string, UrlActionKey>>> = {
  "spend.range.set": spendRangeKeys(spendOffer("UTC"), () => NOW),
  "spend.group.set": spendGroupKeys(),
  "work.filter.apply": workKeys("chip", { seatName: (h: string) => h }),
  "work.arrange.set": workKeys("arrangement", { seatName: (h: string) => h }),
  "home.feed.set": {
    feed: choiceKey(
      "Feed",
      [
        { value: "all", label: "All activity" },
        { value: "runs", label: "Coding runs" },
      ],
      "filter",
      "all",
    ),
  },
};

/** Every value a member lists, as a patch of that member alone. */
function listed(args: AssistantActionArgs): Record<string, unknown>[] {
  const out: Record<string, unknown>[] = [];
  for (const [name, member] of Object.entries(args.properties ?? {})) {
    for (const value of member.enum ?? []) out.push({ [name]: value });
  }
  return out;
}

describe("a URL action's arguments are its keys", () => {
  for (const [id, keys] of Object.entries(URL_KEYS)) {
    test(id, () => {
      const args = urlArgs(keys);
      const declared = [
        ...Object.keys(args.properties ?? {}),
        ...Object.keys(args.patternProperties ?? {}),
      ];
      expect(declared.length).toBe(Object.keys(keys).length);
      for (const name of Object.keys(keys).filter((k) => !k.endsWith("."))) {
        expect(args.properties?.[name], `${name} is not declared`).toBeDefined();
      }
      for (const raw of [...(args.examples ?? []), ...listed(args)]) {
        const parsed = parseUrlPatch(raw, keys);
        expect(parsed.ok, `${JSON.stringify(raw)}: ${parsed.ok ? "" : parsed.reason}`).toBe(true);
      }
      // A KEY THE ARGUMENTS DO NOT NAME IS ONE THE PARSER REFUSES.
      expect(parseUrlPatch({ not_a_key: "x" }, keys).ok).toBe(false);
    });
  }

  test("a family is declared by the pattern its members are named by", () => {
    const keys = workKeys("chip", { seatName: (h: string) => h });
    const family = Object.keys(urlArgs(keys).patternProperties ?? {});
    expect(family).toHaveLength(Object.keys(keys).filter((k) => k.endsWith(".")).length);
    for (const pattern of family) {
      const re = new RegExp(pattern);
      expect(re.test("f.priority")).toBe(true);
      expect(parseUrlPatch({ "f.priority": "high" }, keys).ok).toBe(true);
      expect(re.test("f.")).toBe(false);
    }
  });

  test("an action that takes nothing says so", () => {
    expect(urlArgs({}, true)).toMatchObject({ type: ["object", "null"], examples: [{}] });
  });
});

describe("the Budgets ceiling's arguments are what its parser takes", () => {
  const seats = new Set(["qa", "engineer"]);
  const args = ceilingPrefillArgs(seats);

  test("every example, every seat and every window parses", () => {
    const raws: unknown[] = [...(args.examples ?? [])];
    for (const seat of args.properties?.seat?.enum ?? []) {
      for (const period of args.properties?.period?.enum ?? []) {
        raws.push({ seat, period, ceiling: "2M" });
      }
    }
    expect(raws.length).toBeGreaterThan(6);
    for (const raw of raws) {
      const parsed = parseCeilingPrefill(raw, seats);
      expect(parsed.ok, `${JSON.stringify(raw)}: ${parsed.ok ? "" : parsed.reason}`).toBe(true);
    }
  });

  test("the guesses the QA run made are refused by both", () => {
    expect(args.additionalProperties).toBe(false);
    for (const guess of [
      { handle: "qa", limit: 1_500_000, window: "day" },
      { scope: "qa", value: "1.5M", window: "day" },
    ]) {
      expect(Object.keys(guess).some((k) => !(k in (args.properties ?? {})))).toBe(true);
      expect(parseCeilingPrefill(guess, seats).ok).toBe(false);
    }
  });

  test("a screen with no seats offers the company alone", () => {
    const none = ceilingPrefillArgs(new Set());
    expect(none.properties?.seat).toBeUndefined();
    for (const raw of none.examples ?? [])
      expect(parseCeilingPrefill(raw, new Set()).ok).toBe(true);
  });
});

describe("the org builder's operations are the ones it makes", () => {
  const state = builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: fixtureCompany(),
    revision: "rev-1",
  });
  const args = orgOpsArgs(state.draft);

  test("its example applies to the draft it was written against", () => {
    for (const raw of args.examples ?? []) {
      const parsed = parseOrgOps(raw, state, countingKeys());
      expect(parsed.ok, parsed.ok ? "" : parsed.reason).toBe(true);
    }
  });

  test("every operation it declares is one the builder reads", () => {
    const variants = args.properties?.ops?.items?.oneOf ?? [];
    expect(variants.length).toBe(7);
    for (const variant of variants) {
      const op = variant.properties?.op?.const;
      const parsed = parseOrgOps({ ops: [{ op }] }, state, countingKeys());
      expect(parsed.ok ? "" : parsed.reason).not.toMatch(/is not a change the builder makes/);
    }
    const unknown = parseOrgOps({ ops: [{ op: "promote" }] }, state, countingKeys());
    expect(unknown.ok ? "" : unknown.reason).toMatch(/is not a change the builder makes/);
  });
});

/** A draft with one text field the assistant may set, held to a rule. */
const SPEC: AssistantDraftSpec<string> = {
  kind: "client",
  id: "pm",
  step: "day",
  scope: "PM, daily token ceiling (not saved)",
  value: "2M",
  fields: [
    {
      path: "ceiling",
      label: "PM · Daily token ceiling",
      hint: "Type 40M or 2.5M; empty means no limit.",
      read: (t) => t,
      write: (_, next) => (typeof next === "string" && next !== "lots" ? next : refuse("No.")),
    },
  ],
  apply: () => {},
};

const DRAFT: AssistantPageDraft = {
  kind: "client",
  id: "pm",
  step: "day",
  fields: [
    {
      path: "ceiling",
      label: "PM · Daily token ceiling",
      kind: "text",
      value: "2M",
      hint: "Type 40M or 2.5M; empty means no limit.",
      editable: true,
    },
  ],
  more: 0,
};

describe("an open draft's arguments are its editable fields", () => {
  test("its example sets the draft's own field", () => {
    const args = draftSetArgs(DRAFT);
    expect(Object.keys(args.properties?.values?.properties ?? {})).toEqual(["ceiling"]);
    for (const raw of args.examples ?? []) {
      const parsed = parseDraftChange(SPEC, raw);
      expect(parsed.ok, parsed.ok ? "" : parsed.reason).toBe(true);
    }
  });
});

/** Where the engine reads the dashboard's declarations from. */
const FIXTURE = join(
  process.cwd(),
  "..",
  "internal",
  "assistant",
  "wire",
  "testdata",
  "action_args.json",
);

test("the engine's fixture is every declaration as the dashboard emits it", () => {
  const emitted: Record<string, AssistantActionArgs> = {
    ...Object.fromEntries(Object.entries(URL_KEYS).map(([id, keys]) => [id, urlArgs(keys)])),
    "work.filters.clear": urlArgs({}, true),
    "budgets.ceiling.prefill": ceilingPrefillArgs(new Set(["qa", "engineer"])),
    "org.draft.apply_ops": orgOpsArgs(
      builderReducer(INITIAL_BUILDER, {
        type: "load",
        mode: "edit",
        document: fixtureCompany(),
        revision: "rev-1",
      }).draft,
    ),
    "draft.set": draftSetArgs(DRAFT),
  };
  const text = `${JSON.stringify(emitted, null, 2)}\n`;
  if (process.env.CREWLET_REGENERATE_ASSISTANT_FIXTURES === "1") writeFileSync(FIXTURE, text);
  expect(readFileSync(FIXTURE, "utf8"), "run `make assistant-fixtures`").toBe(text);
});
