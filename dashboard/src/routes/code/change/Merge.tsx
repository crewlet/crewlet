/**
 * A live change's merge controls, above its tabs: where its gate stands, and
 * Merge — or Add to queue, where its target's gate requires a check — with
 * how it merges, whether its branch goes and whether its work item closes;
 * Override, a person's merge past the gate; and Re-run checks.
 *
 * # The engine decides, and the page says what it decided
 *
 * Every press is ONE act through `useAct` and a `WriteButton`, never
 * optimistic. Merge sends the head ON SCREEN as `expected_head`, so a push
 * that landed after the page was read is refused `head_moved` rather than
 * merged unseen. The controls are drawn for every reader and held, with a
 * sentence, where the engine would refuse: Merge with the gate's own first
 * unmet precondition (`readiness.first`, judged for THIS reader as the
 * merger, so a reader without the authority to merge is told so), Override
 * for anybody but a person.
 *
 * # A refusal in its own words
 *
 * Several of the code host's refusals share one class — a head that moved and
 * a train whose base moved are both `stale_version` — so the controls answer
 * by the host's own word (`hostRefusal`, `contract/merge.ts`), and fall back
 * to the engine's sentence where theirs would say less: the gate's every
 * unmet precondition (`not_ready`), or why this reader may not
 * (`not_permitted`).
 *
 * WHAT THIS IS NOT: a landing-policy choice or an auto-revert consent. A
 * gated target's queue lands each change tested merged on the branch, the
 * one way the engine lands one.
 */

import { useState } from "react";
import { Button, Callout, Modal, SegmentedControl, Textarea } from "@crewlethq/ui";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { MERGE_CONFLICT, MERGE_METHODS, MERGE_REFUSALS } from "~/contract/merge.ts";
import { changeLive } from "~/lib/code.ts";
import { useAct, type Act } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { ActionTool } from "~/protocol/act.ts";
import type { CodeChangeDetail, CodePrecondition } from "~/protocol/index.ts";

type MergeMethod = (typeof MERGE_METHODS)[number];
type MergeRefusal = (typeof MERGE_REFUSALS)[number];

/** How each method is offered. */
const METHOD_LABELS: Readonly<Record<MergeMethod, { label: string; title: string }>> = {
  merge: { label: "Merge commit", title: "Its head merged with a merge commit" },
  squash: { label: "Squash", title: "Its changes landed as one commit" },
  fast_forward: {
    label: "Fast-forward",
    title: "Its target moved to its head, which must already hold the target's tip",
  },
};

/**
 * What the controls say for each of the code host's refusals, or `null` for
 * the engine's own sentence — which names what this page cannot: the gate's
 * every unmet precondition and who clears it, the rule a reader's merge or
 * a deletion broke, a size it went past. COMPLETE OVER `MERGE_REFUSALS` by
 * the compiler, so a refusal the engine grows is a type error here before
 * it is a sentence in the general words of its class.
 */
export const MERGE_REFUSAL_SENTENCES: Readonly<Record<MergeRefusal, string | null>> = {
  repo_deleted: "Its repository is deleted, so nothing merges into it.",
  repo_archived: "Its repository is archived and read-only, so nothing merges into it.",
  repo_importing: "Its repository is still being imported. Try again once the import is done.",
  repo_mirrored:
    "Its repository is a pull mirror: it moves only by syncs from its source, and takes no merge.",
  expunging: "An expunge began on its repository while the merge was built. Merge it again.",
  change_not_found: "The repository holds no such change any more.",
  change_not_open: "It is no longer open: somebody merged or closed it since you loaded it.",
  change_draft: "It is a draft, and a draft merges nowhere. Mark it ready first.",
  stale: "Its target changed since you loaded it. Look again, then merge it.",
  head_moved:
    "Somebody pushed to it since you loaded it. Read what the push changed, then merge the head you read.",
  target_moved: "Its target moved while the merge was built. Merge it again.",
  target_moving:
    "Its target moved under every rebuild of the merge: the branch moves faster than a merge is built onto it. Add it to the queue, or try again at a quieter moment.",
  not_person: "Only a person overrides a merge gate.",
  queue_target:
    "Its target's gate requires a check, so its merges land through the queue: add it to the queue, or override as a person.",
  no_queue: "Its target has no queue: its gate requires no check, so it merges directly.",
  already_queued: "It is in its target's queue already.",
  queue_full: "Its target's queue is full. Try again once a change has landed.",
  gate_unmet: null,
  source_refused: null,
  upload_expired: "The merge took too long to record. Merge it again.",
  repo_size: null,
  compaction_behind:
    "Its repository is waiting for a compaction before it takes more. Try again in a moment.",
  not_queued: "It is not in its target's queue.",
  not_permitted: null,
  not_green: "Its target's required checks have not all passed on what it would land on.",
  not_red: "None of the checks named failed on that landing.",
  no_landing: "There is no landing above its target's green one.",
  record_full: "The landing holds more than one record may. Its queue lands fewer at once.",
  busy: "Its repository's committer is busy. Try again in a moment.",
  not_applied: "The merge was recorded and dropped on every node. Look again before you retry.",
  unavailable: "The code log refused it on every node. Try again in a moment.",
};

/** What the controls say when the merge's build met a conflict with its target's tip. */
const CONFLICT_SENTENCE =
  "It does not merge cleanly onto its target's tip: rebase it, or resolve the conflict on its branch.";

/** The controls' own words for a refusal, or null for the engine's. */
export function mergeRefusalSentence(hostRefusal: string): string | null {
  if (hostRefusal === MERGE_CONFLICT) return CONFLICT_SENTENCE;
  return (MERGE_REFUSALS as readonly string[]).includes(hostRefusal)
    ? MERGE_REFUSAL_SENTENCES[hostRefusal as MergeRefusal]
    : null;
}

/** A merge write's refusal, drawn in the controls' words where they have some. */
function MergeRefusalNote<T extends ActionTool>({ write }: { write: Act<T> }) {
  const said = write.refusal ? mergeRefusalSentence(write.refusal.hostRefusal) : null;
  return (
    <RefusalNote
      write={
        said && write.refusal ? { ...write, refusal: { ...write.refusal, sentence: said } } : write
      }
    />
  );
}

/** Where a live change's gate stands, as the controls read it. */
export function mergeStanding(change: CodeChangeDetail): {
  /** Its target's gate requires a check, so it is ADDED TO THE QUEUE rather than merged. */
  viaQueue: boolean;
  /** Its place in the queue, where it holds one. */
  queued: CodePrecondition | undefined;
  /** Why a Merge would be refused now, or undefined where the gate holds. */
  held: string | undefined;
} {
  const pre = change.readiness?.preconditions ?? [];
  const viaQueue = pre.some((p) => p.kind === "check");
  const queued = pre.find((p) => p.kind === "queue");
  const readiness = change.readiness;
  let held: string | undefined;
  if (!changeLive(change.state)) held = `This change is ${change.state}: it merges no more.`;
  else if (change.state === "draft") held = "A draft merges nowhere: mark it ready first.";
  else if (queued) held = queued.detail;
  else if (!readiness) held = "Its gate has not been judged on this read yet.";
  else if (readiness.met < readiness.total) held = readiness.first;
  return { viaQueue, queued, held };
}

export function MergePanel({ change }: { change: CodeChangeDetail }) {
  const merge = useAct("code_merge_change");
  const rerun = useAct("code_rerun_checks");
  const viewer = useViewer();
  const [method, setMethod] = useState<MergeMethod>(MERGE_METHODS[0]);
  // UNSAID UNTIL CHOSEN: the engine's own default — a seat's branch deleted
  // unless its rule keeps it, any other kept — stands until the person
  // ticks it either way, because a box drawn ticked or unticked on a guess
  // would send a choice nobody made.
  const [deleteBranch, setDeleteBranch] = useState<boolean | null>(null);
  const [closeTask, setCloseTask] = useState(false);
  const [overriding, setOverriding] = useState(false);
  if (!changeLive(change.state)) return null;
  const { viaQueue, held } = mergeStanding(change);
  const readiness = change.readiness;
  const work = change.work_item;
  const verb = viaQueue ? "Add to queue" : "Merge";
  const person = viewer.kind === "human";
  const overrideHeld = !person
    ? "Only a person overrides a merge gate."
    : change.state === "draft"
      ? "A draft merges nowhere, overridden or not: mark it ready first."
      : !held
        ? `Its gate holds: ${viaQueue ? "add it to the queue" : "merge it"} instead.`
        : undefined;
  const submit = async () => {
    await merge.run(
      {
        change: change.ref,
        expected_head: change.head,
        method,
        ...(deleteBranch === null ? {} : { delete_branch: deleteBranch }),
        ...(closeTask ? { close_task: true } : {}),
      },
      { done: viaQueue ? `Added ${change.ref} to the queue` : `Merged ${change.ref}` },
    );
  };
  return (
    <section className="col gap-2" aria-label="Merge">
      <p className={held ? "muted" : undefined}>
        {readiness
          ? `${readiness.met} of ${readiness.total} of its gate's conditions met${
              held ? ` — ${held}` : ": it may merge."
            }`
          : held}
      </p>
      <div className="row gap-2 wrap">
        <SegmentedControl<MergeMethod>
          semantics="radio"
          activate="manual"
          size="sm"
          label="How it merges"
          value={method}
          onValueChange={setMethod}
          options={MERGE_METHODS.map((value) => ({ value, ...METHOD_LABELS[value] }))}
        />
        <label className="row gap-1">
          <input
            type="checkbox"
            checked={deleteBranch === true}
            onChange={(event) => setDeleteBranch(event.target.checked)}
          />
          Delete its branch
          <span className="muted t-caption">
            {deleteBranch === null
              ? "(unsaid: a seat's branch goes unless its rule keeps it; any other stays)"
              : deleteBranch
                ? "(deleted as it merges)"
                : "(kept)"}
          </span>
        </label>
        {work && (
          <label className="row gap-1">
            <input
              type="checkbox"
              checked={closeTask}
              onChange={(event) => setCloseTask(event.target.checked)}
            />
            Close {work.key || work.id} once it merges
          </label>
        )}
      </div>
      <div className="row gap-2 wrap">
        <WriteButton
          write={merge}
          size="small"
          variant="primary"
          blocked={held}
          showRefusal={false}
          onPress={() => void submit()}
        >
          {verb}
        </WriteButton>
        <WriteButton
          write={merge}
          size="small"
          variant="secondary"
          blocked={overrideHeld}
          showRefusal={false}
          onPress={() => setOverriding(true)}
        >
          Override
        </WriteButton>
        <WriteButton
          write={rerun}
          size="small"
          variant="ghost"
          onPress={() =>
            void rerun.run(
              { change: change.ref, all_failed: true },
              { done: `Ran ${change.ref}'s failed checks again` },
            )
          }
        >
          Re-run checks
        </WriteButton>
      </div>
      {!overriding && <MergeRefusalNote write={merge} />}
      {overriding && (
        <OverrideDialog
          change={change}
          method={method}
          deleteBranch={deleteBranch}
          closeTask={closeTask}
          onClose={() => setOverriding(false)}
        />
      )}
    </section>
  );
}

/**
 * A person's merge past the gate: merged directly — even where the target's
 * merges land through its queue — and audited, with the reason they give,
 * which the change keeps beside its merge.
 */
function OverrideDialog({
  change,
  method,
  deleteBranch,
  closeTask,
  onClose,
}: {
  change: CodeChangeDetail;
  method: MergeMethod;
  deleteBranch: boolean | null;
  closeTask: boolean;
  onClose: () => void;
}) {
  const write = useAct("code_merge_change");
  const [reason, setReason] = useState("");
  const submit = async () => {
    const result = await write.run(
      {
        change: change.ref,
        expected_head: change.head,
        method,
        override_reason: reason.trim(),
        ...(deleteBranch === null ? {} : { delete_branch: deleteBranch }),
        ...(closeTask ? { close_task: true } : {}),
      },
      { done: `Merged ${change.ref} past its gate` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  const { held } = mergeStanding(change);
  return (
    <Modal
      open
      size="sm"
      title={`Override ${change.ref}'s gate`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="danger"
            showRefusal={false}
            blocked={reason.trim() ? undefined : "Say why it merges past its gate."}
            onPress={() => void submit()}
          >
            Merge past the gate
          </WriteButton>
        </>
      }
    >
      <Callout variant="warning">
        It merges now, directly, whatever its gate still waits on
        {held ? ` — ${held}` : ""}. Your reason is kept with the merge, and the override is audited.
      </Callout>
      <Textarea
        aria-label="Reason"
        placeholder="Why it merges past its gate"
        value={reason}
        rows={3}
        onChange={(event) => setReason(event.target.value)}
      />
      <MergeRefusalNote write={write} />
    </Modal>
  );
}
