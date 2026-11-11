/**
 * What woke a turn, in words, and who can change how often it happens: the
 * turn ledger's wake reasons (`types.WakeOf` in the engine), `kind:reason`.
 */

import type { WakeKind } from "~/contract/spend.ts";

export interface WakeWords {
  /** What woke the turns. */
  label: string;
  /** Who can make it happen less. */
  who: string;
  /** Where they do it; empty where nothing on the dashboard changes it. */
  path: string[];
}

/** A reason as its source writes it (`ask_answered`), as a phrase. */
function said(reason: string): string {
  return reason.replaceAll("_", " ");
}

/** Each kind's words, from the reason and the seat; every kind has some. */
const KINDS: Record<WakeKind, (reason: string, seat: string) => WakeWords> = {
  schedule: (reason, seat) => ({
    label: `Schedule ${reason}`,
    who: "An admin, by changing how often it runs",
    path: ["company", "schedules", "role", seat, reason],
  }),
  work_item: (reason) => ({
    label: reason === "pickup" ? "Work handed to it while idle" : `Work items${why(reason)}`,
    who: "Whoever assigns it work or mentions it",
    path: ["worklet"],
  }),
  chat_message: (reason) => ({
    label: `Chat messages${why(reason)}`,
    who: "People and agents who write to it",
    path: ["chatlet"],
  }),
  colleague_ask: (_, seat) => ({
    label: "Other agents' questions",
    who: "The agents that ask it",
    path: ["company", "org", "seats", seat],
  }),
  code_change: (reason) => ({
    label: `Code changes${why(reason)}`,
    who: "Whoever asks it to review",
    path: ["gitlet"],
  }),
  alarm: () => ({
    label: "Engine alarms",
    who: "Whoever fixes what raised the alarm",
    path: ["settings", "backups"],
  }),
  // A REQUEST IT DECIDES as a delegate (ADR-0125): fewer come by delegating
  // fewer kinds to it, which the company document's rules say.
  delegated_request: () => ({
    label: "Requests it decides as a delegate",
    who: "An admin, by changing the delegated decisions rules",
    path: ["settings", "delegated-decisions"],
  }),
};

function why(reason: string): string {
  return reason ? ` (${said(reason)})` : "";
}

function isKind(kind: string): kind is WakeKind {
  return Object.hasOwn(KINDS, kind);
}

/** The words for `wake`, for the agent `seat` it woke. */
export function wakeWords(wake: string, seat: string): WakeWords {
  const at = wake.indexOf(":");
  const kind = at < 0 ? wake : wake.slice(0, at);
  const reason = at < 0 ? "" : wake.slice(at + 1);
  // A KIND THIS BUILD DOES NOT KNOW is said as written, and says so.
  return isKind(kind)
    ? KINDS[kind](reason, seat)
    : { label: said(wake), who: "Not described in this version", path: [] };
}
