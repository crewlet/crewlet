// @vitest-environment node
/**
 * The company templates.
 *
 * What these protect: each template's reporting chart has exactly one root;
 * a template never writes a contact identity the operator did not type; names
 * stay unique when the operator's own seat takes a template title; and every
 * node carries a minted key.
 *
 * ONE ROOT WITHOUT A COPY OF THE ENGINE. The dashboard does not derive managers,
 * so the test asserts the three properties of a template's document that make
 * the engine's derivation of it plain: no `manages` entry names a unit (so no
 * entry expands), every unit's only direct member is its own lead (so a lead's
 * automatic management adds nobody), and every seat but one is listed by name
 * in exactly one `manages`. Under those, a seat's primary manager is the one
 * seat listing it, and the forest built from that has one root.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument, ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { allSeats, allUnits, EMPTY_DRAFT } from "./draft.ts";
import { toDocument } from "./document.ts";
import { isMintedKey } from "./keys.ts";
import { apply, record } from "./operations.ts";
import { reportingForest } from "./reporting.ts";
import { seatsNeedingContact, templateIntent, type TemplateOptions } from "./templates.ts";
import { countingKeys, fixtureDerived, fixtureHandle } from "./testkit.ts";

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
  return { intent: built.intent, draft, doc: toDocument(draft).document };
}

function seatsOf(doc: CompanyDocument): ConfigRole[] {
  const out = [...(doc.roles ?? [])];
  const walk = (units: readonly ConfigUnit[] | undefined) => {
    for (const u of units ?? []) {
      out.push(...(u.roles ?? []));
      walk(u.children);
    }
  };
  walk(doc.units);
  return out;
}

describe("templates", () => {
  for (const options of CASES) {
    test(`${options.label}: one reporting root`, () => {
      const { draft, doc } = build(options);
      const seats = seatsOf(doc);
      const unitNames = new Set([...allUnits(draft)].map(({ unit }) => unit.data.name));

      for (const seat of seats) {
        for (const entry of seat.manages ?? [])
          expect(unitNames.has(entry), `${seat.name} manages the unit ${entry}`).toBe(false);
      }
      for (const { unit } of allUnits(draft)) {
        expect(
          unit.roles.map((r) => r.data.name),
          unit.data.name,
        ).toEqual([unit.data.lead]);
      }
      const listers = new Map<string, string[]>();
      for (const seat of seats)
        for (const entry of seat.manages ?? [])
          listers.set(entry, [...(listers.get(entry) ?? []), seat.name]);
      const unlisted = seats.filter((s) => !listers.has(s.name));
      expect(unlisted).toHaveLength(seats.length === 0 ? 0 : 1);
      for (const [name, by] of listers) expect(by, `${name} is listed by`).toHaveLength(1);

      // The forest the engine's derivation of this shape gives.
      const handleOf = new Map(seats.map((s) => [s.name, fixtureHandle(s)]));
      const base = fixtureDerived(doc);
      const derived = {
        ...base,
        seats: (base.seats ?? []).map((s) => ({
          ...s,
          manager: handleOf.get(listers.get(s.name)?.[0] ?? "") ?? "",
        })),
      };
      const forest = reportingForest(derived);
      expect(forest.roots).toHaveLength(seats.length === 0 ? 0 : 1);
      expect(forest.cycles).toEqual([]);
    });

    test(`${options.label}: never invents a contact identity, and mints every key`, () => {
      const { intent, draft, doc } = build(options);
      for (const seat of seatsOf(doc)) {
        if (options.founder && seat.name === options.founder.name) {
          expect(seat.contact).toEqual({ [options.founder.identity]: options.founder.value });
        } else {
          expect(seat.contact, seat.name).toBeUndefined();
        }
      }
      const keys = [...allSeats(draft)]
        .map(({ seat }) => seat.key)
        .concat([...allUnits(draft)].map(({ unit }) => unit.key));
      expect(keys.every(isMintedKey)).toBe(true);
      expect(new Set(keys).size).toBe(keys.length);
      expect(intent.type === "applyTemplate" && intent.charter.name).toBe("Acme");
    });
  }

  test("the shapes are the ones the create form describes", () => {
    const established = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "people",
    }).doc;
    expect(established.units!.map((u) => [u.name, (u.children ?? []).map((c) => c.name)])).toEqual([
      ["Engineering", ["Reliability"]],
      ["Product", ["Design"]],
      ["Go to Market", []],
    ]);
    expect(
      seatsOf(established)
        .filter((s) => s.kind === "human")
        .map((s) => s.name),
    ).toEqual([
      "Engineering Lead",
      "Reliability Lead",
      "Product Lead",
      "Design Lead",
      "Go to Market Lead",
    ]);
    const fresh = build({
      template: "new_company",
      charter: { name: "Acme" },
      founder: FOUNDER,
    }).doc;
    expect(fresh.roles!.map((r) => [r.name, r.manages])).toEqual([
      ["Alex Rivera", ["Chief Executive"]],
      ["Chief Executive", ["Software Engineer", "Product Manager", "Content Strategist"]],
    ]);
    expect(fresh.units!.map((u) => u.name)).toEqual(["Engineering", "Product", "Marketing"]);
  });

  test("the operator's seat keeps its name and a template seat it collides with takes the next free one", () => {
    const { doc } = build({
      template: "new_company",
      charter: { name: "Acme" },
      founder: { name: "Chief Executive", identity: "github_login", value: "alex" },
    });
    expect(doc.roles!.map((r) => [r.name, r.manages])).toEqual([
      ["Chief Executive", ["Chief Executive 2"]],
      ["Chief Executive 2", ["Software Engineer", "Product Manager", "Content Strategist"]],
    ]);
  });

  test("a create form missing what a template needs is refused with what to do", () => {
    const keys = countingKeys();
    expect(templateIntent({ template: "new_company", charter: { name: " " } }, keys)).toMatchObject(
      { ok: false },
    );
    expect(
      templateIntent(
        { template: "empty", charter: { name: "Acme" }, founder: { ...FOUNDER, value: " " } },
        keys,
      ),
    ).toMatchObject({ ok: false, message: expect.stringContaining("contact identity") });
    expect(
      templateIntent({ template: "established_company", charter: { name: "Acme" } }, keys),
    ).toMatchObject({
      ok: false,
      message: expect.stringContaining("people or agents"),
    });
  });

  test("the human seats that still need an identity are listed for the checklist", () => {
    const { draft } = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "people",
      founder: FOUNDER,
    });
    const needing = seatsNeedingContact(draft).map(
      (key) => [...allSeats(draft)].find(({ seat }) => seat.key === key)!.seat.data.name,
    );
    expect(needing).toEqual([
      "Engineering Lead",
      "Reliability Lead",
      "Product Lead",
      "Design Lead",
      "Go to Market Lead",
    ]);
    expect(
      seatsNeedingContact(build({ template: "new_company", charter: { name: "Acme" } }).draft),
    ).toEqual([]);
  });
});
