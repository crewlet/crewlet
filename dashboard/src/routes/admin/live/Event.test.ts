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

import { seatHandleOf } from "./Event.tsx";
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
