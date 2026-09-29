// @vitest-environment node
/**
 * Keys, the chart and the draft, and the settings a save writes.
 *
 * What these protect: a node's key is its address in the chart and survives a
 * second reading of the same rows; the draft keeps every key it does not
 * model, and holds every list in the order the chart serves it; "the chart
 * has not changed" is judged on the rows alone, never on the position every
 * read moves; a derivation is placed only on the chart it describes; and a
 * merge patch names only what changed, removing a key with an explicit `null`
 * because a merge patch keeps every key it does not name.
 */

import { describe, expect, test } from "vitest";
import type { ChartRead } from "~/protocol/index.ts";
import { cloneJson, getPath, jsonEqual, setPath } from "./json.ts";
import {
  COMPANY_KEY,
  handleOfKey,
  isMintedKey,
  isNodeKey,
  mintKey,
  seatKey,
  unitKey,
  unitKeyOf,
} from "./keys.ts";
import { addressIndex, allSeats, allUnits, compareAddress, locate, sameChart } from "./draft.ts";
import {
  chartPrint,
  describes,
  fingerprint,
  fromChart,
  placeDerivation,
  rekeying,
  settingsPatch,
  slugOf,
  suggestAddress,
  suggestUniqueName,
} from "./document.ts";
import { apply, record } from "./operations.ts";
import {
  chartOf,
  chartOfDraft,
  countingKeys,
  fixtureChart,
  fixtureDerived,
  fixtureSettings,
} from "./testkit.ts";

describe("keys", () => {
  test("an existing node is keyed by its address, a created one by a minted token", () => {
    expect(seatKey("dev")).toBe("seat:dev");
    expect(unitKey("engineering")).toBe("unit:engineering");
    expect(handleOfKey("seat:dev")).toBe("dev");
    expect(handleOfKey(unitKey("dev"))).toBeUndefined();
    expect(unitKeyOf("unit:sales")).toBe("sales");
    expect(unitKeyOf("seat:sales")).toBeUndefined();
    const key = mintKey(countingKeys("n"));
    expect(key).toBe("new:n1");
    expect(isMintedKey(key)).toBe(true);
    expect(isMintedKey(seatKey("x"))).toBe(false);
  });

  test("a key source that produces an unusable token is refused rather than stored", () => {
    expect(() => mintKey({ next: () => "has space" })).toThrow(RangeError);
    expect(() => mintKey({ next: () => "" })).toThrow(RangeError);
    expect(() => mintKey({ next: () => "x".repeat(65) })).toThrow(RangeError);
    // Control: a token of the documented shape mints.
    expect(mintKey({ next: () => "a_b-1" })).toBe("new:a_b-1");
  });

  test("only shapes this module produces are keys", () => {
    for (const good of [COMPANY_KEY, "seat:a", "unit:a-b", "new:abc_-1"]) {
      expect(isNodeKey(good), good).toBe(true);
    }
    for (const bad of [
      "",
      "seat:",
      "unit:",
      "new:",
      "new:a b",
      "company2",
      "seat@roles[0]",
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
    const record = { runtime: { github: { tier: "developer" } }, keep: { deep: true } };
    const removed = setPath(record, ["runtime", "github", "tier"], undefined);
    expect(removed).toEqual({ keep: { deep: true } });
    expect(removed.keep).toBe(record.keep);
    const added = setPath({}, ["runtime", "github", "tier"], "developer");
    expect(added).toEqual({ runtime: { github: { tier: "developer" } } });
    expect(setPath(record, ["missing", "x"], undefined)).toBe(record);
  });

  test("a clone drops undefined properties and shares nothing with its source", () => {
    const source = { a: { b: [1, { c: undefined, d: 2 }] } };
    const copy = cloneJson(source);
    expect(copy).toEqual({ a: { b: [1, { d: 2 }] } });
    expect(copy.a).not.toBe(source.a);
  });

  test("a key named __proto__ stays a key: it never becomes a prototype nobody sees", () => {
    // What storage hands back: `JSON.parse` makes the key an own property.
    const seat = JSON.parse('{"name":"Ops","__proto__":{"kind":"human"}}') as Record<
      string,
      unknown
    >;
    const copy = cloneJson(seat);
    expect(Object.getPrototypeOf(copy)).toBe(Object.prototype);
    expect(copy.kind).toBeUndefined();
    expect(JSON.parse(JSON.stringify(copy))).toEqual(seat);
    expect(Object.keys(copy)).toEqual(["name", "__proto__"]);
    expect(getPath({ name: "Ops" }, ["__proto__"])).toBeUndefined();
    expect(getPath(copy, ["__proto__", "kind"])).toBe("human");
    expect(jsonEqual({}, JSON.parse('{"__proto__":{}}'))).toBe(false);
  });
});

describe("reading the chart", () => {
  test("keys every seat by its handle and every unit by its key, where the chart places it", () => {
    const draft = fromChart(fixtureSettings(), fixtureChart());
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:ceo"]);
    expect(draft.units.map((u) => u.key)).toEqual(["unit:engineering", "unit:sales"]);
    expect(locate(draft, "seat:sre")?.parent).toBe("unit:platform");
    expect(locate(draft, "unit:platform")?.parent).toBe("unit:engineering");
    // The tree holds the placement; the data does not repeat it.
    const sre = locate(draft, "seat:sre");
    expect(sre?.kind === "seat" && sre.node.data).toEqual({
      handle: "sre",
      name: "SRE",
      goal: "Keep it up",
      project: "OPS",
    });
    // `manages` is read beside the seat it belongs to.
    const ceo = locate(draft, "seat:ceo");
    expect(ceo?.kind === "seat" && ceo.node.data.manages).toEqual(["engineering", "designer"]);
  });

  test("every list is in the order the chart serves it, however the answer listed its rows", () => {
    const chart = fixtureChart();
    const shuffled: ChartRead = {
      ...chart,
      seats: [...chart.seats].reverse(),
      units: [...chart.units].reverse(),
    };
    const a = fromChart(null, chart);
    const b = fromChart(null, shuffled);
    expect(b).toEqual(a);
    expect(sameChart(a, b)).toBe(true);
    const engineering = locate(a, "unit:engineering");
    expect(
      engineering?.kind === "unit" && engineering.node.roles.map((s) => s.data.handle),
    ).toEqual(["dev", "vp-engineering"]);
    // The byte order of the addresses, not the UTF-16 order of a plain `<`.
    expect(compareAddress("\u{1F600}", "￿")).toBe(1);
    expect("\u{1F600}" < "￿").toBe(true);
  });

  test("every key the builder does not model survives, and an empty value reads as absent", () => {
    const draft = fromChart(
      null,
      chartOf({
        seats: [
          {
            handle: "x",
            name: "X",
            goal: "",
            responsibilities: [],
            kind: "agent",
            future_field: { kept: true },
            runtime: { future_runtime_key: 1 },
          } as never,
        ],
      }),
    );
    expect(draft.roles[0]!.data).toEqual({
      handle: "x",
      name: "X",
      future_field: { kept: true },
      runtime: { future_runtime_key: 1 },
    });
  });

  test("a unit whose parent the reading does not hold is drawn at the root rather than lost", () => {
    const draft = fromChart(
      null,
      chartOf({
        units: [{ key: "orphan", name: "Orphan", parent: "gone" }],
        seats: [{ handle: "s", name: "S", unit: "gone" }],
      }),
    );
    expect(draft.units.map((u) => u.key)).toEqual(["unit:orphan"]);
    expect(draft.roles.map((s) => s.key)).toEqual(["seat:s"]);
  });

  test("the settings a stored revision still carries a chart in are read without it", () => {
    const draft = fromChart({ name: "Acme", roles: [{ name: "Old" }], units: [] } as never, null);
    expect(draft.company).toEqual({ name: "Acme" });
    expect(draft.roles).toEqual([]);
  });
});

describe("the chart's print", () => {
  test("is the rows alone: the position every read moves is not part of it", () => {
    const chart = fixtureChart();
    const later: ChartRead = {
      ...chart,
      answer: { level: "linearizable", position: "CREWLET_CHART_LOG@1:99" },
    };
    expect(chartPrint(later)).toBe(chartPrint(chart));
    expect(fingerprint(chartPrint(later))).toBe(fingerprint(chartPrint(chart)));
    // Control: a changed row is a different print.
    const changed: ChartRead = {
      ...chart,
      seats: chart.seats.map((s) => (s.handle === "dev" ? { ...s, goal: "Ship" } : s)),
    };
    expect(fingerprint(chartPrint(changed))).not.toBe(fingerprint(chartPrint(chart)));
    // Key order and row order are not changes either.
    const reordered: ChartRead = { ...chart, seats: [...chart.seats].reverse() };
    expect(chartPrint(reordered)).toBe(chartPrint(chart));
  });
});

describe("the engine's derivation", () => {
  test("is placed on the node holding each handle and each unit key", () => {
    const chart = fixtureChart();
    const draft = fromChart(null, chart);
    const derived = fixtureDerived(chart, { seats: { dev: { manager: "vp-engineering" } } });
    const placed = placeDerivation(draft, derived);
    expect(placed.seatByKey.get("seat:dev")?.manager).toBe("vp-engineering");
    expect(placed.unitByKey.get("unit:platform")?.name).toBe("Platform");
    expect(placed.keyOfHandle.get("sre")).toBe("seat:sre");
  });

  test("describes a chart only when it holds the same seats, units and declared leads", () => {
    const chart = fixtureChart();
    const draft = fromChart(null, chart);
    expect(describes(draft, fixtureDerived(chart))).toBe(true);
    expect(describes(draft, null)).toBe(false);
    const gained = chartOf({
      units: chart.units,
      seats: [...chart.seats, { handle: "qa", name: "QA" }],
    });
    expect(describes(draft, fixtureDerived(gained))).toBe(false);
    expect(
      describes(draft, fixtureDerived(chart, { units: { engineering: { lead: "dev" } } })),
    ).toBe(false);
    // An inherited lead is the derivation's own answer, not a declaration to match.
    expect(
      describes(
        draft,
        fixtureDerived(chart, {
          units: { platform: { lead: "vp-engineering", lead_inherited: true } },
        }),
      ),
    ).toBe(true);
  });
});

describe("identity", () => {
  test("a renamed node keeps the key of the address it was created under", () => {
    const draft = fromChart(
      null,
      chartOf({
        units: [{ key: "rnd", name: "R&D", origin_key: "engineering" }],
        seats: [{ handle: "chief-tech", name: "CTO", unit: "rnd", origin_handle: "cto" }],
      }),
    );
    expect(draft.units[0]!.key).toBe("unit:engineering");
    expect(draft.units[0]!.roles[0]!.key).toBe("seat:cto");
    // The identity is the key's, not the data's: nothing edits it.
    expect(draft.units[0]!.roles[0]!.data).toEqual({ handle: "chief-tech", name: "CTO" });
    // Control: a node never renamed is keyed by its address, which is its identity.
    expect(fromChart(null, fixtureChart()).roles[0]!.key).toBe("seat:ceo");
  });

  test("a save moves a created node from its minted key to its identity, and a renamed one nowhere", () => {
    const base = fromChart(null, fixtureChart());
    let draft = base;
    for (const intent of [
      {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:sales" },
        data: { handle: "qa", name: "QA" },
      },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "zed" }] },
    ] as const) {
      const recorded = record(draft, intent);
      if (!recorded.ok) throw new Error(recorded.message);
      draft = apply(draft, recorded.op).draft;
    }
    const saved = fromChart(null, chartOfDraft(draft));
    expect([...allSeats(saved)].map(({ seat }) => seat.key)).toEqual(
      expect.arrayContaining(["seat:qa", "seat:dev"]),
    );
    expect(rekeying(draft, saved)).toEqual(new Map([["new:qa", "seat:qa"]]));
    const zed = locate(saved, "seat:dev");
    expect(zed?.kind === "seat" && zed.node.data.handle).toBe("zed");
    expect([...allUnits(saved)].map(({ unit }) => unit.key)).toEqual(
      [...allUnits(draft)].map(({ unit }) => unit.key),
    );
  });
});

describe("addresses", () => {
  test("an address a node used to answer to resolves to it until something else claims it", () => {
    const draft = fromChart(
      null,
      chartOf({
        seats: [
          { handle: "lead", name: "Lead", former_handles: ["boss", "chief"] } as never,
          { handle: "chief", name: "Chief" },
        ],
      }),
    );
    const index = addressIndex(draft);
    expect(index.seats.get("boss")?.data.name).toBe("Lead");
    // Somebody took `chief` since: it names them now.
    expect(index.seats.get("chief")?.data.name).toBe("Chief");
  });

  test("an address is suggested from a name, folded, bounded and free", () => {
    expect(slugOf("Ingénierie & Ops")).toBe("ingenierie-ops");
    expect(slugOf("—")).toBe("");
    expect(slugOf("x".repeat(80))).toHaveLength(64);
    expect(suggestAddress(["sales"], "Sales", "unit")).toBe("sales-2");
    expect(suggestAddress([], "—", "unit")).toBe("unit");
    expect(suggestAddress(["x".repeat(64)], "x".repeat(80), "seat")).toBe(`${"x".repeat(62)}-2`);
  });

  test("a name is suggested free, numbered past every number in use", () => {
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

describe("the settings patch", () => {
  const base = fixtureSettings();

  test("an unchanged draft patches nothing, and nothing outside what the builder edits is named", () => {
    expect(settingsPatch(base, cloneJson(base))).toEqual({});
    const draft = cloneJson(base);
    draft.future_setting = { changed: true };
    expect(settingsPatch(base, draft)).toEqual({});
  });

  test("a changed or removed charter field is named, a removal as an explicit null", () => {
    const draft = cloneJson(base);
    draft.mission = "Make better things.";
    delete draft.policies;
    expect(settingsPatch(base, draft)).toEqual({ mission: "Make better things.", policies: null });
    // An empty list and an absent one are the same document.
    expect(settingsPatch({ name: "X" }, { name: "X", policies: [] })).toEqual({});
  });

  test("the Datadog fallback is sent as its one value, or null when removed", () => {
    const moved = cloneJson(base);
    (moved.integrations as { datadog: { route_to?: string } }).datadog.route_to = "dev";
    expect(settingsPatch(base, moved)).toEqual({ integrations: { datadog: { route_to: "dev" } } });
    const cleared = cloneJson(base);
    delete (cleared.integrations as { datadog: { route_to?: string } }).datadog.route_to;
    expect(settingsPatch(base, cleared)).toEqual({ integrations: { datadog: { route_to: null } } });
  });

  test("a removed GitLab access level is null by handle, and the remaining entries are not resent", () => {
    const draft = cloneJson(base);
    const levels = (
      draft.integrations as { gitlab: { provisioning: { access_levels: Record<string, string> } } }
    ).gitlab.provisioning.access_levels;
    delete levels.dev;
    levels["account-executive"] = "developer";
    expect(settingsPatch(base, draft)).toEqual({
      integrations: {
        gitlab: {
          provisioning: { access_levels: { dev: null, "account-executive": "developer" } },
        },
      },
    });
  });

  test("with no base, every present edited value is named", () => {
    expect(settingsPatch(null, { name: "New", mission: "M", other: 1 })).toEqual({
      name: "New",
      mission: "M",
    });
  });
});
