/**
 * The operator's gestures on the object store's placement map: take a data
 * node out, put it back, hold the map through planned maintenance, and release
 * the hold.
 *
 * # One compare-and-set on the map, answered whole
 *
 * Each gesture is a read of the stored map, a pure change and a
 * compare-and-set in the engine (`internal/engine/objectscontrol.go`), so the
 * answer is not "done" but whether the map NOW SAYS what was asked — `landed`
 * — and the map as it stands. A gesture that lost every race to the map's
 * maintainer is not a refusal and not a fault: nothing it asked is in the map,
 * the answer's `hint` says so, and sending it again is the remedy. A request
 * nobody answered is safe to send again too, for a reason that is not the same
 * for every gesture: an out, an in and a release the map already says change
 * nothing — the record of who made the first included — and write nothing,
 * while a hold always writes, replacing the one in force with a length counted
 * from the resend. Each dialog says which of the two it is.
 *
 * # Taking a member out is typed out; putting it back is not
 *
 * Out moves the member's whole share across the fleet, which is the first step
 * of taking a node away for good, so the node id is typed — the shape every
 * gesture on this dashboard that moves data or stops a machine takes. In moves
 * the share back and undoes exactly that, so the dialog is its confirmation.
 * The route asks for the id either way and the dialog sends it.
 *
 * # A hold states its length
 *
 * A hold pins every gone member in the map, its groups a copy short, so how
 * long is the operator's to choose from the lengths offered, never a default
 * this screen picks — and every hold ends by itself within a day.
 *
 * # A dialog is for the gesture it opened for
 *
 * The screen behind re-reads the map the moment a gesture lands, and polls it
 * besides, so any prop that describes the map changes UNDER an open dialog.
 * The hold dialog is both gestures — hold when there is none, release when
 * there is — and deriving which from the live prop turned a landed hold into a
 * "released" dialog on the re-read that followed it. So the gesture is fixed
 * when the dialog opens, and the outcome is worded from the ANSWER — whether it
 * names a hold in force — rather than from what was asked, so what the dialog
 * says always matches what the engine did.
 */

import { useState } from "react";
import { Button, Callout, InlineCode, Input, Modal, Select } from "@crewlethq/ui";
import { DatabaseGlyph, ScheduleGlyph } from "@crewlethq/icons/glyphs";
import { rest, RestError } from "~/protocol/index.ts";
import type { ObjectHold, ObjectsGestureAnswer } from "~/protocol/index.ts";
import { fmtDateTime } from "~/lib/format.ts";

/** What the last request came back with, rendered under the dialog's text. */
type Heard =
  | { kind: "answer"; answer: ObjectsGestureAnswer }
  | { kind: "refused"; detail: string; hint: string }
  | { kind: "unanswered"; why: string };

/** Sends one gesture and reads back what it did. */
async function gesture(path: string, query: Record<string, string>): Promise<Heard> {
  try {
    const answer = (await rest.request("POST", path, { body: {}, query }))
      .body as ObjectsGestureAnswer | null;
    if (!answer) return { kind: "unanswered", why: "the node answered with no body" };
    return { kind: "answer", answer };
  } catch (err) {
    if (err instanceof RestError && err.unanswered) {
      // NOT A REFUSAL: nothing here knows what the node did. Sending the
      // same gesture again is how to find out, and the dialog says what
      // that does — see the file's doc.
      return {
        kind: "unanswered",
        why:
          err.status === 0 || err.code === "unreadable_body"
            ? err.message
            : `a ${err.status} came back with no engine error code in it — something in front of the node answered`,
      };
    }
    if (err instanceof RestError) return { kind: "refused", detail: err.message, hint: err.hint };
    return { kind: "refused", detail: String(err), hint: "" };
  }
}

/** Whether what came back is a gesture the map now says. */
function landed(heard: Heard | null): boolean {
  return heard?.kind === "answer" && heard.answer.landed;
}

/** What sending a gesture again does, for one the map already says changes nothing. */
const RESEND_CHANGES_NOTHING =
  "Sending the same gesture again is safe: one the map already says changes nothing and writes nothing.";

/**
 * The outcome line under a gesture's text. `resend` says what sending the same
 * gesture again does, for a request nobody answered.
 */
function Outcome({ heard, done, resend }: { heard: Heard; done: React.ReactNode; resend: string }) {
  switch (heard.kind) {
    case "answer":
      return heard.answer.landed ? (
        <Callout variant="success" role="status">
          <span className="col" style={{ gap: 6 }}>
            <span>{done}</span>
            {heard.answer.hint && <span className="t-caption">{heard.answer.hint}</span>}
            <span className="t-caption">Placement map epoch {heard.answer.epoch}</span>
          </span>
        </Callout>
      ) : (
        <Callout variant="warning" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>
              <strong>Not in the map.</strong> {heard.answer.hint}
            </span>
          </span>
        </Callout>
      );
    case "unanswered":
      return (
        <Callout variant="warning" role="alert" icon={<ScheduleGlyph size="md" />}>
          <span>
            <strong>No answer.</strong> {heard.why}, so whether the map changed is unknown. {resend}
          </span>
        </Callout>
      );
    case "refused":
      return (
        <Callout variant="danger" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>{heard.detail}</span>
            {heard.hint && <span className="t-caption">{heard.hint}</span>}
          </span>
        </Callout>
      );
  }
}

/**
 * Taking a data node out of the placement map, or putting it back.
 *
 * `onDone` is called after every answer, so the screen behind re-reads the map
 * rather than waiting for its poll: the member's row is what says the gesture
 * happened, and it should say so now.
 */
export function ObjectsMemberDialog({
  node,
  out,
  onDone,
  onClose,
}: {
  node: string;
  /** True to take the member out, false to put it back. */
  out: boolean;
  onDone: () => void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard | null>(null);
  const verb = out ? "Take out" : "Put back";
  const confirmed = !out || typed.trim() === node;

  async function send() {
    if (busy || !confirmed) return;
    setBusy(true);
    const query: Record<string, string> = { confirm: node };
    if (out && reason.trim()) query.reason = reason.trim();
    const next = await gesture(`/objects/${out ? "out" : "in"}/${encodeURIComponent(node)}`, query);
    setHeard(next);
    setBusy(false);
    onDone();
  }

  const answer = heard?.kind === "answer" ? heard.answer : undefined;
  const done = out ? (
    <>
      <InlineCode>{node}</InlineCode> is out: nothing new is placed on it, and its share is being
      copied to the other members while it keeps serving what it holds. Keep it running until no
      member has a chunk pending and it holds no strays.
    </>
  ) : answer && !answer.member ? (
    <>
      <InlineCode>{node}</InlineCode> is no longer held back: the map places on it the next time its
      maintainer sees it present and healthy.
    </>
  ) : (
    <>
      <InlineCode>{node}</InlineCode> is back in the map: its share moves back to it.
    </>
  );

  return (
    <Modal
      open
      title={`${verb} ${node}`}
      icon={<DatabaseGlyph size="md" />}
      onClose={onClose}
      // A REQUEST IN FLIGHT HAS AN OUTCOME NOBODY HAS HEARD YET.
      dismissable={!busy}
      closeDisabledReason="Waiting for the placement map to answer."
      size="md"
      stackBody
      footer={
        <>
          <Button variant="tertiary" onClick={onClose} disabled={busy}>
            {landed(heard) ? "Close" : "Cancel"}
          </Button>
          {!landed(heard) && (
            <Button
              variant={out ? "danger" : "primary"}
              onClick={() => void send()}
              disabled={busy || !confirmed}
            >
              {busy ? "Sending" : heard ? "Send again" : verb}
            </Button>
          )}
        </>
      }
    >
      {!landed(heard) && (
        <>
          <p className="t-body secondary" style={{ margin: 0 }}>
            {out ? (
              <>
                The map stops placing on <InlineCode>{node}</InlineCode>, and its share of the
                company&apos;s files is copied to the other members while it keeps serving every
                chunk it holds — so taking a node away is a copy rather than a recovery. It stays a
                member until it is put back or is gone past the grace.
              </>
            ) : (
              <>
                The map places on <InlineCode>{node}</InlineCode> again and its share moves back to
                it. For a node the map removed for being gone, this vouches for it rather than
                waiting for it to prove itself stable: one back on probation is placed on at once,
                and one not seen since is placed on the next time it is seen.
              </>
            )}
          </p>
          {out && (
            <>
              <label className="col" style={{ gap: 6 }}>
                <span className="t-caption">
                  Type <InlineCode>{node}</InlineCode> to confirm
                </span>
                <Input
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  autoFocus
                  spellCheck={false}
                  disabled={busy}
                  aria-label={`Type ${node} to confirm`}
                />
              </label>
              <label className="col" style={{ gap: 6 }}>
                <span className="t-caption">Why — recorded on the map beside who did it</span>
                <Input
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  disabled={busy}
                  aria-label="Why"
                />
              </label>
            </>
          )}
        </>
      )}
      {heard && <Outcome heard={heard} done={done} resend={RESEND_CHANGES_NOTHING} />}
    </Modal>
  );
}

/**
 * The lengths a hold is offered at, the longest being the engine's own ceiling
 * (`upkeep.MaxHold`): a hold nobody releases must still end.
 */
export const HOLD_LENGTHS = ["30m", "1h", "2h", "4h", "8h", "24h"] as const;

/**
 * Holding the placement map through planned maintenance, or releasing a hold.
 *
 * `hold` is the hold in force AS THE DIALOG OPENS, if any: the dialog releases
 * it, and otherwise places one. READ ONCE — a later value, from the re-read
 * the dialog's own gesture sets off or from the screen's poll, changes neither
 * which gesture the dialog is for nor what "Send again" sends (see the file's
 * doc).
 */
export function ObjectsHoldDialog({
  hold,
  onDone,
  onClose,
}: {
  hold?: ObjectHold;
  onDone: () => void;
  onClose: () => void;
}) {
  const [held] = useState(hold);
  const [length, setLength] = useState<string>("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard | null>(null);
  const release = held !== undefined;
  const ready = release || length !== "";

  async function send() {
    if (busy || !ready) return;
    setBusy(true);
    const query: Record<string, string> = {};
    if (!release) {
      query.for = length;
      if (reason.trim()) query.reason = reason.trim();
    }
    const next = await gesture(release ? "/objects/release" : "/objects/hold", query);
    setHeard(next);
    setBusy(false);
    onDone();
  }

  // WORDED FROM THE ANSWER: whether the map now names a hold in force is what
  // the engine did, and the only thing this line may claim.
  const answer = heard?.kind === "answer" ? heard.answer : undefined;
  const done = answer?.hold ? (
    <>
      The map is held until {fmtDateTime(answer.hold.until)}: no member is removed however long it
      is gone.
    </>
  ) : (
    <>
      The hold is released: a member gone past its grace is removed at the map maintainer&apos;s
      next tick.
    </>
  );

  return (
    <Modal
      open
      title={release ? "Release the hold" : "Hold the placement map"}
      icon={<ScheduleGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the placement map to answer."
      size="md"
      stackBody
      footer={
        <>
          <Button variant="tertiary" onClick={onClose} disabled={busy}>
            {landed(heard) ? "Close" : "Cancel"}
          </Button>
          {!landed(heard) && (
            <Button variant="primary" onClick={() => void send()} disabled={busy || !ready}>
              {busy ? "Sending" : heard ? "Send again" : release ? "Release" : "Hold"}
            </Button>
          )}
        </>
      }
    >
      {!landed(heard) &&
        (held ? (
          <p className="t-body secondary" style={{ margin: 0 }}>
            Held until {fmtDateTime(held.until)} by <InlineCode>{held.by}</InlineCode>
            {held.reason ? <> ({held.reason})</> : null}. Released, a member that has been gone past
            the grace is removed and its share re-placed across the fleet.
          </p>
        ) : (
          <>
            <p className="t-body secondary" style={{ margin: 0 }}>
              While the map is held, no member is removed however long it is gone — for a node going
              down for maintenance and coming back, whose share would otherwise be copied away and
              then back. Its groups stay a copy short while it is gone, which is why a hold always
              ends.
            </p>
            <label className="col" style={{ gap: 6 }}>
              <span className="t-caption">For how long</span>
              <Select
                value={length}
                onChange={(value) => setLength(String(value))}
                placeholder="Choose a length"
                ariaLabel="For how long"
                disabled={busy}
                options={HOLD_LENGTHS.map((l) => ({ value: l, label: l }))}
              />
            </label>
            <label className="col" style={{ gap: 6 }}>
              <span className="t-caption">Why — recorded on the map beside who held it</span>
              <Input
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                disabled={busy}
                aria-label="Why"
              />
            </label>
          </>
        ))}
      {heard && (
        <Outcome
          heard={heard}
          done={done}
          resend={
            release
              ? RESEND_CHANGES_NOTHING
              : "Sending the hold again is safe, but it replaces the one in force: its length is counted from the resend."
          }
        />
      )}
    </Modal>
  );
}
