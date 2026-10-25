/**
 * The question a colleague's change raises: somebody saved this part while the
 * person was writing it, and whose words stand is theirs to decide.
 *
 * WHAT CHANGED IS SHOWN, not just that something did: a line diff of the part
 * from what the person started with to what it says now, so they can tell a
 * typo fixed from a rewrite before choosing. Their own draft stays in the
 * field above it, unchanged.
 *
 * TWO ANSWERS AND NO THIRD. Keep my draft makes the colleague's words the base,
 * so the next Save writes the draft over them knowingly; Use theirs drops the
 * draft and shows the part as it is now. Prose is never merged for the
 * person: two rewrites of one sentence have no merge that either author wrote.
 */

import { Button, ConflictNote, DiffView, collapseDiff, diffLines } from "@crewlethq/ui";
import {
  PART_TITLES,
  partName,
  trimLikeEngine,
  type CharterPart,
  type PartValue,
} from "./charter.ts";

export function ConflictPanel({
  part,
  base,
  theirs,
  onKeepMine,
  onTakeTheirs,
}: {
  part: CharterPart;
  /** What the person started from. */
  base: PartValue;
  /** What the part says now. */
  theirs: PartValue;
  onKeepMine: () => void;
  onTakeTheirs: () => void;
}) {
  const subject = partName(part);
  return (
    <div className="charter-edit__conflict">
      {/* NOBODY IS NAMED: the engine cannot say who changed the charter
          without guessing (a later unrelated write would be named), and a
          name that can be wrong is worse than none. */}
      <ConflictNote
        subject={subject.charAt(0).toUpperCase() + subject.slice(1)}
        labels={{
          changed: (name) =>
            `${name} changed while you were editing. Your draft is kept: compare, then keep yours or use theirs.`,
        }}
        actions={
          <>
            <Button size="small" variant="secondary" onClick={onTakeTheirs}>
              Use theirs
            </Button>
            <Button size="small" variant="primary" onClick={onKeepMine}>
              Keep my draft
            </Button>
          </>
        }
      />
      <DiffView
        aria-label={`${PART_TITLES[part]}: what changed since you started`}
        sections={collapseDiff(diffLines(asText(base), asText(theirs)))}
      />
    </div>
  );
}

/**
 * A part as lines to compare: a statement as written, the policies one line
 * each. NOT NUMBERED: a policy removed from the top would renumber every line
 * under it, and the diff would show the whole list rewritten.
 */
export function asText(value: PartValue): string {
  return typeof value === "string"
    ? trimLikeEngine(value)
    : value.map((policy) => trimLikeEngine(policy)).join("\n");
}
