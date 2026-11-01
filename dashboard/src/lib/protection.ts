/**
 * A branch protection rule, said in a line: what it lets a push do and what
 * its merge gate asks of a merge. ONE wording, read by the repository page's
 * settings and by Settings › Code, so the two can never describe one rule
 * two ways.
 */

import { plural } from "~/lib/format.ts";
import type { CodeMergeGate, CodeProtection } from "~/protocol/index.ts";

/** What a rule lets a push do to the refs it matches. */
export function pushRuleLine(rule: CodeProtection): string {
  const direct =
    rule.direct_push === "none"
      ? "nobody pushes to it directly"
      : rule.direct_push === "persons"
        ? "only a person pushes to it directly"
        : rule.direct_push;
  return `${direct}${rule.allow_force ? ", forced pushes allowed" : ""}${
    rule.allow_delete ? ", deletion allowed" : ""
  }`;
}

/** What a merge gate asks, in a line. */
export function gateLine(gate: CodeMergeGate): string {
  const parts = [
    gate.merge_by === "persons"
      ? "merged by a person"
      : gate.merge_by === "maintainers"
        ? "merged by its maintainers"
        : "merged by its writers",
    gate.approvals === "one" ? "one approval" : "no approval",
  ];
  if (gate.human_paths?.length)
    parts.push(`a person approves ${plural(gate.human_paths.length, "path")}`);
  if (gate.checks?.length) {
    parts.push(`${plural(gate.checks.length, "check")} pass`);
    // A GATE REQUIRING A CHECK LANDS THROUGH ITS QUEUE (the engine's rule:
    // the queue applies to a gated branch with a required check), so that
    // the queue tests each change before it lands is part of what the gate
    // asks. The engine has one way a queue lands, so the line names it
    // rather than reading a policy off the gate.
    parts.push("landed by its queue, each change tested merged on the branch before it lands");
  }
  if (gate.owners_approve) parts.push("its path owners approve");
  return parts.join(", ");
}
