/**
 * The readers a chat write is drawn for and refused to, and how a suite says
 * a control was held with why.
 *
 * ONE LIST FOR EVERY CHAT SUITE that `app/writeGate.test.tsx` names: nobody
 * signed in, a person without the Chatlet app, a person the engine makes no
 * chat change for, and a credential acting as no seat. Each suite renders its
 * controls for all four, so a new control cannot pass by being drawn for
 * three of them.
 */

import { waitFor } from "@testing-library/react";
import { expect } from "vitest";

import { noAccessReason, WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { withDerived } from "./org.ts";
import { ADA } from "./chat.tsx";

/** The four readers who may not make a chat change, and the sentence each is told. */
export const WRITE_READERS = [
  {
    who: "nobody signed in",
    viewer: { login: "", owner: "", handle: "", name: "", kind: "", grants: [], acts: [] },
    reason: WRITE_REASONS.anonymous,
  },
  {
    // THE APP FIRST: a remedy they can ask an admin for, where "this engine
    // does not make this change for you" would send them nowhere.
    who: "a person without the Chatlet app",
    viewer: { ...ADA, grants: ["worklet"], acts: [] },
    reason: noAccessReason(["chatlet"]),
  },
  {
    who: "a person the engine makes no chat change for",
    viewer: { ...ADA, acts: [] },
    reason: WRITE_REASONS.not_served,
  },
  {
    // HOLDING CHATLET AND NO SEAT: the engine offers a seated verb to nobody
    // acting as themselves, so `acts` names none and every control says so.
    who: "a credential acting as no seat",
    viewer: {
      ...ADA,
      login: "ci:deploy",
      owner: "ci:deploy",
      handle: "",
      name: "",
      kind: "",
      acts: [],
    },
    reason: WRITE_REASONS.not_served,
  },
];

/**
 * Why a control is held, as the reader is told: its title, or the words it is
 * described by (a kit control that says its reason beside itself, a menu
 * item's description), or null when it is not held.
 */
export function holdOf(control: HTMLElement): string | null {
  const held =
    control.getAttribute("aria-disabled") === "true" || (control as HTMLButtonElement).disabled;
  if (!held) return null;
  const said = (control.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => (id ? document.getElementById(id)?.textContent : ""))
    .join(" ")
    .trim();
  // THE WORDS IT IS DESCRIBED BY COME FIRST: the kit's icon button titles
  // itself with its label and says its reason in a described-by node.
  return said || (control.getAttribute("title") ?? control.textContent);
}

/** A control held with `reason`: drawn, refusing the press, and saying why. */
export async function heldWith(control: HTMLElement, reason: string): Promise<void> {
  await waitFor(() => expect(holdOf(control)).toBe(reason));
}

/** What every chat read is served at, for a suite's answers. */
export const SERVED = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 4 },
};

/**
 * A small company: the reader, a colleague, and an agent on a team, which is
 * who the pickers, faces and member lists draw from.
 */
export const CHAT_ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "Ada", handle: "ada", kind: "human", goal: "Run it" },
    { name: "Bo Chen", handle: "bo", kind: "human", goal: "Ship it" },
    { name: "Ops Agent", handle: "ops", goal: "Keep it running" },
  ],
  units: [],
});
