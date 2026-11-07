import { expect, test } from "vitest";
import { projectionRead } from "./projection.ts";
import { fromDocument, placeDerivation, toDocument } from "./document.ts";
import { seatKey, unitKey } from "./keys.ts";
import type { OrgProjection } from "~/protocol/index.ts";

// A projection of a company whose root seat Ana is placed in Core by its
// `unit:` reference: the projection writes her at the root, and the engine's
// derivation (with no paths, as the projection carries it) says she sits in Core.
const ORG = {
  name: "Kestrel",
  roles: [
    {
      name: "Ana",
      handle: "ana",
      kind: "agent",
      llm: { execute: ["main"] },
      tool_sources: ["builtin"],
    },
    { name: "Robin", handle: "robin", kind: "human" },
  ],
  units: [
    {
      id: "core",
      name: "Core",
      lead: "ana",
      roles: [{ name: "Bo", handle: "bo", kind: "agent" }],
    },
  ],
  derived: {
    seats: [
      {
        handle: "ana",
        name: "Ana",
        kind: "agent",
        manager: "robin",
        managers: ["robin"],
        reports: [],
        auto_reports: [],
        onboarding_chain: [],
      },
      {
        handle: "robin",
        name: "Robin",
        kind: "human",
        manager: "",
        managers: [],
        reports: ["ana"],
        auto_reports: [],
        onboarding_chain: [],
      },
      {
        handle: "bo",
        name: "Bo",
        kind: "agent",
        manager: "ana",
        managers: ["ana"],
        reports: [],
        auto_reports: [],
        onboarding_chain: [],
      },
    ],
    units: [
      {
        id: "core",
        name: "Core",
        type: "team",
        lead: "ana",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["ana", "bo"],
      },
    ],
  },
} as unknown as OrgProjection;

// A READER THE CONFIGURATION IS GUARDED FROM reads the org from the
// projection, and MEMBERSHIP IS THE ENGINE'S: a seat moved into a unit by its
// reference is drawn there, not at the root where the projection wrote it, and
// the derivation is given that document's paths, so leads and managers land
// on the nodes they describe. What only the projection resolves (model
// chains, tool sources) is never handed to the builder as if it were written.
//
// Mutation: keep seats where the projection wrote them, leave the derivation
// without paths, or pass `llm` through, and this fails.
test("the projection is read as a document placed and derived as the engine did", () => {
  const { document, derived } = projectionRead(ORG);
  expect(document.roles?.map((r) => r.handle)).toEqual(["robin"]);
  expect(document.units?.[0]?.roles?.map((r) => r.handle)).toEqual(["ana", "bo"]);
  expect(JSON.stringify(document)).not.toContain("tool_sources");
  expect(JSON.stringify(document)).not.toContain('"llm"');

  const { index } = toDocument(fromDocument(document));
  const placed = placeDerivation(index, derived);
  expect(placed.seatByKey.get(seatKey("bo"))?.manager).toBe("ana");
  expect(placed.seatByKey.get(seatKey("ana"))?.unit_path).toBe(index.pathOf.get(unitKey("core")));
  expect(placed.unitByKey.size).toBe(1);
});

test("a projection with no derivation is drawn as written, with none", () => {
  const { document, derived } = projectionRead({ ...ORG, derived: undefined } as OrgProjection);
  expect(derived).toBeNull();
  expect(document.units?.[0]?.roles?.map((r) => r.handle)).toEqual(["bo"]);
  expect(document.roles?.map((r) => r.handle)).toEqual(["ana", "robin"]);
});
