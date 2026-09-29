/**
 * Updating a draft onto the company as it is now: what still applies, what is
 * already there, what is dropped, and a choice for every change somebody else
 * also made.
 *
 * NOTHING IS REPLAYED OVER A CHANGED VALUE WITHOUT A PERSON CHOOSING. The
 * reducer's pending update is `history.rebase` of the draft's log onto the
 * company the engine holds now — its chart and its settings — and every
 * operation lands in one of four places this dialog shows: it still applies;
 * the company already holds it (a save that landed part of the draft, or a
 * colleague who made the same change); its target is gone, so it is dropped
 * with the reason; or a value it recorded was changed upstream, and the
 * operator keeps their value or the one now saved. The rebase is re-run on
 * every choice, so the counts on screen are always what confirming adopts,
 * and confirming waits until every conflict has a choice.
 *
 * The same dialog restores a kept draft onto a company that moved while the
 * tab was away, and carries the rest of a save that stopped part way onto the
 * company that save left; there, cancelling a restore discards the kept draft
 * rather than leaving a draft on a company that is gone.
 *
 * A VALUE IS SHOWN AS A PERSON READS IT, NEVER AS THE BUILDER HOLDS IT. A
 * conflict about a field carries that field's values; one about where a node
 * sits, a removal, an address somebody else now holds or a kind change
 * carries node keys, whole nodes or the stripped fields themselves, which the
 * model tags with their shape. Node keys become names, a whole node becomes
 * the fields that differ, stripped fields are named and never valued, and a
 * credential's mask reads as a literal that is set, never as the marker, as
 * everywhere else on the page.
 */

import { configValueKind, plural } from "~/lib/format.ts";
import type { Choice, RebaseEntry } from "./model/history.ts";
import { isRecord, jsonEqual } from "./model/json.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { fieldName, type Conflict, type Operation } from "./model/operations.ts";
import type { PendingUpdate } from "./model/reducer.ts";
import { RefreshGlyph } from "@crewlethq/icons/glyphs";
import { Button, Modal, SegmentedControl, Table } from "@crewlethq/ui";

/** A node's name for its key, or `null` when no draft at hand holds it. */
export type NameOf = (key: NodeKey) => string | null;

const NOT_SET = "Not set";

/** A field's recorded value as a person reads it. */
export function formatValue(value: unknown): string {
  if (value === undefined || value === null || value === "") return NOT_SET;
  if (typeof value === "string") {
    return configValueKind(value) === "hidden" ? "A literal value is set (hidden)" : value;
  }
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) return value.length === 0 ? "None" : value.map(formatValue).join(", ");
  if (isRecord(value)) {
    const entries = Object.entries(value);
    if (entries.length === 0) return "None";
    return entries.map(([key, v]) => `${key}: ${formatValue(v)}`).join("; ");
  }
  return String(value);
}

/** The three cells of a conflict, in the order the table shows them. */
export interface ConflictCells {
  readonly base: string;
  readonly theirs: string;
  readonly mine: string;
}

/** A conflict's three values as a person reads them (see the module doc). */
export function conflictCells(conflict: Conflict, nameOf: NameOf): ConflictCells {
  const name = (key: unknown): string =>
    (typeof key === "string" && nameOf(key)) || "a node no longer in the organization";
  const where = (key: unknown): string =>
    key === COMPANY_KEY ? "The top of the organization" : name(key);
  const each = (show: (value: unknown) => string): ConflictCells => ({
    base: show(conflict.base),
    theirs: show(conflict.theirs),
    mine: show(conflict.mine),
  });
  switch (conflict.shape) {
    case "parent":
      return each((key) => (key === undefined ? NOT_SET : where(key)));
    case "snapshot": {
      // AN ADDRESS SOMEBODY ELSE NOW HOLDS: this draft created a node under
      // it, and the company holds one there already.
      if (conflict.address !== undefined) {
        const label = (value: unknown) =>
          isRecord(value) && typeof value.name === "string" && value.name !== ""
            ? value.name
            : conflict.address!;
        return {
          base: "Free",
          theirs: `Held by ${label(conflict.theirs)}`,
          mine: `This draft's ${label(conflict.mine)}`,
        };
      }
      const before = isRecord(conflict.base) ? conflict.base : {};
      const now = isRecord(conflict.theirs) ? conflict.theirs : {};
      const changed = [...new Set([...Object.keys(before), ...Object.keys(now)])].filter(
        (key) => !jsonEqual(before[key], now[key]),
      );
      return {
        base: "As it was",
        theirs: changed.length === 0 ? "Changed" : `Changed: ${changed.join(", ")}`,
        mine: "Removed",
      };
    }
    case "fields": {
      // Named, never valued: these are the fields a kind change strips, and
      // several of them are credentials.
      const fields = (value: unknown) =>
        new Map(
          (Array.isArray(value) ? value : [])
            .filter(isRecord)
            .map((f) => [fieldName(Array.isArray(f.path) ? (f.path as string[]) : []), f.before]),
        );
      const before = fields(conflict.base);
      const now = fields(conflict.theirs);
      const list = (names: string[]) => (names.length === 0 ? "None" : names.join(", "));
      return {
        base: list([...before.keys()]),
        theirs: list(
          [...now.entries()].map(([field, value]) =>
            !before.has(field)
              ? `${field} (added)`
              : jsonEqual(before.get(field), value)
                ? field
                : `${field} (changed)`,
          ),
        ),
        mine: "Removed",
      };
    }
    default:
      return each(formatValue);
  }
}

export function UpdateDraftDialog({
  update,
  describe,
  nameOf,
  onChoose,
  onConfirm,
  onCancel,
}: {
  update: PendingUpdate;
  /** One sentence for an operation, naming the nodes it touched. */
  describe: (op: Operation) => string;
  /** A node's name for a key a conflict's values carry. */
  nameOf: NameOf;
  onChoose: (index: number, choice: Choice | null) => void;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { entries, pending } = update.result;
  const applies = entries.filter((e) => e.outcome === "applies");
  const already = entries.filter(
    (e): e is Extract<RebaseEntry, { outcome: "already" }> => e.outcome === "already",
  );
  const gone = entries.filter(
    (e): e is Extract<RebaseEntry, { outcome: "gone" }> => e.outcome === "gone",
  );
  const conflicts = entries.filter(
    (e): e is Extract<RebaseEntry, { outcome: "conflict" }> => e.outcome === "conflict",
  );
  const title = update.restoring ? "Restore the kept draft" : "Update my draft and review";

  return (
    <Modal
      open
      stackBody
      title={title}
      icon={<RefreshGlyph />}
      size="lg"
      // ONE WAY OUT PER JOB. "Not now" is in the foot; a close control beside
      // the title would be a second, unnamed spelling of it.
      showCloseButton={false}
      onClose={onCancel}
      // The reason the primary action is unavailable, at the foot's own start
      // edge rather than a flexing span inside the actions.
      footerStart={pending > 0 ? `Choose for ${plural(pending, "conflict")} first.` : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onCancel}>
            {update.restoring ? "Discard the kept draft" : "Not now"}
          </Button>
          <Button variant="primary" onClick={onConfirm} disabled={pending > 0}>
            {update.restoring ? "Restore the draft" : "Update my draft"}
          </Button>
        </>
      }
    >
      <p>
        {update.restoring
          ? "The company changed since this draft was kept. Each change in it is replayed onto the company as it is now."
          : "The company changed since you started editing. Each of your changes is replayed onto it as it is now."}
      </p>
      <p className="t-caption">
        {applies.length + already.length === entries.length
          ? "Every change still applies."
          : `${applies.length + already.length} of ${plural(entries.length, "change")} still ${applies.length + already.length === 1 ? "applies" : "apply"}.`}
      </p>

      {already.length > 0 && (
        <section className="col gap-1" aria-label="Changes the company already holds">
          <strong>Already in the company</strong>
          <ul className="org-builder-list">
            {already.map((entry) => (
              <li key={entry.index}>
                {describe(entry.op)}{" "}
                <span className="muted">
                  {entry.resolved && entry.resolved.length > 0
                    ? "Part of it is saved already; the rest is kept."
                    : "It is saved already."}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      {gone.length > 0 && (
        <section className="col gap-1" aria-label="Dropped changes">
          <strong>No longer applies, and is dropped</strong>
          <ul className="org-builder-list">
            {gone.map((entry) => (
              <li key={entry.index}>
                {describe(entry.op)} <span className="muted">{entry.reason}</span>
              </li>
            ))}
          </ul>
        </section>
      )}

      {conflicts.length > 0 && (
        <section className="col gap-3" aria-label="Conflicts">
          <strong>Changed by somebody else as well</strong>
          {conflicts.map((entry) => {
            const label = describe(entry.op);
            return (
              <div key={entry.index} className="org-builder-conflict col gap-2">
                <span>{label}</span>
                <Table
                  caption={`What changed under this edit: ${label}`}
                  captionHidden
                  headers={["What", "When you started", "Saved now", "Yours"]}
                  data={entry.conflicts.map((c) => {
                    const cells = conflictCells(c, nameOf);
                    return [c.subject, cells.base, cells.theirs, cells.mine];
                  })}
                />
                <SegmentedControl<Choice | "">
                  label={`Resolve: ${label}`}
                  semantics="radio"
                  size="sm"
                  value={entry.choice ?? ""}
                  onValueChange={(v) => onChoose(entry.index, v === "" ? null : v)}
                  options={[
                    { value: "mine", label: "Keep mine" },
                    { value: "theirs", label: "Keep theirs" },
                  ]}
                />
              </div>
            );
          })}
        </section>
      )}
    </Modal>
  );
}
