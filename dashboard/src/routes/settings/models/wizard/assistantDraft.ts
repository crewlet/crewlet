/**
 * The model wizard's draft as the Dashboard Assistant reads it: what is being
 * set up, field by field, in the words the wizard uses, so a question about it
 * ("is Haiku fast enough for routines?") is answered about this draft.
 *
 * READ, NEVER SET. Every field here changes through the wizard's own step,
 * which tests a sign-in before anything is stored; a change the assistant
 * made would skip that. And the credential is absent: not its value, not the
 * secret it is stored under. A pasted key never leaves the page.
 */

import type { DraftFieldSpec } from "~/app/assistant/draft.ts";
import {
  DEFAULT_HARNESS_MAX_CONCURRENT,
  DEFAULT_HARNESS_TIMEOUT_SECONDS,
} from "~/contract/models.ts";
import { listed } from "~/lib/format.ts";
import { CATEGORY_ORDER, categoryState } from "~/lib/modelUses.ts";
import { CATEGORY_WORDS, RUN_IN, product } from "../words.ts";
import type { Draft } from "./draft.ts";
import { METHOD_WORDS } from "./steps/SignInStep.tsx";

/** What the wizard tells the assistant it can do with the draft. */
export const MODEL_DRAFT_HELP =
  "This is a model being set up in the Models wizard, not saved. Read it to answer questions about it; the person changes it on the wizard's own steps, where a sign-in is tested before anything is stored.";

/** The seats, by handle, that use the draft for a category, whole or in part. */
function seatsFor(draft: Draft, seats: readonly string[]): string {
  const lines = CATEGORY_ORDER.flatMap((c) => {
    const on = seats.filter((s) => categoryState(draft.uses, s, c) !== "none");
    return on.length > 0 ? [`${CATEGORY_WORDS[c].label}: ${listed(on)}`] : [];
  });
  if (draft.uses.assistant) lines.push("Ask Crewlet");
  return lines.length > 0 ? lines.join("\n") : "Nothing yet";
}

/** The draft's fields, each read in the wizard's words; `seats` are the company's agent seats by handle. */
export function modelDraftFields(seats: readonly string[]): readonly DraftFieldSpec<Draft>[] {
  return [
    { path: "name", label: "Name", read: (d) => d.key },
    { path: "harness", label: "Harness", read: (d) => product(d.harness) },
    { path: "method", label: "Method", read: (d) => METHOD_WORDS[d.method].label },
    { path: "model", label: "Model", read: (d) => d.model },
    {
      path: "where_it_runs",
      label: "Where it runs",
      read: (d) => (d.runIn ? RUN_IN[d.runIn] : "The company default"),
    },
    { path: "used_for", label: "Used for", read: (d) => seatsFor(d, seats) },
    {
      path: "processes_at_once",
      label: "Processes at once",
      read: (d) => d.concurrent || String(DEFAULT_HARNESS_MAX_CONCURRENT),
    },
    {
      path: "timeout_seconds",
      label: "Timeout, in seconds",
      read: (d) => d.timeout || String(DEFAULT_HARNESS_TIMEOUT_SECONDS),
    },
    { path: "web_tools", label: "Web tools", read: (d) => (d.webDenied ? "Denied" : "Allowed") },
  ];
}
