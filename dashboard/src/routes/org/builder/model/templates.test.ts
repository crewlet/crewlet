// @vitest-environment node
/**
 * The company templates.
 *
 * What these protect: each template's reporting chart has exactly one root;
 * a template never writes a contact identity the operator did not type; names
 * stay unique when the operator's own seat takes a template title; every node
 * has an address, unique among its kind, and every reference names a seat by
 * the handle it was given; and every node carries a minted key.
 *
 * ONE ROOT WITHOUT A COPY OF THE ENGINE. The dashboard does not derive
 * managers, so the test asserts the three properties of a template's chart
 * that make the engine's derivation of it plain: no `manages` entry names a
 * unit (so no entry expands), every unit's only direct member is its own lead
 * (so a lead's automatic management adds nobody), and every seat but one is
 * listed by handle in exactly one `manages`. Under those, a seat's primary
 * manager is the one seat listing it, and the forest built from that has one
 * root.
 */

import { describe, expect, test } from "vitest";
import { allSeats, allUnits, EMPTY_DRAFT } from "./draft.ts";
import { isMintedKey } from "./keys.ts";
import { apply, record } from "./operations.ts";
import { reportingForest } from "./reporting.ts";
import { seatsWithoutContact, templateIntent, type TemplateOptions } from "./templates.ts";
import { chartOfDraft, countingKeys, fixtureDerived } from "./testkit.ts";

const FOUNDER = { name: "Alex Rivera", identity: "slack_user_id", value: "U0FOUNDER" } as const;

const CASES: readonly (TemplateOptions & { label: string })[] = [
  { label: "New company", template: "new_company", charter: { name: "Acme" } },
  {
    label: "New company with your seat",
    template: "new_company",
    charter: { name: "Acme", mission: "Build" },
    founder: FOUNDER,
  },
  {
    label: "Established, leads are agents",
    template: "established_company",
    charter: { name: "Acme" },
    leads: "agents",
  },
  {
    label: "Established, leads are people",
    template: "established_company",
    charter: { name: "Acme" },
    leads: "people",
  },
  {
    label: "Established, leads are people, with your seat",
    template: "established_company",
    charter: { name: "Acme" },
    leads: "people",
    founder: FOUNDER,
  },
  {
    label: "Start empty with your seat",
    template: "empty",
    charter: { name: "Acme" },
    founder: FOUNDER,
  },
];

function build(options: TemplateOptions) {
  const built = templateIntent(options, countingKeys());
  if (!built.ok) throw new Error(built.message);
  const recorded = record(EMPTY_DRAFT, built.intent);
  if (!recorded.ok) throw new Error(recorded.message);
  const draft = apply(EMPTY_DRAFT, recorded.op).draft;
  const seats = [...allSeats(draft)].map(({ seat }) => seat.data);
  return { intent: built.intent, draft, seats };
}

describe("templates", () => {
  for (const options of CASES) {
    test(`${options.label}: one reporting root`, () => {
      const { draft, seats } = build(options);
      const unitKeys = new Set([...allUnits(draft)].map(({ unit }) => unit.data.key));
      const handles = new Set(seats.map((s) => s.handle));

      for (const seat of seats) {
        for (const entry of seat.manages ?? []) {
          expect(unitKeys.has(entry) && !handles.has(entry), `${seat.name} manages a unit`).toBe(
            false,
          );
          expect(handles.has(entry), `${seat.name} manages ${entry}, which is no seat`).toBe(true);
        }
      }
      for (const { unit } of allUnits(draft)) {
        expect(
          unit.roles.map((r) => r.data.handle),
          unit.data.name,
        ).toEqual([unit.data.lead]);
      }
      const listers = new Map<string, string[]>();
      for (const seat of seats)
        for (const entry of seat.manages ?? [])
          listers.set(entry, [...(listers.get(entry) ?? []), seat.handle]);
      const unlisted = seats.filter((s) => !listers.has(s.handle));
      expect(unlisted).toHaveLength(seats.length === 0 ? 0 : 1);
      for (const [handle, by] of listers) expect(by, `${handle} is listed by`).toHaveLength(1);

      // The forest the engine's derivation of this shape gives.
      const chart = chartOfDraft(draft);
      const base = fixtureDerived(chart);
      const derived = {
        ...base,
        seats: (base.seats ?? []).map((s) => ({ ...s, manager: listers.get(s.handle)?.[0] ?? "" })),
      };
      const forest = reportingForest(derived);
      expect(forest.roots).toHaveLength(seats.length === 0 ? 0 : 1);
      expect(forest.cycles).toEqual([]);
    });

    test(`${options.label}: never invents a contact identity, and gives every node a key and an address`, () => {
      const { intent, draft, seats } = build(options);
      for (const seat of seats) {
        if (options.founder && seat.name === options.founder.name) {
          expect(seat.runtime?.contact).toEqual({
            [options.founder.identity]: options.founder.value,
          });
        } else {
          expect(seat.runtime?.contact, seat.name).toBeUndefined();
        }
      }
      const keys = [...allSeats(draft)]
        .map(({ seat }) => seat.key)
        .concat([...allUnits(draft)].map(({ unit }) => unit.key));
      expect(keys.every(isMintedKey)).toBe(true);
      expect(new Set(keys).size).toBe(keys.length);
      const handles = seats.map((s) => s.handle);
      expect(handles.every((h) => /^[a-z0-9-]+$/.test(h))).toBe(true);
      expect(new Set(handles).size).toBe(handles.length);
      const unitKeys = [...allUnits(draft)].map(({ unit }) => unit.data.key);
      expect(new Set(unitKeys).size).toBe(unitKeys.length);
      expect(intent.type === "applyTemplate" && intent.charter.name).toBe("Acme");
    });
  }

  test("the shapes are the ones the create form describes", () => {
    const established = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "people",
    }).draft;
    // The chart serves units by key, so the draft holds them that way.
    expect(established.units.map((u) => [u.data.name, u.children.map((c) => c.data.name)])).toEqual(
      [
        ["Engineering", ["Reliability"]],
        ["Go to Market", []],
        ["Product", ["Design"]],
      ],
    );
    expect(
      [...allSeats(established)]
        .filter(({ seat }) => seat.data.kind === "human")
        .map(({ seat }) => seat.data.name),
    ).toEqual([
      "Engineering Lead",
      "Reliability Lead",
      "Go to Market Lead",
      "Product Lead",
      "Design Lead",
    ]);
    const fresh = build({
      template: "new_company",
      charter: { name: "Acme" },
      founder: FOUNDER,
    }).draft;
    expect(fresh.roles.map((r) => [r.data.handle, r.data.manages])).toEqual([
      ["alex-rivera", ["chief-executive"]],
      ["chief-executive", ["software-engineer", "product-manager", "content-strategist"]],
    ]);
    expect(fresh.units.map((u) => [u.data.key, u.data.lead])).toEqual([
      ["engineering", "software-engineer"],
      ["marketing", "content-strategist"],
      ["product", "product-manager"],
    ]);
  });

  test("the operator's seat keeps its name, and a template seat it collides with takes the next free name and handle", () => {
    const { draft } = build({
      template: "new_company",
      charter: { name: "Acme" },
      founder: { name: "Chief Executive", identity: "github_login", value: "alex" },
    });
    expect(draft.roles.map((r) => [r.data.name, r.data.handle, r.data.manages])).toEqual([
      ["Chief Executive", "chief-executive", ["chief-executive-2"]],
      [
        "Chief Executive 2",
        "chief-executive-2",
        ["software-engineer", "product-manager", "content-strategist"],
      ],
    ]);
  });

  test("a unit whose lead seat took the next free name is led by that seat's handle", () => {
    const { draft } = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "agents",
      founder: { name: "Engineering Lead", identity: "github_login", value: "alex" },
    });
    const engineering = draft.units.find((u) => u.data.key === "engineering")!;
    expect(engineering.roles.map((r) => r.data.name)).toEqual(["Engineering Lead 2"]);
    expect(engineering.data.lead).toBe("engineering-lead-2");
    // The chief executive's reports name the lead as it was finally addressed too.
    expect(draft.roles.find((r) => r.data.name === "Chief Executive")?.data.manages).toContain(
      "engineering-lead-2",
    );
  });

  test("a create form missing what a template needs is refused with what to do", () => {
    const keys = countingKeys();
    expect(templateIntent({ template: "new_company", charter: { name: " " } }, keys)).toMatchObject(
      { ok: false },
    );
    expect(
      templateIntent(
        { template: "empty", charter: { name: "Acme" }, founder: { ...FOUNDER, name: " " } },
        keys,
      ),
    ).toMatchObject({ ok: false, message: expect.stringContaining("name for your seat") });
    expect(
      templateIntent({ template: "established_company", charter: { name: "Acme" } }, keys),
    ).toMatchObject({ ok: false, message: expect.stringContaining("people or agents") });
  });

  // THE ENGINE ADMITS A HUMAN SEAT WITH NO CONTACT, so the form must too: a
  // founder who works only through the dashboard has no chat account to type,
  // and refusing the seat here would refuse a company the engine accepts.
  test("your own seat needs no contact identity, and a blank one writes no runtime block", () => {
    const { seats } = build({
      template: "empty",
      charter: { name: "Acme" },
      founder: { ...FOUNDER, value: "  " },
    });
    const founder = seats.find((s) => s.name === FOUNDER.name)!;
    expect(founder.kind).toBe("human");
    expect(founder).not.toHaveProperty("runtime");
  });

  test("the human seats with no identity are named for the review", () => {
    const { draft } = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "people",
      founder: FOUNDER,
    });
    const without = seatsWithoutContact(draft).map(
      (key) => [...allSeats(draft)].find(({ seat }) => seat.key === key)!.seat.data.name,
    );
    expect(without).toEqual([
      "Engineering Lead",
      "Reliability Lead",
      "Go to Market Lead",
      "Product Lead",
      "Design Lead",
    ]);
    expect(
      seatsWithoutContact(build({ template: "new_company", charter: { name: "Acme" } }).draft),
    ).toEqual([]);
  });
});
