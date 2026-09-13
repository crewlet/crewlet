// @vitest-environment node
/**
 * Keys, the draft and the document, in both directions.
 *
 * What these protect: a node's key is the engine's identity and survives a
 * reload of the same document; the draft keeps every key it does not model;
 * the path index names exactly the document it was built beside; and a merge
 * patch names only what changed, removing a key with an explicit `null`
 * because a merge patch keeps every key it does not name.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { REDACTED } from "~/lib/format.ts";
import { cloneJson, getPath, jsonEqual, setPath } from "./json.ts";
import {
  COMPANY_KEY,
  handleOfKey,
  isMintedKey,
  isNodeKey,
  mintKey,
  seatKey,
  seatPathKey,
  unitKey,
  unitPathKey,
} from "./keys.ts";
import { allSeats, allUnits, locate } from "./draft.ts";
import {
  buildPatch,
  fromDocument,
  handlesByKey,
  maskedCredentialPaths,
  pathOfSegments,
  suggestUniqueName,
  toDocument,
} from "./document.ts";
import { countingKeys, fixtureCompany, fixtureDerived } from "./testkit.ts";

describe("keys", () => {
  test("an existing node is keyed by the engine's identity, a created one by a minted token", () => {
    expect(seatKey("dev")).toBe("seat:dev");
    expect(unitKey("Engineering")).toBe("unit:Engineering");
    expect(handleOfKey("seat:dev")).toBe("dev");
    expect(handleOfKey(unitKey("dev"))).toBeUndefined();
    expect(handleOfKey(seatPathKey("roles[0]"))).toBeUndefined();
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
    for (const good of [
      COMPANY_KEY,
      "seat:a",
      "unit:A b",
      "seat@roles[0]",
      "unit@units[1]",
      "new:abc_-1",
    ]) {
      expect(isNodeKey(good), good).toBe(true);
    }
    for (const bad of [
      "",
      "seat:",
      "unit:",
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
  test("keys seats by the handle the engine derived, and units by name", () => {
    const doc = fixtureCompany();
    const draft = fromDocument(doc, fixtureDerived(doc));
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:ceo", "seat:designer"]);
    expect([...allUnits(draft)].map(({ unit }) => unit.key)).toEqual([
      "unit:Engineering",
      "unit:Platform",
      "unit:Sales",
    ]);
    expect([...allSeats(draft)].map(({ seat }) => seat.key)).toContain("seat:vp-engineering");
  });

  test("the same document keys identically every time it is read", () => {
    const doc = fixtureCompany();
    const a = fromDocument(doc, fixtureDerived(doc));
    const b = fromDocument(cloneJson(doc), fixtureDerived(doc));
    expect(b).toEqual(a);
  });

  test("without a derivation a seat is keyed by its declared handle, or else by its path", () => {
    const doc: CompanyDocument = {
      name: "X",
      roles: [{ name: "A", handle: "alpha" }, { name: "B" }],
    };
    const draft = fromDocument(doc, null);
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:alpha", seatPathKey("roles[1]")]);
  });

  test("a repeated unit name or handle is keyed by path, so neither twin silently wins the identity", () => {
    const doc: CompanyDocument = {
      name: "X",
      roles: [
        { name: "A", handle: "same" },
        { name: "B", handle: "same" },
      ],
      units: [
        { name: "Platform" },
        { name: "Ops", children: [{ name: "Platform" }] },
        { name: "" },
      ],
    };
    const draft = fromDocument(doc, null);
    expect(draft.roles.map((s) => s.key)).toEqual([
      seatPathKey("roles[0]"),
      seatPathKey("roles[1]"),
    ]);
    expect([...allUnits(draft)].map(({ unit }) => unit.key)).toEqual([
      unitPathKey("units[0]"),
      unitKey("Ops"),
      unitPathKey("units[1].children[0]"),
      unitPathKey("units[2]"),
    ]);
  });

  test("every key the builder does not model survives into the draft and back out", () => {
    const doc = fixtureCompany();
    const out = toDocument(fromDocument(doc, fixtureDerived(doc))).document;
    expect(out).toEqual(doc);
    expect(out.future_setting).toEqual({ kept: true });
  });

  test("a null document is the empty draft of create mode", () => {
    expect(fromDocument(null, null)).toEqual({ company: {}, roles: [], units: [] });
  });
});

describe("toDocument", () => {
  test("indexes every node at the path the engine reports a problem at, both ways", () => {
    const doc = fixtureCompany();
    const { index } = toDocument(fromDocument(doc, fixtureDerived(doc)));
    expect(index.byPath.get("")).toBe(COMPANY_KEY);
    expect(index.byPath.get("roles[1]")).toBe("seat:designer");
    expect(index.byPath.get("units[0].children[0]")).toBe("unit:Platform");
    expect(index.byPath.get("units[0].children[0].roles[0]")).toBe("seat:sre");
    expect(index.pathOf.get("seat:dev")).toBe("units[0].roles[1]");
    expect(index.segmentsOf.get("seat:dev")).toEqual(["units", 0, "roles", 1]);
    for (const [key, segments] of index.segmentsOf)
      expect(pathOfSegments(segments)).toBe(index.pathOf.get(key));
  });

  test("an empty list is left out, as the engine writes a document", () => {
    const draft = fromDocument({ name: "X", units: [{ name: "U" }] }, null);
    expect(toDocument(draft).document).toEqual({ name: "X", units: [{ name: "U" }] });
  });
});

describe("handlesByKey", () => {
  test("maps each derived seat to the node at its path in the sent document", () => {
    const doc = fixtureCompany();
    const sent = toDocument(fromDocument(doc, null));
    const handles = handlesByKey(sent, fixtureDerived(doc));
    expect(handles.get(seatPathKey("roles[0]")) ?? handles.get("seat:ceo")).toBe("ceo");
    expect([...handles.values()]).toContain("account-executive");
    expect(handlesByKey(sent, null).size).toBe(0);
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

describe("suggestUniqueName", () => {
  test("keeps a free name and numbers a taken one past every number in use", () => {
    expect(suggestUniqueName(["A"], " Software Engineer ")).toBe("Software Engineer");
    expect(suggestUniqueName(["Software Engineer"], "Software Engineer")).toBe(
      "Software Engineer 2",
    );
    expect(
      suggestUniqueName(["Software Engineer", "Software Engineer 2"], "Software Engineer"),
    ).toBe("Software Engineer 3");
    expect(suggestUniqueName(["Team 2"], "Team 2")).toBe("Team 3");
  });
});

describe("maskedCredentialPaths", () => {
  test("lists the renamed unit's own masked credentials by path, never a value, and nothing a seat inside restores by handle", () => {
    const doc = fixtureCompany();
    const engineering = doc.units![0]!;
    engineering.mcp_env = { tracker: { TOKEN: REDACTED, URL: "${TRACKER_URL}" } };
    engineering.roles![1]!.mcp_env = { tracker: { TOKEN: REDACTED } };
    const draft = fromDocument(doc, fixtureDerived(doc));
    expect(maskedCredentialPaths(draft, "unit:Engineering")).toEqual([
      "units[0].mcp_env.tracker.TOKEN",
    ]);
    expect(maskedCredentialPaths(draft, "unit:Sales")).toEqual([]);
    expect(maskedCredentialPaths(draft, "seat:dev")).toEqual([]);
  });
});

describe("locate", () => {
  test("finds a node anywhere with the list it sits in", () => {
    const doc = fixtureCompany();
    const draft = fromDocument(doc, fixtureDerived(doc));
    const found = locate(draft, "seat:sre");
    expect(found?.kind).toBe("seat");
    expect(found?.parent).toBe("unit:Platform");
    expect(found?.index).toBe(0);
    expect(locate(draft, "seat:nobody")).toBeUndefined();
  });
});
