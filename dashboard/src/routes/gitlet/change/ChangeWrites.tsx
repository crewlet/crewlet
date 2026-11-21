/**
 * A change's later life, written: its commits rebased onto its target's tip
 * (`code_rebase_change`), a queued change taken out of its target's merge
 * queue (`code_dequeue_change`), and a merged change reverted by a change of
 * its own (`code_revert_change`).
 *
 * EACH IS CONFIRMED IN THE PAGE, saying what it does before it is asked: a
 * rebase replays the change's commits and moves its branch by one forced
 * push, from the head on screen (`expected_head`), so a push made since is
 * refused; a change taken out of its queue loses its place and stays open; a
 * revert opens a new change into the same target and never moves the target
 * itself. Every control is drawn for every reader and held with its reason,
 * and the engine's refusal is drawn beside it: who may is the engine's to
 * say, from the change and the repository's chart.
 */

import { useState, type ReactNode } from "react";
import { Checkbox, Stack } from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { changeLive, parseChangeRef, refName, shortOID } from "~/lib/code.ts";
import { useAct } from "~/lib/useAct.ts";
import type { CodeChangeDetail } from "~/protocol/index.ts";
import { changeHref } from "../paths.ts";

/** "Rebase", beside a live change's merge controls. */
export function RebaseButton({ change }: { change: CodeChangeDetail }) {
  const write = useAct("code_rebase_change");
  const [confirming, setConfirming] = useState(false);
  const target = refName(change.target_ref).name;
  const source = refName(change.source_ref).name;
  const held = changeLive(change.state)
    ? undefined
    : `This change is ${change.state}: its commits are rebased no more.`;
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={held}
        onPress={() => setConfirming(true)}
      >
        Rebase
      </WriteButton>
      <ConfirmModal
        open={confirming}
        title={`Rebase ${change.ref} onto ${target}?`}
        message={`Its commits are replayed in order onto ${target}'s tip, each keeping its author, and ${source} is moved to them by one forced push from ${shortOID(change.head)}, the head on this page. A push made to it since is refused, and the first commit that conflicts stops the rebase with nothing changed.`}
        cancelLabel="Cancel"
        confirmLabel="Rebase"
        onClose={() => setConfirming(false)}
        submitting={write.busy}
        onConfirm={async () => {
          await write.run(
            { change: change.ref, expected_head: change.head },
            { done: `Rebased ${change.ref} onto ${target}` },
          );
          setConfirming(false);
        }}
      />
    </>
  );
}

/**
 * "Take out of the queue": on a change's page beside its merge controls, and
 * on a row of Gitlet › Queue. `queued` is whether the page read it in its
 * target's queue; a row of the queue is in it by definition.
 */
export function DequeueButton({
  changeRef,
  target,
  queued,
  label = "Take out of the queue",
  leadingIcon,
}: {
  /** The change's reference, web!42. */
  changeRef: string;
  /** Its target branch, by name. */
  target: string;
  queued: boolean;
  label?: string;
  leadingIcon?: ReactNode;
}) {
  const write = useAct("code_dequeue_change");
  const [confirming, setConfirming] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={queued ? undefined : `It is not in ${target}'s merge queue.`}
        aria-label={`Take ${changeRef} out of the queue`}
        leadingIcon={leadingIcon}
        onPress={() => setConfirming(true)}
      >
        {label}
      </WriteButton>
      <ConfirmModal
        open={confirming}
        title={`Take ${changeRef} out of ${target}'s merge queue?`}
        message="It loses its place in the queue and stays open. It merges only when it is added to the queue again, from the end."
        cancelLabel="Leave it queued"
        confirmLabel="Take out of the queue"
        onClose={() => setConfirming(false)}
        submitting={write.busy}
        onConfirm={async () => {
          await write.run({ change: changeRef }, { done: `Took ${changeRef} out of the queue` });
          setConfirming(false);
        }}
      />
    </>
  );
}

/** The words a revert's receipt says beyond its toast's title: the change it opened. */
function revertLanded(receipt: unknown): string | undefined {
  const r = receipt as { opened?: string; change_not_opened?: string } | null;
  if (r?.opened) return `Its change is ${r.opened}.`;
  return r?.change_not_opened || undefined;
}

/** A change by its reference, as a link to its page. */
function ChangeLink({ refText }: { refText: string }) {
  const named = parseChangeRef(refText);
  return named ? (
    <a className="t-link mono" href={changeHref(named.repo, named.number)}>
      {refText}
    </a>
  ) : (
    <span className="mono">{refText}</span>
  );
}

/**
 * A merged change's one control, where a live change's merge controls were:
 * Revert, by a change of its own. While a revert of it is open, or once one
 * merged, the box links that change in Revert's place; a closed revert gives
 * Revert back.
 */
export function RevertChangePanel({ change }: { change: CodeChangeDetail }) {
  const write = useAct("code_revert_change");
  const [confirming, setConfirming] = useState(false);
  const [merge, setMerge] = useState(false);
  // THE REVERT THIS PAGE OPENED, until the change's read names it.
  const [opened, setOpened] = useState("");
  const target = refName(change.target_ref).name;
  if (change.state !== "merged") return null;
  const by = change.reverted_by;
  const standing = by && by.state !== "closed" ? by : null;
  const shown = standing?.ref ?? (by ? "" : opened);
  return (
    // IN THE MERGE BOX, under the head that says into what it merged.
    <section className="code-merge-box__controls" aria-label="Revert">
      {shown ? (
        <p className="muted">
          Reverted by <ChangeLink refText={shown} />
          {standing
            ? `, ${standing.state === "merged" ? "merged" : `still ${standing.state}`}`
            : ""}
          .
        </p>
      ) : (
        <div className="row gap-2 wrap">
          {by && (
            <p className="muted">
              Its revert <ChangeLink refText={by.ref} /> was closed.
            </p>
          )}
          <WriteButton
            write={write}
            size="small"
            variant="secondary"
            onPress={() => setConfirming(true)}
          >
            Revert
          </WriteButton>
        </div>
      )}
      {/* NOT DESTRUCTIVE: a revert adds a change and moves nothing it
          does not merge, and the merge is a commit of its own. */}
      <ConfirmModal
        open={confirming}
        title={`Revert ${change.ref}?`}
        cancelLabel="Cancel"
        confirmLabel={merge ? "Revert and merge" : "Open the revert"}
        onClose={() => setConfirming(false)}
        submitting={write.busy}
        onConfirm={async () => {
          const result = await write.run(
            { change: change.ref, ...(merge ? { merge: true } : {}) },
            {
              done: merge ? `Reverted ${change.ref}` : `Opened the revert of ${change.ref}`,
              landed: revertLanded,
            },
          );
          const receipt =
            result && (result.kind === "applied" || result.kind === "pending")
              ? (result.receipt as { opened?: string } | null)
              : null;
          if (receipt?.opened) setOpened(receipt.opened);
          setConfirming(false);
        }}
      >
        <Stack gap={3}>
          <p>
            What its merge brought into {target} is undone on a new branch from {target}’s tip,
            opened as a change of its own into {target}. {target} itself does not move until that
            change merges.
          </p>
          <Checkbox
            label="Merge the revert once it is open"
            description="As its gate allows. Left off, the revert waits for its own review and merge."
            checked={merge}
            onCheckedChange={setMerge}
          />
        </Stack>
      </ConfirmModal>
    </section>
  );
}
