// @vitest-environment node
/**
 * The company templates.
 *
 * What these protect: each template's reporting chart has exactly one root;
 * a template never writes a contact identity the operator did not type; every
 * seat carries a handle and every unit a key, all different, and every lead
 * and `manages` entry names them, even when the operator's own seat shares a
 * template title; and every node carries a minted key.
 *
 * ONE ROOT WITHOUT A COPY OF THE ENGINE. The dashboard does not derive managers,
 * so the test asserts the three properties of a template's document that make
 * the engine's derivation of it plain: no `manages` entry names a unit (so no
 * entry expands), every unit's only direct member is its own lead (so a lead's
 * automatic management adds nobody), and every seat but one is listed by
 * handle in exactly one `manages`. Under those, a seat's primary manager is
 * the one seat listing it, and the forest built from that has one root.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument, ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { allSeats, allUnits, EMPTY_DRAFT } from "./draft.ts";
import { toDocument } from "./document.ts";
import { isMintedKey } from "./keys.ts";
import { apply, record } from "./operations.ts";
import { reportingForest } from "./reporting.ts";
import { seatsWithoutContact, templateIntent, type TemplateOptions } from "./templates.ts";
import { countingKeys, fixtureDerived } from "./testkit.ts";

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
      const unitKeys = new Set([...allUnits(draft)].map(({ unit }) => unit.data.id));

      for (const seat of seats) {
        for (const entry of seat.manages ?? [])
          expect(unitKeys.has(entry), `${seat.name} manages the unit ${entry}`).toBe(false);
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
          listers.set(entry, [...(listers.get(entry) ?? []), seat.handle!]);
      const unlisted = seats.filter((s) => !listers.has(s.handle!));
      expect(unlisted).toHaveLength(seats.length === 0 ? 0 : 1);
      for (const [handle, by] of listers) expect(by, `${handle} is listed by`).toHaveLength(1);

      // The forest the engine's derivation of this shape gives.
      const base = fixtureDerived(doc);
      const derived = {
        ...base,
        seats: (base.seats ?? []).map((s) => ({
          ...s,
          manager: listers.get(s.handle)?.[0] ?? "",
        })),
      };
      const forest = reportingForest(derived);
      expect(forest.roots).toHaveLength(seats.length === 0 ? 0 : 1);
      expect(forest.cycles).toEqual([]);
    });

    test(`${options.label}: never invents a contact identity, and mints every key and identity`, () => {
      const { intent, draft, doc } = build(options);
      // Every node carries its identity, all different, before any check:
      // what a lead and a manages entry name has to exist from the start.
      const identities = [
        ...seatsOf(doc).map((s) => s.handle),
        ...[...allUnits(draft)].map(({ unit }) => unit.data.id),
      ];
      expect(identities.every((id) => typeof id === "string" && id !== "")).toBe(true);
      expect(new Set(identities).size).toBe(identities.length);
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
    expect(fresh.roles!.map((r) => [r.name, r.handle, r.manages])).toEqual([
      ["Alex Rivera", "alex-rivera", ["chief-executive"]],
      [
        "Chief Executive",
        "chief-executive",
        ["software-engineer", "product-manager", "content-strategist"],
      ],
    ]);
    expect(fresh.units!.map((u) => [u.name, u.id, u.lead])).toEqual([
      ["Engineering", "engineering", "software-engineer"],
      ["Product", "product", "product-manager"],
      ["Marketing", "marketing", "content-strategist"],
    ]);
  });

  // A NAME IS PROSE: the operator's own seat and a template seat may share
  // one. What they may not share is a handle, which every reference names.
  test("the operator's seat may share a template title, and the two seats get different handles", () => {
    const { doc } = build({
      template: "new_company",
      charter: { name: "Acme" },
      founder: { name: "Chief Executive", identity: "github_login", value: "alex" },
    });
    expect(doc.roles!.map((r) => [r.name, r.handle, r.manages])).toEqual([
      ["Chief Executive", "chief-executive", ["chief-executive-2"]],
      [
        "Chief Executive",
        "chief-executive-2",
        ["software-engineer", "product-manager", "content-strategist"],
      ],
    ]);
  });

  test("a unit whose lead shares the operator's name is led by the handle that seat was given", () => {
    const { doc } = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "agents",
      founder: { name: "Engineering Lead", identity: "github_login", value: "alex" },
    });
    const engineering = doc.units!.find((u) => u.name === "Engineering")!;
    expect(engineering.roles!.map((r) => [r.name, r.handle])).toEqual([
      ["Engineering Lead", "engineering-lead-2"],
    ]);
    expect(engineering.lead).toBe("engineering-lead-2");
    expect(doc.roles!.find((r) => r.handle === "chief-executive")?.manages).toContain(
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
    // The operator's contact identity is optional: the engine admits a human
    // seat with none, and the person is then reached through the dashboard.
    const unreached = templateIntent(
      { template: "empty", charter: { name: "Acme" }, founder: { ...FOUNDER, value: " " } },
      keys,
    );
    expect(unreached.ok).toBe(true);
    expect(
      unreached.ok && unreached.intent.type === "applyTemplate" && unreached.intent.roles[0]!.data,
    ).toEqual({ name: "Alex Rivera", handle: "alex-rivera", kind: "human" });
    expect(
      templateIntent({ template: "established_company", charter: { name: "Acme" } }, keys),
    ).toMatchObject({
      ok: false,
      message: expect.stringContaining("people or agents"),
    });
  });

  test("the human seats with no contact identity are listed for the review's notice", () => {
    const { draft } = build({
      template: "established_company",
      charter: { name: "Acme" },
      leads: "people",
      founder: FOUNDER,
    });
    const needing = seatsWithoutContact(draft).map(
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
      seatsWithoutContact(build({ template: "new_company", charter: { name: "Acme" } }).draft),
    ).toEqual([]);
  });
});
