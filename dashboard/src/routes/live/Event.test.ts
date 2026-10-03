/**
 * Where an event's seat link goes.
 *
 * An event's actor is a display name ("Agent PM") and a seat's page is
 * addressed by its handle, so the actor's link and button went to
 * `#/agents/seats/Agent PM` — no seat's address — on every event a seat
 * published. The seat is found by the store's own `agent_id` tag instead.
 *
 * Mutation: return the actor from `seatHandleOf`, and the first case fails.
 */

import { expect, test } from "vitest";

import { actorKindOf, seatHandleOf } from "./Event.tsx";
import { indexOrg } from "~/lib/seats.ts";
import type { AgentRow, EventRecord } from "~/protocol/index.ts";

const event = (over: Partial<EventRecord>): EventRecord => ({
  id: "e-1",
  type: "agent_phase_completed",
  timestamp: "2026-09-13T10:00:00Z",
  source: "Agent PM",
  actor: "Agent PM",
  summary: "",
  category: "llm",
  trace_id: "",
  span_id: "",
  parent_span_id: "",
  topic: "",
  ...over,
});

const agents = [{ role: "Agent PM", handle: "agent-pm", agent_id: "id-pm" }] as AgentRow[];

test("an event's seat is its agent_id's handle, never its actor", () => {
  expect(seatHandleOf(event({ tags: { agent_id: "id-pm" } }), agents)).toBe("agent-pm");
});

test("an event naming no seat this tab knows links nowhere", () => {
  expect(seatHandleOf(event({}), agents)).toBe("");
  expect(seatHandleOf(event({ tags: { agent_id: "id-gone" } }), agents)).toBe("");
});

// A PERSON'S WRITE IS RECORDED UNDER THE SEAT THEY ARE BOUND TO, by its handle
// (`iam.ActorFor`, author kind `human`), and carries no agent id — so the
// actor is the address, read through the chart. An operator's login is no
// seat's, though it may spell like one.
//
// Mutation: drop the `human` arm, and the first expectation fails; resolve any
// actor through the chart, and the operator links to a seat.
test("a person's event links to the seat their writes are recorded under", () => {
  const index = indexOrg({ roles: [{ name: "Ada Founder", handle: "ada", kind: "human" }] });
  const person = event({ actor: "ada", tags: { actor_kind: "human", operator_id: "session:x" } });
  expect(seatHandleOf(person, agents, index)).toBe("ada");
  expect(actorKindOf(person)).toBe("human");
  const operator = event({ actor: "ada", tags: { actor_kind: "operator" } });
  expect(seatHandleOf(operator, agents, index)).toBe("");
});

// THE KIND IS THE ENGINE'S, off the promoted tag or the payload, and a kind
// nobody recorded is no kind rather than a guess.
test("an event's actor kind is the recorded one, from the tag or the payload", () => {
  expect(actorKindOf(event({ payload: { actor_kind: "operator" } }))).toBe("operator");
  expect(actorKindOf(event({ tags: { actor_kind: "agent" }, payload: { actor_kind: "x" } }))).toBe(
    "agent",
  );
  expect(actorKindOf(event({}))).toBe("");
});
