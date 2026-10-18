/**
 * Who the two people-picking gestures offer.
 *
 * ONE RULE WORTH A TEST ABOVE THE OTHERS: the viewer is never in the list. On
 * a direct conversation the server completes the participants with the
 * caller's own seat, so offering somebody their own name is offering the one
 * value this surface refuses to be told — and on a room create it is offering
 * to add the author to the room they are making.
 */

import { describe, expect, test } from "vitest";

import { handlesOf, peopleOptions } from "./people.ts";
import type { Seat } from "~/lib/seats.ts";

/** A seat as the org index reports one, cut to what this module reads. */
const seat = (handle: string, name: string, kind: "agent" | "human") =>
  ({ handle, name, kind }) as Seat;

describe("who is offered", () => {
  test("the viewer is not offered to themself", () => {
    const options = peopleOptions([seat("ada", "Ada", "human"), seat("bo", "Bo", "human")], "ada");
    expect(options.map((option) => option.value)).toEqual(["bo"]);
  });

  test("a seat the engine reports no handle for is not offered at all", () => {
    // A handle is DERIVED by the engine and this client deliberately does not
    // re-implement that rule, so a seat without one cannot be named — and a
    // row whose value is "" would be a participant the write path drops.
    const options = peopleOptions(
      [seat("", "Nameless", "agent"), seat("bo", "Bo", "human")],
      "ada",
    );
    expect(options.map((option) => option.value)).toEqual(["bo"]);
  });

  test("agents are offered, because that is what the company runs on", () => {
    // A picker that offered only the humans would make the product's own
    // premise unreachable from the screen a person starts a conversation on.
    const options = peopleOptions([seat("ops", "Ops", "agent")], "ada");
    expect(options).toHaveLength(1);
    expect(options[0]!.group).toBe("Agents");
  });

  test("people come before agents, and each group is alphabetical", () => {
    const options = peopleOptions(
      [
        seat("zoe", "Zoe", "human"),
        seat("ops", "Ops", "agent"),
        seat("ann", "Ann", "human"),
        seat("bot", "Bot", "agent"),
      ],
      "ada",
    );
    expect(options.map((option) => option.label)).toEqual(["Ann", "Zoe", "Bot", "Ops"]);
  });
});

describe("what is sent", () => {
  test("a handle chosen twice is one participant", () => {
    // The id of a direct conversation is derived from the SET of handles, so
    // a duplicate changes nothing at the engine and would only be a second
    // row in a request.
    expect(handlesOf(["bo", "bo", " cas ", ""])).toEqual(["bo", "cas"]);
  });
});
