/**
 * Evicting a node from the state log's register, and readmitting one.
 *
 * # This is the screen's one WRITE, and the only one that can answer `pending`
 *
 * Every other panel on Fleet reads. This gesture appends a record to the log
 * and therefore has a write OUTCOME, which is three-valued (D134) and which
 * this dialog renders as three visibly different things:
 *
 *   - `applied`: durable at the returned position AND in this node's own
 *     rows. The confirmation.
 *   - `pending`: durable at the position, NOT applied here yet, so what it
 *     produced is unresolved on the node that answered. It is not a failure
 *     and it must not be retried: the record is already on the log. It renders
 *     as an in-flight state carrying the position, and the panel's next poll is
 *     what clears it.
 *   - `unknown`: no acknowledgement at all, which is the ONE outcome where
 *     retrying is correct, so it is the one that renders as a failure and
 *     carries the op id to retry with.
 *
 * **A dialog that rendered `pending` as success would be the browser half of
 * the lie the durable-versus-applied split exists to prevent**, and worse
 * here than at the tool seam, because a person believes a screen.
 *
 * # Why the eviction is not immediate, and why the dialog says so
 *
 * The node stays COUNTED for one fence window after the gesture, so a live
 * node is certain to have noticed before the trim stops waiting for it. An
 * operator who does not know that reads the unchanged watermark as a failure
 * and runs the gesture again, which is why the confirmation names the window
 * before the button is pressed rather than after.
 */

import { useState } from "react";
import { rest, RestError } from "~/protocol/index.ts";
import type { RetentionGateResult } from "~/protocol/index.ts";
import { Button, Callout, FormField, InlineCode, Input, Modal, Stack, Text } from "@crewlethq/ui";
import {
  CheckCircleGlyph,
  DnsGlyph,
  ErrorGlyph,
  ScheduleGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";

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
    // A RESULT IS A RECORD ALREADY ON THE LOG, so this refuses to send a
    // second one. `pending` above all: the outcome below tells the operator
    // that retrying appends a duplicate for a gesture that already landed, and
    // the dialog must not be the thing that does it. The frame is a form, so
    // Enter reaches here from the field as well as the button.
    if (busy || !confirmed || result) return;
    setBusy(true);
    setError(null);
    try {
      // THE TYPED CONFIRMATION TRAVELS. The server refuses unless
      // `?confirm=` repeats the node id. Checking it only in the
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
      stackBody
      title={`${verb} ${node}`}
      icon={<DnsGlyph />}
      onClose={onClose}
      // Escape, the veil and the close control all stop closing while the
      // request is in flight, so a stray press cannot abandon a write whose
      // outcome the operator has not seen.
      dismissable={!busy}
      size="md"
      /*
       * AN ALERT WHEN IT IS THE DESTRUCTIVE ONE. An eviction interrupts to ask
       * something consequential, so a reader's software is asked to announce
       * the whole surface rather than its name alone: the name says which node
       * and the body says what the fleet stops waiting for. A readmission adds
       * a node back and is an ordinary dialog.
       */
      role={evict ? "alertdialog" : "dialog"}
      onSubmit={() => void submit()}
      footer={
        result ? (
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        ) : (
          <>
            <Button variant="tertiary" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button
              type="submit"
              variant={evict ? "danger" : "primary"}
              // THE NAME HOLDS STILL while the request runs. Swapping the
              // label for "Evicting" tells a screen reader the button has been
              // renamed rather than that it is busy; `loading` says the second
              // thing, with `aria-busy`, and refuses the press either way.
              loading={busy}
              // UNAVAILABLE, AND WHY, rather than natively disabled: a
              // disabled control takes no focus, so the one sentence saying
              // what would make it pressable is unreachable by exactly the
              // reader who cannot see the field above it.
              disabledReason={confirmed ? undefined : `Type ${node} in the field above to confirm.`}
            >
              {verb}
            </Button>
          </>
        )
      }
    >
      {!result && (
        <>
          <Text tone="secondary">
            {evict ? (
              <>
                The trim stops waiting for <InlineCode>{node}</InlineCode>, so the log may advance
                past records that node never applied. It stays COUNTED for one fence window first,
                about a minute, so a node that is still running is certain to have noticed before
                its position stops holding the floor.
              </>
            ) : (
              <>
                <InlineCode>{node}</InlineCode> is counted again and the trim waits for it once
                more. This is refused when the node&apos;s own position is already below the
                published floor: there would be nothing left on the log for it to replay.
              </>
            )}
          </Text>

          {/* NOT A LIVE REGION. This is part of what the dialog opens saying
              rather than news, and an alert that was already on screen when
              the reader arrived is one they are told about as if it had just
              happened. The alertdialog role above announces it with the rest
              of the surface, which is the right moment for it. */}
          {evict && (
            <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
              <span>
                Records this node never applied become deletable. Its disk still holds every row it
                did apply: an eviction is about what the FLEET waits for, not about that
                machine&apos;s data.
              </span>
            </Callout>
          )}

          {/* The field mints its own id, which matters here: the dialog opens
              over the Fleet screen, and a label pointing at a literal id would
              name the first element in the document carrying it. */}
          <FormField
            label={
              <>
                Type <InlineCode>{node}</InlineCode> to confirm
              </>
            }
          >
            {(field) => (
              /* The first control in the body, so the surface opens on the
                 control carrying the question it asks. */
              <Input
                id={field.id}
                // A NODE ID IS READ EXACTLY, not as prose: the mono face is
                // what lets an operator compare what they typed against the
                // name in the label above it.
                appearance="reference"
                value={typed}
                onChange={(event) => setTyped(event.target.value)}
                autoComplete="off"
                spellCheck={false}
                aria-describedby={field.describedBy}
              />
            )}
          </FormField>

          {error && (
            <Callout variant="danger" role="alert" icon={<ErrorGlyph size="sm" />}>
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
        <Callout variant="success" role="status" icon={<CheckCircleGlyph size="sm" />}>
          <span>
            <InlineCode tone="inherit">{result.node}</InlineCode> is{" "}
            {evict ? "evicted" : "readmitted"}, durable
            {at != null && <> at sequence {at}</>} and in this node&apos;s own rows.
          </span>
        </Callout>
      );
    case "pending":
      return (
        <Callout variant="warning" role="status" icon={<ScheduleGlyph size="sm" />}>
          {/* A STEP ON THE SCALE, not a number of pixels: the spacing tokens
              carry the reader's density setting, which a literal gap does not.
              It is the shape the Retention screen's own callouts take. */}
          <Stack gap={2}>
            <span>
              <strong>Durable, not yet applied here.</strong> The record is on the log
              {at != null && <> at sequence {at}</>} and nothing about it needs retrying: this node
              has simply not caught up to it. The panel behind this dialog clears it when it has.
            </span>
            {/* THE ASIDE, in the caption register and its quieter ink: it is
                the instruction that follows from the message rather than the
                message, and it is how the two dialogs beside this one spell a
                second line inside a callout. */}
            <span className="t-caption">
              Retrying would append a second record for a gesture that already landed.
            </span>
          </Stack>
        </Callout>
      );
    default:
      // THE ONE OUTCOME WHERE RETRYING IS CORRECT, so it is the one that
      // renders as a failure, and it carries the op id, because retrying
      // with the same one is what makes the retry idempotent.
      return (
        <Callout variant="danger" role="alert" icon={<ErrorGlyph size="sm" />}>
          <Stack gap={2}>
            <span>
              <strong>No acknowledgement.</strong> Nothing can be established about this gesture: it
              may have landed and it may not. This is the case to retry.
            </span>
            {result.op_id && (
              <span className="t-caption">
                Retry with the same operation id so a landed record is not duplicated:{" "}
                <InlineCode tone="inherit">{result.op_id}</InlineCode>
              </span>
            )}
          </Stack>
        </Callout>
      );
  }
}
