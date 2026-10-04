// @vitest-environment node
/**
 * A lead's draft of one unit (`scope.ts`).
 *
 * What these protect: the draft is the unit as a document of its own, sent
 * whole to the unit's own address under the revision it was read at, and
 * checked there unchanged too; every path the engine writes
 * about the whole company is moved into the draft's coordinates, and what
 * lies outside the unit is never placed on whichever node of the draft sits
 * at that path; and the parts of the draft that reach outside the unit — the
 * company's charter and top level, the unit's own place and lead — are
 * refused at the recording door with the reason, while everything inside it
 * is recorded as for the whole company.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { fromDocument, toDocument } from "./document.ts";
import { COMPANY_KEY, seatKey, unitKey } from "./keys.ts";
import { builderReducer, INITIAL_BUILDER, recordIntent } from "./reducer.ts";
import { inScope, scopedDocument, scopedTransport, scopeLimit } from "./scope.ts";
import { fixtureDerived } from "./testkit.ts";
import {
  checkRequest,
  saveRequest,
  type ConfigRequest,
  type ConfigTransport,
  type HttpAnswer,
} from "./transport.ts";

/** The company as the engine holds it: Engineering is its SECOND unit. */
const COMPANY: CompanyDocument = {
  name: "Acme",
  roles: [{ name: "CEO", handle: "ceo" }],
  units: [
    { name: "Sales", id: "sales", roles: [{ name: "Seller", handle: "seller" }] },
    {
      name: "Engineering",
      id: "eng",
      lead: "dev",
      roles: [
        { name: "Dev", handle: "dev" },
        { name: "QA", handle: "qa" },
      ],
      children: [{ name: "Tools", id: "tools", roles: [{ name: "Tooler", handle: "tooler" }] }],
    },
  ],
};
const ENGINEERING = COMPANY.units![1]!;
const DRAFT_DOC = scopedDocument("Acme", ENGINEERING as unknown as Record<string, unknown>);

function loaded() {
  return builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: DRAFT_DOC,
    revision: "r1",
    scope: "eng",
  });
}

describe("the requests a lead's draft makes", () => {
  const inputs = (sent: CompanyDocument) => ({
    mode: "edit" as const,
    baseRevision: "r1",
    base: DRAFT_DOC,
    sent: toDocument(fromDocument(sent)),
    scope: "eng",
  });

  test("a change is the unit whole, at its own address, under the revision it was read at", () => {
    const changed = structuredClone(DRAFT_DOC);
    changed.units![0]!.purpose = "Build";
    const check = checkRequest(inputs(changed));
    expect(check).toMatchObject({
      method: "PUT",
      path: "/config/units/eng",
      query: { dry_run: "true" },
      headers: { "If-Match": '"r1"' },
    });
    expect(check.body).toEqual(changed.units![0]);
    const save = saveRequest(inputs(changed), "Edited Engineering");
    expect(save).toMatchObject({ method: "PUT", path: "/config/units/eng", query: {} });
    expect(save.body).toEqual({ ...changed.units![0], _summary: "Edited Engineering" });
  });

  // Storing a unit as it was is no lead's write, but checking it is: the
  // engine answers the dry run with what it derives of the unit as it stands.
  test("an unchanged unit is checked at its own address like a changed one", () => {
    expect(checkRequest(inputs(DRAFT_DOC))).toEqual({
      method: "PUT",
      path: "/config/units/eng",
      query: { dry_run: "true" },
      contentType: "application/json",
      headers: { "If-Match": '"r1"' },
      body: DRAFT_DOC.units![0],
    });
  });
});

describe("the engine's answer, in the draft's coordinates", () => {
  const derived = fixtureDerived(COMPANY);

  test("a derivation keeps the unit's own seats and units, moved to where the draft holds them", () => {
    const body = inScope({ valid: true, derived }, "eng") as {
      derived: { seats: { handle: string; path: string }[]; units: { id: string; path: string }[] };
    };
    expect(body.derived.seats.map((s) => [s.handle, s.path])).toEqual([
      ["dev", "units[0].roles[0]"],
      ["qa", "units[0].roles[1]"],
      ["tooler", "units[0].children[0].roles[0]"],
    ]);
    expect(body.derived.units.map((u) => [u.id, u.path])).toEqual([
      ["eng", "units[0]"],
      ["tools", "units[0].children[0]"],
    ]);
  });

  test("a problem inside the unit is placed in it, and one outside is said about the whole draft", () => {
    const body = inScope(
      {
        derived,
        problems: [
          {
            path: "units[1].roles[1].goal",
            segments: ["units", 1, "roles", 1, "goal"],
            message: "a",
          },
          // Sales' first seat sits where the draft holds Dev.
          {
            path: "units[0].roles[0].goal",
            segments: ["units", 0, "roles", 0, "goal"],
            message: "b",
          },
        ],
        warnings: [
          { path: "units[1].lead", segments: ["units", 1, "lead"], message: "c" },
          { path: "roles[0]", segments: ["roles", 0], message: "d" },
        ],
      },
      "eng",
    ) as { problems: unknown[]; warnings: unknown[] };
    expect(body.problems).toEqual([
      { path: "units[0].roles[1].goal", segments: ["units", 0, "roles", 1, "goal"], message: "a" },
      { path: "", segments: null, message: "b" },
    ]);
    expect(body.warnings).toEqual([
      { path: "units[0].lead", segments: ["units", 0, "lead"], message: "c" },
    ]);
  });

  test("with no derivation to say where the unit sits, nothing is placed by its path", () => {
    const body = inScope(
      { problems: [{ path: "units[1].name", segments: ["units", 1, "name"], message: "a" }] },
      "eng",
    ) as { problems: unknown[] };
    expect(body.problems).toEqual([{ path: "", segments: null, message: "a" }]);
  });

  test("the transport reads the unit as the draft's document and answers in its coordinates", async () => {
    const sent: ConfigRequest[] = [];
    const answers: HttpAnswer[] = [
      { status: 200, body: ENGINEERING, etag: '"r1"' },
      { status: 200, body: { valid: true, base_revision_id: "r1", derived } },
      {
        status: 400,
        body: {
          derived,
          problems: [{ path: "units[1].name", segments: ["units", 1, "name"], message: "a" }],
        },
      },
    ];
    const inner: ConfigTransport = {
      current: () => Promise.reject(new Error("a lead never reads the company")),
      send: async (request) => {
        sent.push(request);
        return answers.shift()!;
      },
      revision: () => Promise.reject(new Error("a lead never reads the history")),
    };
    const transport = scopedTransport(inner, "eng", () => "Acme");
    const signal = new AbortController().signal;
    expect(await transport.current(signal)).toEqual({
      status: 200,
      body: DRAFT_DOC,
      etag: '"r1"',
    });
    expect(sent[0]).toMatchObject({ method: "GET", path: "/config/units/eng" });
    const check = checkRequest({
      mode: "edit",
      baseRevision: "r1",
      base: DRAFT_DOC,
      sent: toDocument(fromDocument(DRAFT_DOC)),
      scope: "eng",
    });
    const checked = await transport.send(check, signal);
    expect(checked.body).toMatchObject({
      valid: true,
      derived: { units: [{ id: "eng", path: "units[0]" }, { id: "tools" }] },
    });
    const refused = await transport.send({ ...check, method: "PUT", body: {} }, signal);
    expect(refused.body).toMatchObject({
      problems: [{ path: "units[0].name", segments: ["units", 0, "name"] }],
    });
  });
});

describe("what a lead's draft refuses", () => {
  const state = loaded();
  const top = unitKey("eng");

  test("the company's charter, its top level, and the unit's own place and lead", () => {
    const refused = (intent: Parameters<typeof recordIntent>[1]) => {
      const answer = recordIntent(state, intent);
      return answer.ok ? null : [answer.refusal, answer.message];
    };
    expect(
      refused({ type: "updateCompany", set: [{ path: ["name"], value: "Acme Two" }] }),
    ).toEqual([
      "scope",
      "The company's charter and settings are not part of Engineering: changing them takes the config:write grant.",
    ]);
    expect(
      refused({
        type: "addSeat",
        key: "new:a",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "Analyst", handle: "analyst" },
      }),
    ).toEqual([
      "scope",
      "Only Engineering and what is inside it can be changed here: the company's top level takes the config:write grant.",
    ]);
    expect(refused({ type: "remove", target: top })).toEqual([
      "scope",
      "Where Engineering sits decides who leads it, so moving or deleting it takes the config:write grant.",
    ]);
    expect(refused({ type: "setLead", target: top, lead: "qa" })).toEqual([
      "scope",
      "Engineering's own lead decides who may change it, so changing it takes the config:write grant.",
    ]);
  });

  test("everything inside the unit is recorded as for the whole company", () => {
    for (const intent of [
      { type: "updateSeat", target: seatKey("qa"), set: [{ path: ["goal"], value: "Test" }] },
      { type: "setLead", target: unitKey("tools"), lead: "qa" },
      { type: "remove", target: unitKey("tools") },
      {
        type: "addSeat",
        key: "new:b",
        placement: { parent: top, after: null },
        data: { name: "Analyst", handle: "analyst" },
      },
    ] as const) {
      expect(recordIntent(state, intent)).toMatchObject({ ok: true });
    }
  });

  test("a whole company's draft has no such limit", () => {
    expect(scopeLimit(null, state.draft, COMPANY_KEY, "edit")).toBeNull();
    expect(scopeLimit(null, state.draft, top, "lead")).toBeNull();
  });
});
