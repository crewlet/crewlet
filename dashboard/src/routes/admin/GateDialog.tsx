/**
 * Evicting a node from the state log's register, and readmitting one.
 *
 * # This is the screen's one WRITE, and the only one that can answer `pending`
 *
 * Every other panel on Fleet reads. This gesture appends a record to the log
 * and therefore has a write OUTCOME, which is three-valued (D134) and which
 * this dialog renders as three visibly different things:
 *
 *   - `applied` — durable at the returned position AND in this node's own
 *     rows. The confirmation.
 *   - `pending` — durable at the position, NOT applied here yet, so what it
 *     produced is unresolved on the node that answered. It is not a failure
 *     and it must not be retried: the record is already on the log. It renders
 *     as an in-flight chip carrying the position, and the panel's next poll is
 *     what clears it.
 *   - `unknown` — no acknowledgement at all, which is the ONE outcome where
 *     retrying is correct, so it is the one that renders as a failure and
 *     carries the op id to retry with.
 *
 * **A dialog that rendered `pending` as success would be the browser half of
 * the lie the durable-versus-applied split exists to prevent** — and worse
 * here than at the tool seam, because a person believes a screen.
 *
 * # Why the eviction is not immediate, and why the dialog says so
 *
 * The node stays COUNTED for one fence window after the gesture, so a live
 * node is certain to have noticed before the trim stops waiting for it. An
 * operator who does not know that reads the unchanged watermark as a failure
 * and runs the gesture again — which is why the confirmation names the window
 * before the button is pressed rather than after.
 */

import { useState } from "react";
import { Button, Callout, InlineCode, Input, Modal } from "@crewlethq/ui";
import { DnsGlyph, ScheduleGlyph } from "@crewlethq/icons/glyphs";
import { rest, RestError } from "~/protocol/index.ts";
import type { RetentionGateResult } from "~/protocol/index.ts";

export function GateDialog({
  node,
  evict,
  onClose,
}: {
  node: string;
  /** True to evict, false to readmit. */
  evict: boolean;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<RetentionGateResult | null>(null);

  const verb = evict ? "Evict" : "Readmit";
  // THE NODE ID TYPED OUT, like every other irreversible gesture on this
  // dashboard. An eviction changes what the trim may delete on every other
  // node, so a misclick here costs a fleet its replay window.
  const confirmed = typed.trim() === node;

  async function submit() {
    if (busy || !confirmed) return;
    setBusy(true);
    setError(null);
    try {
      // THE TYPED CONFIRMATION TRAVELS. The server refuses unless
      // `?confirm=` repeats the node id — checking it only in the
      // browser made the gesture unreachable from this dashboard for
      // every node, since the request it sent was always a 400.
      const id = encodeURIComponent(node);
      const answer = (await rest.post(
        `/work/retention/${evict ? "evict" : "readmit"}/${id}?confirm=${id}`,
      )) as RetentionGateResult;
      setResult(answer);
    } catch (err) {
      setError(err instanceof RestError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title={`${verb} ${node}`}
      icon={<DnsGlyph size="md" />}
      onClose={onClose}
      // THE ONE WRITE ON THIS SCREEN, and its outcome is three-valued: a
      // dialog dismissed while the append is in flight leaves the operator
      // unable to tell `applied` from `pending` from `unknown`, which is the
      // whole distinction this dialog exists to render.
      dismissable={!busy}
      closeDisabledReason="Waiting for the log to acknowledge."
      size="md"
      stackBody
      footer={
        result ? (
          <Button variant="tertiary" onClick={onClose}>
            Close
          </Button>
        ) : (
          <>
            <Button variant="tertiary" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button
              variant={evict ? "danger" : "primary"}
              onClick={() => void submit()}
              disabled={busy || !confirmed}
            >
              {busy ? `${verb}ing` : verb}
            </Button>
          </>
        )
      }
    >
      {!result && (
        <>
          <p className="t-body secondary" style={{ margin: 0 }}>
            {evict ? (
              <>
                The trim stops waiting for <InlineCode>{node}</InlineCode>, so the log may advance
                past records that node never applied. It stays COUNTED for one fence window first —
                about a minute — so a node that is still running is certain to have noticed before
                its position stops holding the floor.
              </>
            ) : (
              <>
                <InlineCode>{node}</InlineCode> is counted again and the trim waits for it once
                more. This is refused when the node&apos;s own position is already below the
                published floor: there would be nothing left on the log for it to replay.
              </>
            )}
          </p>

          {evict && (
            <Callout variant="warning" role="alert">
              <span>
                Records this node never applied become deletable. Its disk still holds every row it
                did apply — an eviction is about what the FLEET waits for, not about that
                machine&apos;s data.
              </span>
            </Callout>
          )}

          <label className="col" style={{ gap: 6 }}>
            <span className="t-caption">
              Type <InlineCode>{node}</InlineCode> to confirm
            </span>
            <Input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoFocus
              spellCheck={false}
            />
          </label>

          {error && (
            <Callout variant="danger" role="alert">
              <span>{error}</span>
            </Callout>
          )}
        </>
      )}

      {result && <GateOutcome result={result} evict={evict} />}
    </Modal>
  );
}

/**
 * GateOutcome renders the three-valued write outcome, and they are three
 * visibly different states rather than one with a word in it.
 */
export function GateOutcome({ result, evict }: { result: RetentionGateResult; evict: boolean }) {
  const at = result.position?.seq;
  switch (result.outcome) {
    case "applied":
      return (
        <Callout variant="success" role="status">
          <span>
            <InlineCode>{result.node}</InlineCode> is {evict ? "evicted" : "readmitted"}, durable
            {at != null && <> at sequence {at}</>} and in this node&apos;s own rows.
          </span>
        </Callout>
      );
    case "pending":
      // THE CLOCK RATHER THAN THE TONE'S OWN MARK. Callout draws a warning
      // triangle for `warning`, and this outcome is not a fault — it is a
      // record already on the log that this node has not reached yet, so the
      // glyph that says "wait" is the honest one.
      return (
        <Callout variant="warning" role="status" icon={<ScheduleGlyph size="md" />}>
          <span className="col" style={{ gap: 6 }}>
            <span>
              <strong>Durable, not yet applied here.</strong> The record is on the log
              {at != null && <> at sequence {at}</>} and nothing about it needs retrying — this node
              has simply not caught up to it. The panel behind this dialog clears the chip when it
              has.
            </span>
            <span className="t-caption">
              Retrying would append a second record for a gesture that already landed.
            </span>
          </span>
        </Callout>
      );
    default:
      // THE ONE OUTCOME WHERE RETRYING IS CORRECT, so it is the one that
      // renders as a failure — and it carries the op id, because retrying
      // with the same one is what makes the retry idempotent.
      return (
        <Callout variant="danger" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>
              <strong>No acknowledgement.</strong> Nothing can be established about this gesture —
              it may have landed and it may not. This is the case to retry.
            </span>
            {result.op_id && (
              <span className="t-caption">
                Retry with the same operation id so a landed record is not duplicated:{" "}
                <InlineCode>{result.op_id}</InlineCode>
              </span>
            )}
          </span>
        </Callout>
      );
  }
}
