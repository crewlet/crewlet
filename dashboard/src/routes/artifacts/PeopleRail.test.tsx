/**
 * The People rail: everybody in the room in words, one entry a seat, Follow
 * beside everybody but the reader, and an agent's turn one press away.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { PeopleRail } from "./PeopleRail.tsx";
import type { ArtifactPresenceMember } from "~/protocol/index.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";

afterEach(cleanup);

const NAMES: Record<string, string> = {
  maya: "Maya Ortiz",
  pm: "Product Manager",
  jane: "Jane Founder",
};

const people = {
  seatName: (handle: string) => NAMES[handle] ?? handle,
  seatPrincipal: (handle: string) => ({
    name: NAMES[handle] ?? handle,
    kind: handle === "pm" ? "agent" : "human",
  }),
} as unknown as ArtifactPeople;

function member(over: Partial<ArtifactPresenceMember>): ArtifactPresenceMember {
  return {
    session: "s-1",
    seat: "maya",
    kind: "human",
    activity: "viewing",
    focus: {},
    at: "2026-10-18T10:00:00Z",
    ...over,
  };
}

const room = [
  member({ session: "a", activity: "idle" }),
  member({
    session: "b",
    activity: "editing",
    focus: { collection: "review", doc: "r1", field: "score" },
  }),
  member({
    session: "c",
    seat: "pm",
    kind: "agent",
    activity: "viewing",
    turn: "run-7",
    focus: { version: 3 },
  }),
];

test("everybody is listed once, in words, with Follow and an agent's turn", () => {
  const onFollow = vi.fn();
  render(
    <PeopleRail
      members={room}
      self="jane"
      people={people}
      following=""
      onFollow={onFollow}
      onClose={() => {}}
    />,
  );
  const rows = within(screen.getByRole("list")).getAllByRole("listitem");
  expect(rows).toHaveLength(3);
  expect(rows[0]?.textContent).toContain("You");
  // TWO TABS OF ONE PERSON ARE ONE ENTRY, the most active.
  expect(screen.getAllByText("Maya Ortiz")).toHaveLength(1);
  expect(screen.getByText("Editing on r1, score")).toBeTruthy();
  // AN AGENT READS, and its turn is a link to the turn.
  expect(screen.getByText("Reading on version 3")).toBeTruthy();
  const turn = screen.getByRole("link", { name: "Open turn" });
  expect(turn.getAttribute("href")).toBe("#/live/turns/run-7");
  fireEvent.click(screen.getByRole("button", { name: "Follow Maya Ortiz" }));
  expect(onFollow).toHaveBeenCalledWith("maya");
});

test("the person followed says so, and pressing again stops", () => {
  const onFollow = vi.fn();
  render(
    <PeopleRail
      members={room}
      self="jane"
      people={people}
      following="maya"
      onFollow={onFollow}
      onClose={() => {}}
    />,
  );
  const stop = screen.getByRole("button", { name: "Stop following Maya Ortiz" });
  expect(stop.getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(stop);
  expect(onFollow).toHaveBeenCalledWith(null);
});

test("a room of one says nobody else is here", () => {
  render(
    <PeopleRail
      members={[]}
      self="jane"
      people={people}
      following=""
      onFollow={() => {}}
      onClose={() => {}}
    />,
  );
  expect(screen.getByText("Nobody else is here right now.")).toBeTruthy();
});
