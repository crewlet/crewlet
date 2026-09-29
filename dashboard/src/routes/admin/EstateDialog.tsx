/**
 * The operator's gestures on the estate map: take a data node out of every
 * partition's target, put it back, hold the map through planned maintenance
 * and release the hold, and move one partition's copy off one node or cancel
 * that move.
 *
 * What a gesture answers and how a dialog reads it back is `mapGesture.tsx`'s,
 * one rule for both placement maps. What is the estate map's own is below.
 *
 * # Every gesture is confirmed, and the dialog names what it confirms
 *
 * The engine asks every estate gesture for a confirmation. The ones that MOVE
 * copies — taking a node out, moving a partition off one — are typed out, the
 * shape every gesture on this dashboard that moves data takes. The ones that
 * undo that — putting a node back, cancelling a move — are confirmed by the
 * dialog, which names the node. A hold and a release name no node: they act on
 * the whole map, so the engine confirms them by the map's GENERATION, which the
 * dialog names and sends — the one it was opened over, so a map written again
 * from nothing since is refused rather than held.
 *
 * # A dialog is for the gesture it opened for
 *
 * For the object map's reason: the screen behind re-reads the map when a
 * gesture lands and polls it besides, so the hold dialog reads the hold in
 * force ONCE, as it opens, and words its outcome from the answer.
 */

import { useState } from "react";
import { Button, InlineCode, Input, Modal, Select } from "@crewlethq/ui";
import { AccountTreeGlyph, ScheduleGlyph } from "@crewlethq/icons/glyphs";
import type { EstateGestureAnswer, MapHold } from "~/protocol/index.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { HOLD_LENGTHS } from "./ObjectsDialog.tsx";
import {
  GestureOutcome,
  landed,
  RESEND_CHANGES_NOTHING,
  RESEND_REPLACES_THE_HOLD,
  sendGesture,
  type Heard,
} from "./mapGesture.tsx";

/** The map's name in every outcome's epoch line. */
const MAP = "Estate map";

/** A dialog's footer: close once it landed, else cancel and the gesture. */
function Footer({
  heard,
  busy,
  ready,
  verb,
  danger,
  onSend,
  onClose,
}: {
  heard: Heard<EstateGestureAnswer> | null;
  busy: boolean;
  ready: boolean;
  verb: string;
  danger?: boolean;
  onSend: () => void;
  onClose: () => void;
}) {
  return (
    <>
      <Button variant="tertiary" onClick={onClose} disabled={busy}>
        {landed(heard) ? "Close" : "Cancel"}
      </Button>
      {!landed(heard) && (
        <Button variant={danger ? "danger" : "primary"} onClick={onSend} disabled={busy || !ready}>
          {busy ? "Sending" : heard ? "Send again" : verb}
        </Button>
      )}
    </>
  );
}

/** The typed confirmation a gesture that moves copies asks for. */
function TypedConfirm({
  node,
  value,
  onChange,
  disabled,
}: {
  node: string;
  value: string;
  onChange: (v: string) => void;
  disabled: boolean;
}) {
  return (
    <label className="col" style={{ gap: 6 }}>
      <span className="t-caption">
        Type <InlineCode>{node}</InlineCode> to confirm
      </span>
      <Input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoFocus
        spellCheck={false}
        disabled={disabled}
        aria-label={`Type ${node} to confirm`}
      />
    </label>
  );
}

/** A reason field, recorded on the map beside who made the gesture. */
function Reason({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled: boolean;
}) {
  return (
    <label className="col" style={{ gap: 6 }}>
      <span className="t-caption">Why — recorded on the map beside who did it</span>
      <Input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        aria-label="Why"
      />
    </label>
  );
}

/**
 * Taking a data node out of the estate map, or putting it back. `onDone` is
 * called after every answer, so the screen behind re-reads the map.
 */
export function EstateMemberDialog({
  node,
  out,
  onDone,
  onClose,
}: {
  node: string;
  out: boolean;
  onDone: () => void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard<EstateGestureAnswer> | null>(null);
  const verb = out ? "Take out" : "Put back";

  async function send() {
    if (busy || (out && typed.trim() !== node)) return;
    setBusy(true);
    const query: Record<string, string> = { confirm: node };
    if (out && reason.trim()) query.reason = reason.trim();
    setHeard(
      await sendGesture<EstateGestureAnswer>(
        `/estate/${out ? "out" : "in"}/${encodeURIComponent(node)}`,
        query,
      ),
    );
    setBusy(false);
    onDone();
  }

  const answer = heard?.kind === "answer" ? heard.answer : undefined;
  const done = out ? (
    <>
      <InlineCode>{node}</InlineCode> is out: no partition&apos;s target names it, so each copy it
      holds is rebuilt on another member while it keeps serving, then released. Keep it running
      until it serves, joins and leaves nothing.
    </>
  ) : answer && !answer.member ? (
    <>
      <InlineCode>{node}</InlineCode> is no longer held back: the map places partitions on it the
      next time its maintainer sees it present and healthy.
    </>
  ) : (
    <>
      <InlineCode>{node}</InlineCode> is back in the estate map: partitions are placed on it again.
    </>
  );

  return (
    <Modal
      open
      title={`${verb} ${node}`}
      icon={<AccountTreeGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the estate map to answer."
      size="md"
      stackBody
      footer={
        <Footer
          heard={heard}
          busy={busy}
          ready={!out || typed.trim() === node}
          verb={verb}
          danger={out}
          onSend={() => void send()}
          onClose={onClose}
        />
      }
    >
      {!landed(heard) && (
        <>
          <p className="t-body secondary" style={{ margin: 0 }}>
            {out ? (
              <>
                No partition&apos;s target will name <InlineCode>{node}</InlineCode>: each copy it
                holds is rebuilt on another member while it keeps serving, and let go only once
                every copy the target names serves — so taking a node away is a copy rather than a
                recovery.
              </>
            ) : (
              <>
                Partitions may be placed on <InlineCode>{node}</InlineCode> again. For a node the
                map removed for being gone, this vouches for it rather than waiting for it to prove
                itself stable.
              </>
            )}
          </p>
          {out && (
            <>
              <TypedConfirm node={node} value={typed} onChange={setTyped} disabled={busy} />
              <Reason value={reason} onChange={setReason} disabled={busy} />
            </>
          )}
        </>
      )}
      {heard && (
        <GestureOutcome heard={heard} done={done} resend={RESEND_CHANGES_NOTHING} map={MAP} />
      )}
    </Modal>
  );
}

/**
 * Holding the estate map through planned maintenance, or releasing a hold,
 * confirmed by the map's `generation` — the one the dialog was opened over.
 * `hold` is the hold in force AS THE DIALOG OPENS: read once.
 */
export function EstateHoldDialog({
  hold,
  generation,
  onDone,
  onClose,
}: {
  hold?: MapHold;
  generation: string;
  onDone: () => void;
  onClose: () => void;
}) {
  const [held] = useState(hold);
  const [confirmed] = useState(generation);
  const [length, setLength] = useState<string>("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard<EstateGestureAnswer> | null>(null);
  const release = held !== undefined;
  const ready = release || length !== "";

  async function send() {
    if (busy || !ready) return;
    setBusy(true);
    const query: Record<string, string> = { confirm: confirmed };
    if (!release) {
      query.for = length;
      if (reason.trim()) query.reason = reason.trim();
    }
    setHeard(
      await sendGesture<EstateGestureAnswer>(release ? "/estate/release" : "/estate/hold", query),
    );
    setBusy(false);
    onDone();
  }

  // WORDED FROM THE ANSWER, for the object map's reason.
  const answer = heard?.kind === "answer" ? heard.answer : undefined;
  const done = answer?.hold ? (
    <>
      The estate map is held until {fmtDateTime(answer.hold.until)}: no member is removed however
      long it is gone.
    </>
  ) : (
    <>
      The hold is released: a member gone past its grace is removed at the map maintainer&apos;s
      next tick, and its partitions rebuilt on the others.
    </>
  );

  return (
    <Modal
      open
      title={release ? "Release the hold" : "Hold the estate map"}
      icon={<ScheduleGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the estate map to answer."
      size="md"
      stackBody
      footer={
        <Footer
          heard={heard}
          busy={busy}
          ready={ready}
          verb={release ? "Release" : "Hold"}
          onSend={() => void send()}
          onClose={onClose}
        />
      }
    >
      {!landed(heard) && (
        <>
          {held ? (
            <p className="t-body secondary" style={{ margin: 0 }}>
              Held until {fmtDateTime(held.until)} by <InlineCode>{held.by}</InlineCode>
              {held.reason ? <> ({held.reason})</> : null}. Released, a member gone past the grace
              is removed and each partition it held rebuilt on the others.
            </p>
          ) : (
            <>
              <p className="t-body secondary" style={{ margin: 0 }}>
                While the estate map is held, no member is removed however long it is gone — for a
                node going down for maintenance and coming back, whose partitions would otherwise be
                rebuilt elsewhere and then back. They stay a copy short while it is gone, which is
                why a hold always ends.
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
              <Reason value={reason} onChange={setReason} disabled={busy} />
            </>
          )}
          <p className="t-caption" style={{ margin: 0 }}>
            For the estate map of generation <InlineCode>{confirmed}</InlineCode> — the engine
            refuses it for any other.
          </p>
        </>
      )}
      {heard && (
        <GestureOutcome
          heard={heard}
          done={done}
          resend={release ? RESEND_CHANGES_NOTHING : RESEND_REPLACES_THE_HOLD}
          map={MAP}
        />
      )}
    </Modal>
  );
}

/**
 * Moving one partition's copy off one node — chosen among its holders and
 * typed out — or, with `cancel`, lifting the move of it off `node`.
 */
export function EstateMoveDialog({
  partition,
  holders,
  node: fixed,
  cancel,
  onDone,
  onClose,
}: {
  partition: string;
  /** The partition's holders, which a move may take the copy off. */
  holders: string[];
  /** The node a cancel lifts the move off. */
  node?: string;
  cancel?: boolean;
  onDone: () => void;
  onClose: () => void;
}) {
  const [from, setFrom] = useState<string>(fixed ?? "");
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard<EstateGestureAnswer> | null>(null);
  const ready = from !== "" && (cancel || typed.trim() === from);

  async function send() {
    if (busy || !ready) return;
    setBusy(true);
    const query: Record<string, string> = { from, confirm: from };
    if (!cancel && reason.trim()) query.reason = reason.trim();
    const path = `/estate/move/${encodeURIComponent(partition)}${cancel ? "/cancel" : ""}`;
    setHeard(await sendGesture<EstateGestureAnswer>(path, query));
    setBusy(false);
    onDone();
  }

  const answer = heard?.kind === "answer" ? heard.answer : undefined;
  const done = cancel ? (
    <>
      The move of {partition} off <InlineCode>{from}</InlineCode> is lifted: its target may name{" "}
      <InlineCode>{from}</InlineCode> again.
    </>
  ) : (
    <>
      {partition} moves off <InlineCode>{from}</InlineCode>: its target is now{" "}
      {(answer?.target ?? []).join(", ") || "unchanged"}, so the copy is rebuilt there and then
      released. It stays in force until it is cancelled or the node leaves the map.
    </>
  );

  return (
    <Modal
      open
      title={cancel ? `Cancel the move of ${partition}` : `Move ${partition}`}
      icon={<AccountTreeGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the estate map to answer."
      size="md"
      stackBody
      footer={
        <Footer
          heard={heard}
          busy={busy}
          ready={ready}
          verb={cancel ? "Cancel the move" : "Move"}
          danger={!cancel}
          onSend={() => void send()}
          onClose={onClose}
        />
      }
    >
      {!landed(heard) &&
        (cancel ? (
          <p className="t-body secondary" style={{ margin: 0 }}>
            The target of {partition} may name <InlineCode>{from}</InlineCode> again, and its copy
            there is rebuilt if the target wants it.
          </p>
        ) : (
          <>
            <p className="t-body secondary" style={{ margin: 0 }}>
              The target of {partition} skips the node you choose: its copy there is rebuilt on the
              member the partition&apos;s ranking offers next, and released once every copy the
              target names serves. The engine refuses it where no member is left to rebuild on.
            </p>
            <label className="col" style={{ gap: 6 }}>
              <span className="t-caption">Off which node</span>
              <Select
                value={from}
                onChange={(value) => {
                  setFrom(String(value));
                  setTyped("");
                }}
                placeholder="Choose a holder"
                ariaLabel="Off which node"
                disabled={busy}
                options={holders.map((h) => ({ value: h, label: h }))}
              />
            </label>
            {from && <TypedConfirm node={from} value={typed} onChange={setTyped} disabled={busy} />}
            <Reason value={reason} onChange={setReason} disabled={busy} />
          </>
        ))}
      {heard && (
        <GestureOutcome heard={heard} done={done} resend={RESEND_CHANGES_NOTHING} map={MAP} />
      )}
    </Modal>
  );
}
