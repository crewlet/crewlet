/**
 * A branch protection rule, said in a line: what it lets a push do and what
 * its merge gate asks of a merge. ONE wording, read by the repository page's
 * settings and by Settings › Gitlet, so the two can never describe one rule
 * two ways.
 */

import { fmtDateTime, plural } from "~/lib/format.ts";
import type { CodeMergeGate, CodeProtection, CodeRuleSource } from "~/protocol/index.ts";

/**
 * Who set a rule and when, in a line: the decider of the record that last
 * changed it, by the name the engine records them under (a seat's handle, a
 * person's seat, a credential's login), or the configuration, which names
 * nobody, and that record's instant.
 */
export function ruleSourceLine(source: CodeRuleSource): string {
  const who =
    source.set_by_kind === "config" || !source.set_by ? "the company document" : source.set_by;
  return `Set by ${who}, ${fmtDateTime(source.set_at)}`;
}

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

/**
 * What each class of a path is called after "a change of" (`gitlet.PathClasses`),
 * a class this build does not know said by its key.
 */
export const PATH_CLASS_WORDS: Readonly<Record<string, string>> = {
  whitespace_only: "only whitespace",
  comment_only: "only comments",
};

/** Classes said as a list: "only whitespace or only comments". */
function classWords(classes: readonly string[]): string {
  return classes.map((c) => PATH_CLASS_WORDS[c] ?? c).join(" or ");
}

/** The globs a person approves, named where there are few and counted where there are many. */
function humanPathsWords(paths: readonly { glob: string; unless?: string[] }[]): string {
  if (paths.length > 3) return `a person approves ${plural(paths.length, "path")}`;
  const each = paths.map((h) =>
    h.unless?.length ? `${h.glob} (unless a change of ${classWords(h.unless)})` : h.glob,
  );
  return `a person approves ${each.join(", ")}`;
}

/**
 * What a merge gate asks, in a line: every field of it a merge is held to,
 * so two rules the line says alike hold a merge alike.
 */
export function gateLine(gate: CodeMergeGate): string {
  const parts = [
    gate.merge_by === "persons"
      ? "merged by a person"
      : gate.merge_by === "maintainers"
        ? "merged by its maintainers"
        : "merged by its writers",
    gate.approvals === "one" ? "one approval" : "no approval",
  ];
  if (gate.approvals_unless?.length) {
    parts.push(`none for a change of ${classWords(gate.approvals_unless)}`);
  }
  if (gate.human_paths?.length) parts.push(humanPathsWords(gate.human_paths));
  if (gate.checks?.length) {
    parts.push(`${plural(gate.checks.length, "check")} pass`);
    // A GATE REQUIRING A CHECK LANDS THROUGH ITS QUEUE (the engine's rule:
    // the queue applies to a gated branch with a required check), so that
    // the queue tests each change before it lands, and how many at once, is
    // part of what the gate asks.
    parts.push(
      `landed by its queue, each change tested merged on the branch before it lands (${plural(gate.landing.depth, "candidate")} at once, ${plural(gate.landing.batch, "change")} each)`,
    );
    parts.push(
      gate.agents_edit_checks
        ? "a seat may approve a change to its checks (.crewlet/**)"
        : "a change to its checks (.crewlet/**) needs a person",
    );
  }
  if (gate.owners_approve) parts.push("its path owners approve");
  return parts.join(", ");
}
