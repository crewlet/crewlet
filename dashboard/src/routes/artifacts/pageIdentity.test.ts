import { expect, test } from "vitest";
import type { RoomMember } from "~/protocol/index.ts";
import type { Roster } from "~/lib/artifacts/room.ts";
import { pageRoster, pageSelf, pageViewer } from "./pageIdentity.ts";

const people = {
  index: { byHandle: new Map([["maya", {}]]) } as never,
  pagePerson: (handle: string, kind: "human" | "agent" = "human") => ({
    seat: handle,
    kind,
    displayName: handle === "maya" ? "Maya Ortiz" : handle || "Somebody",
  }),
  seatName: (handle: string) => (handle === "maya" ? "Maya Ortiz" : handle),
};

const member = (session: string, seat: string): RoomMember =>
  ({
    session,
    seat,
    kind: "human",
    activity: "viewing",
    focus: {},
    at: "2026-10-19T09:00:00Z",
  }) as unknown as RoomMember;

const roster = (...members: RoomMember[]): Roster => ({
  members: new Map(members.map((m) => [m.session, m])),
  counts: null,
  truncated: false,
});

test("a reader with a seat is told as that seat, by the name the dashboard shows them", () => {
  const self = pageSelf({ handle: "maya", kind: "human", name: "Maya O." }, people);
  expect(self).toEqual({ seat: "maya", kind: "human", displayName: "Maya O." });
  expect(pageViewer({ loading: false, anonymous: false }, self, "editor")).toEqual({
    ...self,
    role: "editor",
  });
});

test("a reader signed in without a seat is still somebody viewing, never unknown", () => {
  const named = pageSelf({ handle: "", kind: "", name: "Release operator" }, people);
  expect(named).toEqual({ seat: "", kind: "human", displayName: "Release operator" });
  expect(pageViewer({ loading: false, anonymous: false }, named, "owner")).toEqual({
    ...named,
    role: "owner",
  });
  // Without a name either, the reader is "You": the only one a page tells it to.
  expect(pageSelf({ handle: "", kind: "", name: "" }, people).displayName).toBe("You");
});

test("nobody is told as viewing before the dashboard knows who is signed in", () => {
  const self = pageSelf({ handle: "maya", kind: "human", name: "" }, people);
  expect(pageViewer({ loading: true, anonymous: false }, self, "viewer")).toBeNull();
  expect(pageViewer({ loading: false, anonymous: true }, self, "viewer")).toBeNull();
});

test("the reader's own session in the room is named as the reader, everybody else from the chart", () => {
  const self = pageSelf({ handle: "", kind: "", name: "" }, people);
  const told = pageRoster(
    roster(
      member("s-reader", "token:review"),
      member("s-maya", "maya"),
      member("s-other", "jo@example.com"),
    ),
    "s-reader",
    self,
    people,
  );
  expect(told.members.map((m) => [m.session, m.seat, m.displayName])).toEqual([
    ["s-reader", "", "You"],
    ["s-maya", "maya", "Maya Ortiz"],
    // The room names a seatless member by login, and a page is never told a login.
    ["s-other", "", "Somebody"],
  ]);
  expect(JSON.stringify(told)).not.toMatch(/token:review|jo@example\.com/);
  expect(told.truncated).toBe(false);
  expect(told).not.toHaveProperty("counts");
});
