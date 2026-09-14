/**
 * Updating a draft onto a newer revision: what still applies, what is
 * dropped, and a choice for every change somebody else also made.
 *
 * NOTHING IS REPLAYED OVER A CHANGED VALUE WITHOUT A PERSON CHOOSING. The
 * reducer's pending update is `history.rebase` of the draft's log onto the
 * revision the engine holds now, and every operation lands in one of three
 * places this dialog shows: it still applies; its target is gone, so it is
 * dropped with the reason; or a value it recorded was changed upstream, and
 * the operator keeps their value or the one now saved. The rebase is re-run
 * on every choice, so the counts on screen are always what confirming adopts,
 * and confirming waits until every conflict has a choice.
 *
 * The same dialog restores a kept draft onto a revision that moved while the
 * tab was away; there, cancelling discards the kept draft rather than leaving
 * a draft on an older revision.
 */

import { plural } from "~/lib/format.ts";
import { Dialog } from "~/ui/Dialog.tsx";
import { Button, Segmented } from "~/ui/primitives.tsx";
import type { Choice, RebaseEntry } from "./model/history.ts";
import type { Operation } from "./model/operations.ts";
import type { PendingUpdate } from "./model/reducer.ts";

/** A recorded value as a person reads it. */
export function formatValue(value: unknown): string {
  if (value === undefined || value === null || value === "") return "Not set";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value) && value.every((v) => typeof v === "string")) {
    return value.length === 0 ? "None" : value.join(", ");
  }
  return JSON.stringify(value);
}

export function UpdateDraftDialog({
  update,
  describe,
  onChoose,
  onConfirm,
  onCancel,
}: {
  update: PendingUpdate;
  /** One sentence for an operation, naming the nodes it touched. */
  describe: (op: Operation) => string;
  onChoose: (index: number, choice: Choice | null) => void;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { entries, pending } = update.result;
  const applies = entries.filter((e) => e.outcome === "applies");
  const gone = entries.filter(
    (e): e is Extract<RebaseEntry, { outcome: "gone" }> => e.outcome === "gone",
  );
  const conflicts = entries.filter(
    (e): e is Extract<RebaseEntry, { outcome: "conflict" }> => e.outcome === "conflict",
  );
  const title = update.restoring ? "Restore the kept draft" : "Update my draft and review";

  return (
    <Dialog
      title={title}
      icon="refresh"
      width={640}
      onClose={onCancel}
      footer={
        <>
          {pending > 0 && (
            <span className="t-caption muted">Choose for {plural(pending, "conflict")} first.</span>
          )}
          <span className="spacer" />
          <Button onClick={onCancel}>
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
          ? "The configuration changed since this draft was kept. Each change in it is replayed onto the configuration as it is now."
          : "Another revision was saved since you started editing. Each of your changes is replayed onto it."}
      </p>
      <p className="t-caption">
        {applies.length === entries.length
          ? "Every change still applies."
          : `${applies.length} of ${plural(entries.length, "change")} still ${applies.length === 1 ? "applies" : "apply"}.`}
      </p>

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
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th scope="col">What</th>
                        <th scope="col">When you started</th>
                        <th scope="col">Saved now</th>
                        <th scope="col">Yours</th>
                      </tr>
                    </thead>
                    <tbody>
                      {entry.conflicts.map((c, i) => (
                        <tr key={i}>
                          <td>{c.subject}</td>
                          <td>{formatValue(c.base)}</td>
                          <td>{formatValue(c.theirs)}</td>
                          <td>{formatValue(c.mine)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <Segmented<Choice | "">
                  ariaLabel={`Resolve: ${label}`}
                  semantics="radio"
                  size="sm"
                  value={entry.choice ?? ""}
                  onChange={(v) => onChoose(entry.index, v === "" ? null : v)}
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
    </Dialog>
  );
}
