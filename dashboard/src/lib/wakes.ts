/**
 * What woke a turn, in words, and who can change how often it happens: the
 * turn ledger's wake reasons (`types.WakeOf` in the engine), `kind:reason`.
 */

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

/** The words for `wake`, for the agent `seat` it woke. */
export function wakeWords(wake: string, seat: string): WakeWords {
  const at = wake.indexOf(":");
  const kind = at < 0 ? wake : wake.slice(0, at);
  const reason = at < 0 ? "" : wake.slice(at + 1);
  const why = reason ? ` (${said(reason)})` : "";
  switch (kind) {
    case "schedule":
      return {
        label: `Schedule ${reason}`,
        who: "An admin, by changing how often it runs",
        path: ["company", "schedules", "role", seat, reason],
      };
    case "work_item":
      return {
        label: reason === "pickup" ? "Work handed to it while idle" : `Work items${why}`,
        who: "Whoever assigns it work or mentions it",
        path: ["worklet"],
      };
    case "chat_message":
      return {
        label: `Chat messages${why}`,
        who: "People and agents who write to it",
        path: ["chatlet"],
      };
    case "colleague_ask":
      return {
        label: "Other agents' questions",
        who: "The agents that ask it",
        path: ["company", "org", "seats", seat],
      };
    case "code_change":
      return { label: `Code changes${why}`, who: "Whoever asks it to review", path: ["gitlet"] };
    case "alarm":
      return {
        label: "Engine alarms",
        who: "Whoever fixes what raised the alarm",
        path: ["settings", "backups"],
      };
  }
  return { label: said(wake), who: "The engine", path: [] };
}
