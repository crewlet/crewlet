// @vitest-environment node
/**
 * Keys, the draft and the document, in both directions.
 *
 * What these protect: a node's key is its identity — the handle or unit key
 * its document carries — and survives a reload of the same document; keying
 * writes nothing into the document; the draft keeps every key it does not
 * model;
 * the path index names exactly the document it was built beside; and a merge
 * patch names only what changed, removing a key with an explicit `null`
 * because a merge patch keeps every key it does not name.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { cloneJson, getPath, jsonEqual, setPath } from "./json.ts";
import {
  COMPANY_KEY,
  handleOfKey,
  isMintedKey,
  isNodeKey,
  mintKey,
  seatKey,
  unitIdOfKey,
  unitKey,
} from "./keys.ts";
import { allSeats, allUnits, handleOf, locate, unitIdOf } from "./draft.ts";
import {
  buildPatch,
  fromDocument,
  NO_DERIVATION,
  pathOfSegments,
  placeDerivation,
  rekeying,
  toDocument,
} from "./document.ts";
import { countingKeys, fixtureCompany, fixtureDerived } from "./testkit.ts";

describe("keys", () => {
  test("an existing node is keyed by its identity, a created one by a minted token", () => {
    expect(seatKey("dev")).toBe("seat:dev");
    expect(unitKey("engineering")).toBe("unit:engineering");
    expect(handleOfKey("seat:dev")).toBe("dev");
    expect(handleOfKey(unitKey("dev"))).toBeUndefined();
    expect(unitIdOfKey("unit:engineering")).toBe("engineering");
    expect(unitIdOfKey(seatKey("dev"))).toBeUndefined();
    const key = mintKey(countingKeys("n"));
    expect(key).toBe("new:n1");
    expect(isMintedKey(key)).toBe(true);
    expect(isMintedKey(seatKey("x"))).toBe(false);
  });

  test("a key source that produces an unusable token is refused rather than stored", () => {
    expect(() => mintKey({ next: () => "has space" })).toThrow(RangeError);
    expect(() => mintKey({ next: () => "" })).toThrow(RangeError);
    expect(() => mintKey({ next: () => "x".repeat(65) })).toThrow(RangeError);
  });

  test("only shapes this module produces are keys", () => {
    for (const good of [COMPANY_KEY, "seat:a", "unit:a_b", "new:abc_-1"]) {
      expect(isNodeKey(good), good).toBe(true);
    }
    for (const bad of [
      "",
      "seat:",
      "unit:",
      "seat@roles[0]",
      "new:",
      "new:a b",
      "company2",
      7,
      null,
      undefined,
      {},
    ]) {
      expect(isNodeKey(bad), String(bad)).toBe(false);
    }
  });
});

describe("json", () => {
  test("equality ignores key order and reads an undefined property as absent, but not null", () => {
    expect(jsonEqual({ a: 1, b: [1, { c: 2 }] }, { b: [1, { c: 2 }], a: 1 })).toBe(true);
    expect(jsonEqual({ a: 1, b: undefined }, { a: 1 })).toBe(true);
    expect(jsonEqual({ a: null }, { a: undefined })).toBe(false);
    expect(jsonEqual([1, 2], [2, 1])).toBe(false);
  });

  test("setPath removes and prunes the objects a removal emptied, and shares untouched branches", () => {
    const record = { integrations: { jira: { project: "OPS" } }, keep: { deep: true } };
    const removed = setPath(record, ["integrations", "jira", "project"], undefined);
    expect(removed).toEqual({ keep: { deep: true } });
    expect(removed.keep).toBe(record.keep);
    const added = setPath({}, ["integrations", "github", "tier"], "developer");
    expect(added).toEqual({ integrations: { github: { tier: "developer" } } });
    expect(setPath(record, ["missing", "x"], undefined)).toBe(record);
  });

  test("a clone drops undefined properties and shares nothing with its source", () => {
    const source = { a: { b: [1, { c: undefined, d: 2 }] } };
    const copy = cloneJson(source);
    expect(copy).toEqual({ a: { b: [1, { d: 2 }] } });
    expect(copy.a).not.toBe(source.a);
  });

  test("a key named __proto__ stays a key: it never becomes a prototype nobody sees in the document", () => {
    // What storage hands back: `JSON.parse` makes the key an own property.
    const role = JSON.parse('{"name":"Ops","__proto__":{"kind":"human"}}') as Record<
      string,
      unknown
    >;
    const copy = cloneJson(role);
    expect(Object.getPrototypeOf(copy)).toBe(Object.prototype);
    expect(copy.kind).toBeUndefined();
    expect(JSON.parse(JSON.stringify(copy))).toEqual(role);
    expect(Object.keys(copy)).toEqual(["name", "__proto__"]);

    // Reading and editing go through own properties only.
    expect(getPath({ name: "Ops" }, ["__proto__"])).toBeUndefined();
    expect(getPath(copy, ["__proto__", "kind"])).toBe("human");
    expect(jsonEqual({}, JSON.parse('{"__proto__":{}}'))).toBe(false);
    expect(setPath({ name: "Ops" }, ["__proto__", "x"], undefined)).toEqual({ name: "Ops" });
  });
});

describe("fromDocument", () => {
  test("keys seats by the handle they declare, and units by their key", () => {
    const doc = fixtureCompany();
    const draft = fromDocument(doc);
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:ceo", "seat:designer"]);
    expect([...allUnits(draft)].map(({ unit }) => unit.key)).toEqual([
      "unit:engineering",
      "unit:platform",
      "unit:sales",
    ]);
    expect([...allSeats(draft)].map(({ seat }) => seat.key)).toContain("seat:vp-engineering");
  });

  test("the same document keys identically every time it is read", () => {
    const doc = fixtureCompany();
    expect(fromDocument(cloneJson(doc))).toEqual(fromDocument(doc));
  });

  // EVERY DOCUMENT THE ENGINE STORES DECLARES BOTH. One that does not (a
  // fixture, a hand-written file) is keyed by the builder's own minting rule,
  // so its nodes still have keys, and keying writes nothing into it.
  test("a node that declares no identity is keyed by one minted from its name, and its data is left alone", () => {
    const doc: CompanyDocument = {
      name: "X",
      roles: [{ name: "A", handle: "alpha" }, { name: "B" }],
      units: [{ name: "Legal Team" }],
    };
    const draft = fromDocument(doc);
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:alpha", "seat:b"]);
    expect(draft.units.map((u) => u.key)).toEqual(["unit:legal-team"]);
    expect(handleOf(draft.roles[1]!)).toBe("b");
    expect(unitIdOf(draft.units[0]!)).toBe("legal-team");
    expect(toDocument(draft).document).toEqual(doc);
  });

  test("an identity held twice keys the second node apart, so neither twin silently takes the other's key", () => {
    const doc: CompanyDocument = {
      name: "X",
      roles: [
        { name: "A", handle: "same" },
        { name: "B", handle: "same" },
      ],
      units: [
        { name: "Platform", id: "platform" },
        { name: "Ops", id: "ops", children: [{ name: "Platform", id: "platform" }] },
        { name: "" },
      ],
    };
    const draft = fromDocument(doc);
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:same", "seat:b"]);
    expect([...allUnits(draft)].map(({ unit }) => unit.key)).toEqual([
      "unit:platform",
      "unit:ops",
      "unit:platform-2",
      "unit:unit",
    ]);
  });

  test("every key the builder does not model survives into the draft and back out", () => {
    const doc = fixtureCompany();
    const out = toDocument(fromDocument(doc)).document;
    expect(out).toEqual(doc);
    expect(out.future_setting).toEqual({ kept: true });
  });

  test("a null document is the empty draft of create mode", () => {
    expect(fromDocument(null)).toEqual({ company: {}, roles: [], units: [] });
  });
});

describe("toDocument", () => {
  test("indexes every node at the path the engine reports a problem at, both ways", () => {
    const doc = fixtureCompany();
    const { index } = toDocument(fromDocument(doc));
    expect(index.byPath.get("")).toBe(COMPANY_KEY);
    expect(index.byPath.get("roles[1]")).toBe("seat:designer");
    expect(index.byPath.get("units[0].children[0]")).toBe("unit:platform");
    expect(index.byPath.get("units[0].children[0].roles[0]")).toBe("seat:sre");
    expect(index.pathOf.get("seat:dev")).toBe("units[0].roles[1]");
    expect(index.segmentsOf.get("seat:dev")).toEqual(["units", 0, "roles", 1]);
    for (const [key, segments] of index.segmentsOf)
      expect(pathOfSegments(segments)).toBe(index.pathOf.get(key));
  });

  test("an empty list is left out, as the engine writes a document", () => {
    const draft = fromDocument({ name: "X", units: [{ name: "U" }] });
    expect(toDocument(draft).document).toEqual({ name: "X", units: [{ name: "U" }] });
  });
});

describe("placeDerivation", () => {
  test("places each derived seat and unit on the node at its path in the sent document", () => {
    const doc = fixtureCompany();
    const sent = toDocument(fromDocument(doc));
    const placed = placeDerivation(sent.index, fixtureDerived(doc));
    expect(placed.seatByKey.get("seat:ceo")?.handle).toBe("ceo");
    expect(placed.keyOfHandle.get("ceo")).toBe("seat:ceo");
    expect(placed.unitByKey.get("unit:sales")?.name).toBe("Sales");
    expect(placeDerivation(sent.index, null)).toEqual(NO_DERIVATION);
  });

  // A refused draft can give one handle to two seats; the reading of a
  // reported handle as a node must not change with the order a map is built.
  test("a handle two seats share names the first", () => {
    const doc: CompanyDocument = { name: "X", roles: [{ name: "Dev" }, { name: "dev" }] };
    const sent = toDocument(fromDocument(doc));
    const placed = placeDerivation(sent.index, fixtureDerived(doc));
    expect(placed.keyOfHandle.get("dev")).toBe(sent.index.byPath.get("roles[0]"));
  });
});

describe("rekeying", () => {
  // A SAVE MOVES THE NODES IT CREATED. The draft it sent keys them by the
  // tokens they were minted with; the base it becomes keys them by the
  // identities they were given, at the same paths.
  test("follows each created node from its minted key to the identity at its path", () => {
    const base = fromDocument(fixtureCompany());
    const sent = {
      ...base,
      roles: [...base.roles, { key: "new:a", data: { name: "QA", handle: "qa" } }],
    };
    const saved = fromDocument(toDocument(sent).document);
    const moved = rekeying(sent, saved);
    expect(moved.get("new:a")).toBe(seatKey("qa"));
    // A node that kept its key is not listed.
    expect(moved.has("seat:ceo")).toBe(false);
    expect(rekeying(saved, saved).size).toBe(0);
  });
});

describe("buildPatch", () => {
  const base = fixtureCompany();

  test("an unchanged draft patches nothing", () => {
    expect(buildPatch(base, cloneJson(base))).toEqual({});
  });

  test("names only the changed top-level keys it edits, whole", () => {
    const draft = cloneJson(base);
    draft.mission = "Make better things.";
    draft.roles![0]!.goal = "Lead well";
    draft.providers = { llm: { changed: true } };
    const patch = buildPatch(base, draft);
    expect(Object.keys(patch).sort()).toEqual(["mission", "roles"]);
    expect(patch.roles).toEqual(draft.roles);
  });

  test("a removed charter field or list is an explicit null", () => {
    const draft = cloneJson(base);
    delete draft.mission;
    delete draft.roles;
    expect(buildPatch(base, draft)).toEqual({ mission: null, roles: null });
  });

  test("an empty list and an absent one are the same document", () => {
    expect(buildPatch({ name: "X" }, { name: "X", roles: [] })).toEqual({});
  });

  test("the Datadog fallback is sent as its one value, or null when removed", () => {
    const moved = cloneJson(base);
    (moved.integrations as { datadog: { route_to?: string } }).datadog.route_to = "dev";
    expect(buildPatch(base, moved)).toEqual({ integrations: { datadog: { route_to: "dev" } } });
    const cleared = cloneJson(base);
    delete (cleared.integrations as { datadog: { route_to?: string } }).datadog.route_to;
    expect(buildPatch(base, cleared)).toEqual({ integrations: { datadog: { route_to: null } } });
  });

  test("a removed GitLab access level is null by handle, and the remaining entries are not resent", () => {
    const draft = cloneJson(base);
    const levels = (
      draft.integrations as { gitlab: { provisioning: { access_levels: Record<string, string> } } }
    ).gitlab.provisioning.access_levels;
    delete levels.dev;
    levels["account-executive"] = "developer";
    expect(buildPatch(base, draft)).toEqual({
      integrations: {
        gitlab: {
          provisioning: { access_levels: { dev: null, "account-executive": "developer" } },
        },
      },
    });
  });

  test("with no base, every present edited key is named", () => {
    expect(buildPatch(null, { name: "New", units: [{ name: "U" }], other: 1 })).toEqual({
      name: "New",
      units: [{ name: "U" }],
    });
  });
});

describe("locate", () => {
  test("finds a node anywhere with the list it sits in", () => {
    const doc = fixtureCompany();
    const draft = fromDocument(doc);
    const found = locate(draft, "seat:sre");
    expect(found?.kind).toBe("seat");
    expect(found?.parent).toBe("unit:platform");
    expect(found?.index).toBe(0);
    expect(locate(draft, "seat:nobody")).toBeUndefined();
  });
});
