/**
 * Evicting a node from the state log's register, and readmitting one.
 *
 * # One gesture, every identity-claiming log, and each log's own answer
 *
 * The engine judges the gesture once and then writes its record to every log
 * the trim counts nodes on — the tracker's and the pages log — and each log
 * answers on its own, with the three-valued outcome every write has (D134) or
 * the refusal that stopped it. So this dialog renders ONE ROW PER LOG, and the
 * summary above them is driven by `complete` — whether every log holds the
 * record — and by what the unfinished logs say to do:
 *
 *   - `applied` — durable at the position AND in this node's own rows.
 *   - `pending` — durable at the position, NOT applied here yet. It is not a
 *     failure and it must not be retried: the record is already on the log.
 *   - `unknown` — no acknowledgement; it may or may not be on the log, so it
 *     has NO position, and the same gesture under the same operation id is
 *     what finishes it: a log that holds the record answers from its own rows.
 *     Except where it is `unvouched`: this node's ledger may have lost the row
 *     the operation needs, so it answers the same way every time, and the log
 *     says so and sends the gesture to another node instead.
 *   - not written — a refusal, with its reason and what to do instead.
 *
 * **A dialog that rendered `pending` as success would be the browser half of
 * the lie the durable-versus-applied split exists to prevent** — and one that
 * rendered an applied gesture as "no acknowledgement", which this one did for
 * as long as it read a top-level outcome the route had stopped writing, sends
 * an operator to press again: a SECOND gesture, which writes every log that
 * already holds the record again, re-dates each eviction and restarts its
 * fence window.
 *
 * # The operation id is minted HERE, before the first request, and kept
 *
 * The node finishes a gesture under its own one-minute budget whatever happens
 * to the connection, so a request that timed out or dropped has very likely
 * done its work — and an id the route minted comes back only in the answer
 * that never arrived. So the dialog mints the id in the engine's grammar
 * ([newGateOpID]) before it asks, waits past the node's own bound
 * ([GATE_REQUEST_TIMEOUT_MS]), and offers **Finish this gesture** — the same
 * request, the same id, force carried — for a request nobody answered and for
 * a log whose remedy keeps the id ([keepsOperation]). "Nobody answered"
 * includes an answer the ENGINE did not write ([RestError.unanswered]): a
 * reverse proxy's read timeout is a minute by default, which is the node's own
 * budget, so a slow gesture reaches this page as a gateway's 504 — and read as
 * a refusal it dropped the id of a gesture the node went on to finish. The
 * gesture is lifted into the screen ([GateGesture]), so closing the dialog and
 * opening it again offers Finish rather than a fresh gesture — and so does a
 * COMPLETE one, until the report behind the dialog shows it (or, from a node
 * that has applied every record the gesture wrote, shows the node moved on
 * since): reopened as a fresh gesture, an eviction that landed a moment ago is
 * minted a new id and re-dated on every log.
 *
 * # Force is a second, separate decision
 *
 * An eviction of a node still holding a live presence lease is refused, and so
 * is one whose leases this node cannot read. The refusal's `actions` say when
 * forcing is the way past it — never a sentence telling the operator to pass a
 * flag this screen does not have — and the dialog offers **Force eviction**
 * behind its own tick and a second typed confirmation, then carries force into
 * every Finish of that gesture: a retry without it is judged again against the
 * very lease the operator overrode, and refused.
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
import { Button, Callout, Checkbox, InlineCode, Input, Modal, Tag } from "@crewlethq/ui";
import { DnsGlyph, ScheduleGlyph } from "@crewlethq/icons/glyphs";
import {
  GATE_REQUEST_TIMEOUT_MS,
  keepsOperation,
  newGateOpID,
  rest,
  RestError,
} from "~/protocol/index.ts";
import type { RetentionGateDomain, RetentionGateResult } from "~/protocol/index.ts";

/**
 * A gesture the screen is holding for one node and one sign: the operation id
 * it is finished under, whether it was forced, and the last thing it heard.
 *
 * LIFTED OUT OF THE DIALOG, because the dialog is unmounted when it closes and
 * a gesture is not: a partial eviction reopened as a fresh one is a second
 * gesture over the logs the first one reached.
 */
export interface GateGesture {
  opId: string;
  force: boolean;
  /** The last answer the engine gave, past the judgement. */
  answer?: RetentionGateResult;
  /**
   * Why the LATEST request went unanswered — timed out, dropped, or answered
   * by something in front of the node. Beside `answer` when that request was
   * a Finish: the answer before it is still what the logs last said.
   */
  unanswered?: string;
}

/**
 * Whether a held gesture is one to finish under its own operation id: a
 * request nobody answered, or an incomplete answer where some log's remedy
 * keeps the id — sent again now, through another node, or once the log has
 * room or has been re-anchored.
 */
export function finishable(g: GateGesture): boolean {
  if (g.unanswered) return true;
  if (!g.answer || g.answer.complete) return false;
  return g.answer.domains.some((d) => (d.actions ?? []).some(keepsOperation));
}

/**
 * Why a request is held as unanswered, as the clause the dialog's "No answer"
 * sentence carries: the transport's own words where nothing came back or the
 * body could not be read, and the status where something in front of the node
 * answered with a body of its own.
 */
function unansweredWhy(err: RestError): string {
  if (err.status === 0 || err.code === "unreadable_body") return err.message;
  return `a ${err.status} came back with no engine error code in it — something in front of the node answered`;
}

/** A refusal the engine answered before anything was written. */
interface Refusal {
  detail: string;
  hint: string;
  actions: string[];
}

export function GateDialog({
  node,
  evict,
  held,
  onHeld,
  onClose,
}: {
  node: string;
  /** True to evict, false to readmit. */
  evict: boolean;
  /** The gesture the screen is holding for this node and sign, if any. */
  held?: GateGesture;
  /**
   * Called with the gesture after every answer that leaves it worth holding —
   * one a request can still finish, and a complete one the screen's report may
   * not show yet — and with null for an operation no request can finish,
   * where the next gesture is rightly a new one. A refusal leaves what the
   * screen holds as it was: it wrote nothing this time.
   */
  onHeld: (gesture: GateGesture | null) => void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [gesture, setGesture] = useState<GateGesture | null>(held ?? null);
  // THE REFUSAL'S HINT AND ACTIONS TRAVEL WITH ITS DETAIL. A refused
  // readmission answers `409 readmission_refused` with the sentence in
  // `detail` and what to do in `hint` and `actions`, and a dialog that kept
  // only the message told the operator why and never what next.
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [forcing, setForcing] = useState(false);
  const [typedForce, setTypedForce] = useState("");
  // AN EARLIER GESTURE'S ID, pasted to finish it here: one started on another
  // node that can no longer write, or from the command line.
  const [earlier, setEarlier] = useState("");

  const verb = evict ? "Evict" : "Readmit";
  const sign = evict ? "evict" : "readmit";
  // THE NODE ID TYPED OUT, like every other irreversible gesture on this
  // dashboard. An eviction changes what the trim may delete on every other
  // node, so a misclick here costs a fleet its replay window.
  const confirmed = typed.trim() === node;
  const forceConfirmed = typedForce.trim() === node;
  const offersForce = evict && (refusal?.actions.includes("force") ?? false);
  const answered = gesture?.answer || gesture?.unanswered;

  /** Sends the gesture under `opId`, and records what came back. */
  async function send(opId: string, force: boolean) {
    setBusy(true);
    setRefusal(null);
    try {
      // THE TYPED CONFIRMATION TRAVELS, and so does the id and the force: the
      // server refuses unless `?confirm=` repeats the node id, a retry is the
      // same gesture only under the same `op_id`, and one without `force` is
      // judged again against the lease the operator overrode.
      const answer = (
        await rest.request("POST", `/work/retention/${sign}/${encodeURIComponent(node)}`, {
          body: {},
          query: { confirm: node, op_id: opId, force: force ? "true" : undefined },
          timeoutMs: GATE_REQUEST_TIMEOUT_MS,
        })
      ).body as RetentionGateResult;
      const next: GateGesture = { opId, force, answer };
      setGesture(next);
      // THE FORCE DECISION IS SPENT, and the gesture carries it into every
      // Finish: left ticked, the footer kept offering "Force eviction" over
      // an answer that needs Finish instead.
      setForcing(false);
      setTypedForce("");
      // A COMPLETE GESTURE IS HELD TOO, until the report shows it: the row
      // behind this dialog reads the report, which a poll refreshes, and in
      // between it still offered the gesture just made — reopened, a new id
      // and a second record on every log.
      onHeld(answer.complete || finishable(next) ? next : null);
    } catch (err) {
      if (err instanceof RestError && err.unanswered) {
        // NO ANSWER IS NOT A REFUSAL, and neither is one the engine did not
        // write — a gateway's 504, a 200 cut off part way through: the node
        // finishes a gesture whatever happens to the connection, so it may
        // have reached every log. The id is kept, and Finish asks again
        // under it.
        //
        // AND SO IS WHAT THE GESTURE ALREADY HEARD, for the reason a refused
        // Finish keeps it: a Finish nobody answered says nothing about the
        // logs the earlier request reached, and dropping their answer took
        // the per-log outcomes off the screen while a refusal left them on.
        const heard = gesture?.opId === opId ? gesture.answer : undefined;
        const next: GateGesture = {
          opId,
          force,
          unanswered: unansweredWhy(err),
          ...(heard ? { answer: heard } : {}),
        };
        setGesture(next);
        setForcing(false);
        setTypedForce("");
        onHeld(next);
      } else {
        // A REFUSAL WROTE NOTHING THIS TIME — which is all it says. On a
        // gesture's first request that means its id names no record
        // anywhere and the next attempt may reuse it; on a FINISH the logs
        // the gesture already reached still hold its record, and the
        // judgement a Finish re-runs (`503 eviction_unjudged`, `409
        // readmission_refused`) says nothing about them. So what the gesture
        // already heard is KEPT and the refusal renders beside it: dropped,
        // the dialog fell back to "Type node-4 to confirm" with the per-log
        // answer and the operation id gone from the screen.
        //
        // AN ID PASTED TO FINISH A GESTURE STARTED ELSEWHERE IS NOT TAKEN UP
        // on a refusal before anything answered: the field still shows it to
        // correct, and held it was re-sent by every submit — the field
        // cleared included — refused `op_id_invalid` each time until the
        // dialog was closed.
        const pasted = !(gesture?.answer || gesture?.unanswered) && opId === earlier.trim();
        setGesture((prev) =>
          pasted ? prev : prev?.opId === opId ? { ...prev, force } : { opId, force },
        );
        setRefusal(
          err instanceof RestError
            ? {
                detail: err.message,
                hint: err.hint,
                actions: Array.isArray(err.body.actions)
                  ? err.body.actions.filter((a): a is string => typeof a === "string")
                  : [],
              }
            : { detail: String(err), hint: "", actions: [] },
        );
      }
    } finally {
      setBusy(false);
    }
  }

  function submit() {
    if (busy) return;
    if (forcing) {
      if (!forceConfirmed) return;
      void send(nextOpId(), true);
      return;
    }
    if (!confirmed) return;
    void send(nextOpId(), false);
  }

  /**
   * The operation the next request goes under: an answered gesture's own, and
   * before anything answered the id pasted to finish one started elsewhere,
   * then the one this dialog minted for an earlier refused attempt, then a
   * fresh one.
   */
  function nextOpId(): string {
    if (gesture && answered) return gesture.opId;
    return earlier.trim() || gesture?.opId || newGateOpID(sign, node);
  }

  function finish() {
    if (busy || !gesture) return;
    void send(gesture.opId, gesture.force);
  }

  /** Lets go of this gesture, for an operation no request can finish. */
  function startAfresh() {
    setGesture(null);
    setRefusal(null);
    setForcing(false);
    setTyped("");
    setTypedForce("");
    setEarlier("");
    onHeld(null);
  }

  const canFinish = gesture ? finishable(gesture) : false;
  const endsOperation =
    gesture?.answer?.domains.some((d) => d.actions?.includes("new_gesture")) ?? false;

  return (
    <Modal
      open
      title={`${verb} ${node}`}
      icon={<DnsGlyph size="md" />}
      onClose={onClose}
      // A REQUEST IN FLIGHT HAS AN OUTCOME NOBODY HAS HEARD YET, and a dialog
      // dismissed now leaves the operator unable to tell `applied` from
      // `pending` from `unknown`. The gesture itself is lifted into the screen,
      // so what IS heard survives a close.
      dismissable={!busy}
      closeDisabledReason="Waiting for the logs to acknowledge."
      size="md"
      stackBody
      footer={
        <>
          <Button variant="tertiary" onClick={onClose} disabled={busy}>
            {answered ? "Close" : "Cancel"}
          </Button>
          {forcing ? (
            <Button variant="danger" onClick={submit} disabled={busy || !forceConfirmed}>
              {busy ? "Forcing" : "Force eviction"}
            </Button>
          ) : answered ? (
            <>
              {endsOperation && !canFinish && (
                <Button variant="secondary" onClick={startAfresh} disabled={busy}>
                  Start a new gesture
                </Button>
              )}
              {canFinish && (
                <Button variant="primary" onClick={finish} disabled={busy}>
                  {busy ? "Finishing" : "Finish this gesture"}
                </Button>
              )}
            </>
          ) : (
            <Button
              variant={evict ? "danger" : "primary"}
              onClick={submit}
              disabled={busy || !confirmed}
            >
              {busy ? `${verb}ing` : verb}
            </Button>
          )}
        </>
      }
    >
      {!answered && (
        <>
          <p className="t-body secondary" style={{ margin: 0 }}>
            {evict ? (
              <>
                The trim stops waiting for <InlineCode>{node}</InlineCode>, so the log may advance
                past records that node never applied. It stays COUNTED for one fence window first —
                about a minute — so a node that is still running is certain to have noticed before
                its position stops holding the floor. A node that still holds a live presence lease
                is refused, with nothing written: it is still reaching the fleet, and forcing it is
                a separate decision.
              </>
            ) : (
              <>
                <InlineCode>{node}</InlineCode> is counted again and the trim waits for it once
                more. This is refused, with nothing written, until the node has applied every record
                up to the one just before the higher of each log&apos;s published floor and its
                first surviving sequence, in the tracker&apos;s log and the pages log: counting a
                node the floor has passed would put back the pin the eviction lifted. A refused node
                catches up on its own — replaying what the log still holds, adopting a snapshot
                where it does not — and can be readmitted once it has.
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
              disabled={busy || forcing}
              aria-label={`Type ${node} to confirm`}
            />
          </label>

          {/* FINISHING A GESTURE STARTED ELSEWHERE. A node that is itself
              evicted cannot write, so its unfinished gesture is finished
              through another node — under the SAME id, which is how every log
              that already holds the record answers from its own rows. SHOWN
              FOR AS LONG AS THIS FORM IS, a refusal included: hidden after
              one, a pasted id the route refused could not be corrected. */}
          <details>
            <summary className="t-caption">Finish a gesture started elsewhere</summary>
            <label className="col" style={{ gap: 6, marginTop: 8 }}>
              <span className="t-caption">
                Its operation id, exactly as it was answered — a fresh one would be a second gesture
              </span>
              <Input
                value={earlier}
                onChange={(e) => setEarlier(e.target.value)}
                spellCheck={false}
                disabled={busy}
                aria-label="Operation id of the earlier gesture"
              />
            </label>
          </details>
        </>
      )}

      {gesture?.unanswered && (
        <Callout variant="warning" role="alert" icon={<ScheduleGlyph size="md" />}>
          <span className="col" style={{ gap: 6 }}>
            <span>
              <strong>No answer.</strong> {gesture.unanswered}, so what the{" "}
              {evict ? "eviction" : "readmission"} did is unknown — it may have reached every log:
              the node finishes a gesture whatever happens to the connection. Finish it under the
              same operation id to read every log&apos;s answer; a log that already holds the record
              answers from its own rows and is not written twice.
            </span>
            <span className="t-caption">
              Operation <InlineCode>{gesture.opId}</InlineCode>
              {gesture.force && <> · forced</>}
            </span>
          </span>
        </Callout>
      )}

      {gesture?.answer && (
        <>
          {gesture.unanswered && (
            <span className="t-caption">
              What the logs answered before the Finish that went unanswered:
            </span>
          )}
          <GateOutcome result={gesture.answer} evict={evict} />
        </>
      )}

      {/* A REFUSAL RENDERS WHEREVER IT ARRIVED — beside the answer a gesture
          already had when it was a Finish that was refused, since that answer
          is still true, and with its own remedies either way. */}
      {refusal && (
        <Callout variant="danger" role="alert">
          <span className="col" style={{ gap: 6 }}>
            {answered && <strong>Finishing it was refused, and wrote nothing.</strong>}
            <span>{refusal.detail}</span>
            {refusal.hint && <span className="t-caption">{refusal.hint}</span>}
            {refusal.actions
              .filter((a) => a !== "force")
              .map((a) => (
                <span key={a} className="t-caption">
                  {actionWords(a, { evict, stream: "", refused: true })}
                </span>
              ))}
          </span>
        </Callout>
      )}
      {/* FORCE, OFFERED ONLY WHERE THE REFUSAL SAYS IT IS THE WAY PAST IT,
          and behind its own tick and its own typed confirmation: it is the
          one gesture here nobody's evidence cleared. */}
      {offersForce && (
        <>
          <Checkbox
            framed
            tone="danger"
            checked={forcing}
            disabled={busy}
            onCheckedChange={setForcing}
            label="Force the eviction"
            description={
              <>
                Evict <InlineCode>{node}</InlineCode> past the presence-lease judgement — for a node
                wedged in a way that still renews its lease, or one this node cannot see because
                coordination is unreachable. Only when you know it is gone: its records stop
                applying everywhere and its seats move.
              </>
            }
          />
          {forcing && (
            <label className="col" style={{ gap: 6 }}>
              <span className="t-caption">
                Type <InlineCode>{node}</InlineCode> again to force it
              </span>
              <Input
                value={typedForce}
                onChange={(e) => setTypedForce(e.target.value)}
                spellCheck={false}
                disabled={busy}
                aria-label={`Type ${node} again to force it`}
              />
            </label>
          )}
        </>
      )}
    </Modal>
  );
}

/**
 * GateOutcome renders one gesture's answer: a summary driven by `complete` and
 * by what the unfinished logs say to do, then one row per log.
 */
export function GateOutcome({ result, evict }: { result: RetentionGateResult; evict: boolean }) {
  const done = evict ? "evicted" : "readmitted";
  const unfinished = result.domains.filter((d) => !holds(d));
  const pending = result.domains.filter((d) => d.outcome === "pending");
  const retryNow = unfinished.some((d) => d.actions?.includes("retry_same_op"));
  const keeps = unfinished.some((d) => (d.actions ?? []).some(keepsOperation));

  let summary;
  if (result.complete && pending.length === 0) {
    summary = (
      <Callout variant="success" role="status">
        <span>
          <InlineCode>{result.node}</InlineCode> is {done} on every log, durable and in this
          node&apos;s own rows.
        </span>
      </Callout>
    );
  } else if (result.complete) {
    // THE CLOCK RATHER THAN THE TONE'S OWN MARK. Callout draws a warning
    // triangle for `warning`, and this outcome is not a fault — it is a
    // record already on the log that this node has not reached yet, so the
    // glyph that says "wait" is the honest one.
    summary = (
      <Callout variant="warning" role="status" icon={<ScheduleGlyph size="md" />}>
        <span className="col" style={{ gap: 6 }}>
          <span>
            <strong>Durable on every log, not yet applied here</strong> on{" "}
            {pending.map((d) => d.domain).join(" and ")}. Nothing about it needs retrying — this
            node has simply not caught up to the record. The panel behind this dialog clears it when
            it has.
          </span>
          <span className="t-caption">
            Do not retry: a fresh gesture appends a second record for one that already landed.
          </span>
        </span>
      </Callout>
    );
  } else if (retryNow) {
    summary = (
      <Callout variant="danger" role="alert">
        <span>
          <strong>Not finished</strong> — {result.domains.length - unfinished.length} of{" "}
          {result.domains.length} logs hold the record. Finish it under the same operation id: a log
          that already holds the record answers from its own rows and is not written twice.
        </span>
      </Callout>
    );
  } else if (keeps) {
    summary = (
      <Callout variant="warning" role="alert">
        <span>
          <strong>Not finished, and sending it again now cannot finish it.</strong> Do what each log
          below names first, then finish this gesture under the same operation id — a fresh gesture
          would write the logs that already hold the record again.
        </span>
      </Callout>
    );
  } else {
    summary = (
      <Callout variant="warning" role="alert">
        <span>
          <strong>Not finished, and this operation cannot be.</strong> Each log below says what
          needs a different remedy.
        </span>
      </Callout>
    );
  }

  return (
    <div className="col gap-2">
      {summary}
      <ul className="col gap-2" style={{ margin: 0, paddingLeft: 0, listStyle: "none" }}>
        {result.domains.map((d) => (
          <li key={d.domain} className="col" style={{ gap: 4 }}>
            <span className="row wrap gap-1 baseline">
              <InlineCode>{d.domain}</InlineCode>
              <DomainAnswer d={d} />
            </span>
            {!holds(d) && d.hint && <span className="t-caption">{d.hint}</span>}
            {!holds(d) &&
              (d.actions ?? []).map((a) => (
                <span key={a} className="t-caption">
                  {actionWords(a, { evict, stream: d.stream, opId: result.op_id })}
                </span>
              ))}
          </li>
        ))}
      </ul>
      <span className="t-caption">
        Operation <InlineCode>{result.op_id}</InlineCode>
      </span>
    </div>
  );
}

/** Whether a log holds the record durably. */
function holds(d: RetentionGateDomain): boolean {
  return !d.error && (d.outcome === "applied" || d.outcome === "pending");
}

/**
 * One log's answer. `unknown` carries NO position, never "sequence 0": a zero
 * would read as a record at the log's origin, and the gesture may never have
 * reached the log at all.
 */
function DomainAnswer({ d }: { d: RetentionGateDomain }) {
  if (d.error) {
    return (
      <span className="t-caption">
        not written{d.reason && <> ({d.reason})</>} — {d.error}
      </span>
    );
  }
  if (d.outcome === "unknown" && d.unvouched) {
    return (
      <span className="t-caption">
        <Tag variant="danger">unknown</Tag> this node cannot tell whether the record is on the log
      </span>
    );
  }
  if (d.outcome === "unknown" || !d.position) {
    return (
      <span className="t-caption">
        <Tag variant="danger">{d.outcome ?? "no outcome"}</Tag> the record may or may not be on the
        log
      </span>
    );
  }
  return (
    <span className="t-caption">
      <Tag variant={d.outcome === "applied" ? "success" : "warning"}>{d.outcome}</Tag> at{" "}
      {d.position.stream} {d.position.seq}
    </span>
  );
}

/**
 * What this screen says to do for one of the engine's remedy actions — its own
 * controls where it has them, and the command where it has none.
 *
 * An action this build does not know renders as its name: the engine's `hint`
 * above it still says what it is, and dropping it would hide a remedy a newer
 * node sent.
 */
function actionWords(
  action: string,
  {
    evict,
    stream,
    opId,
    refused = false,
  }: { evict: boolean; stream: string; opId?: string; refused?: boolean },
): string {
  switch (action) {
    case "retry_same_op":
      // A REFUSAL WROTE NOTHING, so "the same gesture" is the dialog's own
      // button rather than a Finish: there is no answer yet to finish.
      return refused
        ? "Send it again once what stopped it has cleared — nothing was written."
        : "Finish this gesture sends it again under the same operation id.";
    case "new_gesture":
      return "This operation cannot be finished: start a new gesture if the node should still change.";
    case "force":
      return "Force the eviction, if you know the node is gone.";
    case "other_node":
      return `Open this dashboard on a node the fleet still counts and finish it there with the same operation id${
        opId ? ` (${opId})` : ""
      }.`;
    case "reanchor":
      return `Re-anchor ${stream} with \`crewlet retention reanchor -stream ${stream}\` — there is no control for it here — then finish this gesture.`;
    case "set_capacity":
      return `Raise ${stream}'s ceiling with \`crewlet retention set-capacity\` — there is no control for it here — then finish this gesture.`;
    case "restore":
      return "Restore the store and the stream from one backup.";
    case "wait":
      return evict
        ? "Wait for its presence lease to lapse — its row stops showing it live — then evict it again."
        : "Wait for it to catch up — its position on this screen says when — then readmit it again.";
  }
  return `The engine also names ${action}.`;
}
