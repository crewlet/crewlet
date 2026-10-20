import { expect, test } from "vitest";
import { indexOrg } from "~/lib/seats.ts";
import { withDerived } from "~/test/org.ts";
import { artifactBuilders } from "./builders.ts";

const ORG = indexOrg(
  withDerived({
    name: "Acme",
    timezone: "UTC",
    roles: [
      { name: "Jane Founder", handle: "jane", kind: "human" },
      { name: "Agent PM", handle: "pm", kind: "agent", goal: "Own the product roadmap" },
      { name: "Agent SWE", handle: "swe", kind: "agent", goal: "Ship the web frontend" },
      {
        name: "Agent Designer",
        handle: "designer",
        kind: "agent",
        responsibilities: ["UX research", "Brand and visual design"],
      },
    ],
    units: [],
  }) as never,
);

const order = (kind: Parameters<typeof artifactBuilders>[1], suggested?: string) =>
  artifactBuilders(ORG, kind, suggested).map((b) => [b.seat.handle, b.suggested]);

// EVERY AGENT IS OFFERED, and never a person: the artifact tools are built in,
// so the order is the only judgement, and the seat that speaks the kind's work
// comes first.
test("every agent is offered, the one whose work the kind is first", () => {
  expect(order("design")).toEqual([
    ["designer", true],
    ["pm", false],
    ["swe", false],
  ]);
  expect(order("html")[0]).toEqual(["swe", true]);
  expect(order("doc")[0]).toEqual(["pm", true]);
});

test("a page's own suggestion outranks the words, and an unknown one is ignored", () => {
  expect(order("design", "swe")[0]).toEqual(["swe", true]);
  expect(order("design", "nobody")[0]).toEqual(["designer", true]);
});

test("a kind no seat speaks keeps the chart's order", () => {
  expect(order("sheet").map(([h]) => h)).toEqual(["pm", "swe", "designer"]);
});
