/**
 * A hand-off's refusal notice: the access to ask an admin for where that access
 * is the way past the rule, and the refusal's own words everywhere else.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { Refusal } from "~/lib/useAct.ts";
import { taskRefusal } from "./AskAgent.tsx";

afterEach(cleanup);

/** A filing the engine refused on authority, by `reason`, naming `grants`. */
function refused(reason: string, grants: string[], sentence: string): Refusal {
  return { sentence, hint: "", retryable: false, grants, reason, remedy: null };
}

test("a refusal some access clears names that access", () => {
  render(<>{taskRefusal(refused("no_grant", ["worklet"], "You don’t have access to this."))}</>);
  expect(screen.getByText("You need Worklet access for this. Ask an admin.")).toBeTruthy();
});

// THE RULE DECIDES WHETHER ACCESS IS THE WAY PAST, NEVER THE GRANTS ALONE: a
// proof gone stale names the access that admitted the reader, and an
// artifact's role the admin override beside the share its owner makes.
// Mutation: decide by the grants alone and both rows fail.
test.each([
  ["a proof gone stale", "step_up", "Confirm it’s you first."],
  [
    "a role on an artifact below the verb's",
    "role",
    "Your role on this doesn't allow that. Whoever owns it can share it with you at a higher one.",
  ],
])("%s is the refusal's own sentence", (_, reason, sentence) => {
  const { container } = render(<>{taskRefusal(refused(reason, ["admin"], sentence))}</>);
  expect(container.textContent).toContain(sentence);
  expect(container.textContent).not.toMatch(/You need .* access/);
});
