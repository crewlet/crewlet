/**
 * The run-time controls on a seat, for one seat or many: Pause, Pause and
 * stop, Resume and Steer, and the remedy a seat's state asks for (Answer its
 * executor, Review a held call, Raise its budget). Drawn on Structure's table
 * rows, in its selection bar, in the seat panel and on the seat's page, so a
 * seat is controlled the same way wherever it is met.
 *
 * THE ENGINE'S OWN CONTROLS, AND ONLY THEM. Pause is `pause_seat`, and the
 * running turn finishes first; Pause and stop is `pause_seat` with
 * `stop_running`, which ends the turn at its next step and its executor at
 * once, so it always asks first; Resume is `resume_seat`, one press for one
 * seat. "Stopped" stays the word for a fault, never for what a person did.
 * The engine admits an admin or a lead above the seat (`viewer.acts`, then
 * the seat itself on each press), so a control the reader may not use is
 * drawn held with its reason, never hidden.
 *
 * MANY SEATS ARE ONE QUESTION AND ONE ANSWER. The engine pauses a seat at a
 * time, so a selection sends one press per seat it changes, and says once
 * what came of all of them: every seat, or how many and which ones were not
 * changed and why. Stopping at the first refusal left the reader guessing
 * which of the rest had been sent.
 */

import { useState, type ReactNode } from "react";
import {
  Button,
  Checkbox,
  FormField,
  IconButton,
  Input,
  Menu,
  Modal,
  SelectionBar,
  useToast,
} from "@crewlethq/ui";
import {
  CircleStopGlyph,
  CompassGlyph,
  EllipsisGlyph,
  PauseGlyph,
  RotateCwGlyph,
} from "@crewlethq/icons/glyphs";
import { usePeekControls } from "~/app/frame/peekAddress.ts";
import { PAUSE_REASON_MAX_RUNES } from "~/contract/actions.ts";
import { useToastClearOf } from "~/app/toastInset.tsx";
import { waitedOn } from "~/lib/budget.ts";
import { plural, possessive } from "~/lib/format.ts";
import type { SeatLive } from "~/lib/seatLive.ts";
import { useAct } from "~/lib/useAct.ts";
import { useWriteAccess, type WriteAccess } from "~/lib/useWriteAccess.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { RaiseBudgetButton } from "./budgetWrite.tsx";
import { RefusalNote, WriteButton, pressable } from "./WriteButton.tsx";
import { AnswerRunButton, SteerTurnDialog } from "./writes.tsx";

export type SeatControl = "pause" | "stop" | "resume";

/** Whether a seat is on a turn, or its executor is: what Pause and stop ends. */
export function isRunning(s: SeatLive): boolean {
  return s.state === "working" || s.state === "needs";
}

/** The seats a control would change: the ones the reader may control, in the state it applies to. */
export function appliesTo(kind: SeatControl, seats: readonly SeatLive[]): SeatLive[] {
  return seats.filter((s) => {
    if (!s.controllable) return false;
    if (kind === "resume") return s.paused;
    // A PAUSED SEAT WHOSE TURN STILL RUNS can be stopped: a second pause amends the first.
    if (kind === "stop") return isRunning(s);
    return !s.paused;
  });
}

/**
 * Why a seat's controls are held: the frame's words for offline and the like,
 * and the rule's own for a reader the engine does not let control it. Pausing
 * needs no app, so the frame calls such a reader "not served".
 */
export function controlHeld(access: WriteAccess, s: SeatLive): string | undefined {
  return (
    accessHeldReason(access) ??
    (s.controllable ? undefined : "Only an admin or a lead above this seat can control it.")
  );
}

/** Why a reader's access holds a seat control, in the rule's words where the frame has none. */
export function accessHeldReason(access: WriteAccess): string | undefined {
  if (access.can) return undefined;
  return access.block === "not_served" || access.block === "no_access"
    ? "Pausing a seat needs Admin access, or leading it."
    : access.reason;
}

const TITLES: Readonly<Record<SeatControl, (name: string, one: boolean) => string>> = {
  pause: (name) => `Pause ${name}`,
  stop: (name, one) =>
    one ? `Pause ${name} and stop its turn` : `Pause ${name} and stop their turns`,
  resume: (name) => `Resume ${name}`,
};

function nameOfSet(seats: readonly SeatLive[]): string {
  return seats.length === 1 ? seats[0]!.seat.name : plural(seats.length, "agent");
}

/** What a control did, said once for every seat it was sent to. */
export function controlOutcome(
  kind: SeatControl,
  stop: boolean,
  changed: readonly SeatLive[],
  refused: readonly { s: SeatLive; why: string }[],
): { variant: "success" | "warning"; title: string; message?: string } {
  const all = changed.length + refused.length;
  const n = changed.length;
  // ONE SEAT BY ITS NAME; SOME OF MANY AS "2 of 3 agents".
  const who =
    all === 1 && n === 1
      ? changed[0]!.seat.name
      : n === all
        ? plural(n, "agent")
        : `${n} of ${plural(all, "agent")}`;
  const did =
    kind === "resume"
      ? `Resumed ${who}`
      : stop
        ? `Paused ${who} and stopped ${n === 1 ? "its turn" : "their turns"}`
        : `Paused ${who}`;
  if (refused.length === 0) {
    // A PAUSE WAITS FOR A RUNNING TURN, and the toast says so for one seat that is on one.
    const waits = kind === "pause" && !stop && n === 1 && isRunning(changed[0]!);
    return { variant: "success", title: waits ? `${who} pauses once its turn finishes` : did };
  }
  return {
    variant: "warning",
    title: n === 0 ? `Nothing was ${kind === "resume" ? "resumed" : "paused"}` : did,
    message: refused.map(({ s, why }) => `${s.seat.name}: ${why}`).join(" "),
  };
}

/**
 * The confirmation for a pause, a pause and stop, or a resume of many seats,
 * over the seats it applies to. A pause asks why, and offers to stop a turn
 * that is running.
 */
export function SeatControlDialog({
  kind,
  seats,
  onClose,
}: {
  kind: SeatControl;
  seats: readonly SeatLive[];
  onClose: () => void;
}) {
  const pause = useAct("pause_seat");
  const resume = useAct("resume_seat");
  const toast = useToast();
  const write = kind === "resume" ? resume : pause;
  const [reason, setReason] = useState("");
  const [alsoStop, setAlsoStop] = useState(false);
  const [busy, setBusy] = useState(false);
  const running = seats.some(isRunning);
  const one = seats.length === 1;
  const label = nameOfSet(seats);
  const long = [...reason.trim()].length > PAUSE_REASON_MAX_RUNES;
  const tooLong = long
    ? `Keep the reason to ${PAUSE_REASON_MAX_RUNES} characters or fewer.`
    : undefined;

  const submit = async () => {
    if (!pressable(write, tooLong) || busy || seats.length === 0) return;
    setBusy(true);
    const stop = kind === "stop" || alsoStop;
    const changed: SeatLive[] = [];
    const refused: { s: SeatLive; why: string }[] = [];
    let lastRefused = false;
    for (const s of seats) {
      const handle = s.seat.handle;
      // QUIET: the one toast below says what came of every seat.
      const result =
        kind === "resume"
          ? await resume.run({ handle }, { done: "", quiet: true })
          : await pause.run(
              {
                handle,
                ...(reason.trim() ? { reason: reason.trim() } : {}),
                // SENT ONLY WHEN CHOSEN: absent is the engine's default, the turn finishing first.
                ...(stop ? { stop_running: true } : {}),
              },
              { done: "", quiet: true },
            );
      lastRefused = result?.kind === "refused";
      if (result?.kind === "applied" || result?.kind === "pending") changed.push(s);
      else if (result?.kind === "refused") refused.push({ s, why: result.sentence });
      else if (result?.kind === "unknown")
        refused.push({ s, why: `Not confirmed. ${result.reason}` });
      else refused.push({ s, why: "Not sent." });
    }
    setBusy(false);
    // ONE SEAT REFUSED STAYS HERE, its refusal beside the control and the reason still typed.
    if (one && lastRefused) return;
    write.dismiss();
    const said = controlOutcome(kind, stop, changed, refused);
    toast.show({
      id: one ? `seat-control-${seats[0]!.seat.handle}` : "seat-control-selection",
      variant: said.variant,
      title: said.title,
      ...(said.message ? { message: said.message } : {}),
    });
    onClose();
  };

  const body =
    kind === "resume"
      ? `${label} ${one ? "picks" : "pick"} up new work again. What waited is delivered first, in order.`
      : kind === "stop"
        ? `The running ${one ? "turn ends" : "turns end"} at the next step, and ${one ? "its executor ends" : "their executors end"} at once. Unfinished work is lost and is not run again. Nothing is pushed until resumed.`
        : `${label} ${one ? "takes" : "take"} no new turns until resumed, and scheduled runs are skipped. A turn already running finishes first.`;

  return (
    <Modal
      open
      size="md"
      title={TITLES[kind](label, one)}
      icon={
        kind === "resume" ? (
          <RotateCwGlyph />
        ) : kind === "stop" ? (
          <CircleStopGlyph />
        ) : (
          <PauseGlyph />
        )
      }
      onClose={onClose}
      onSubmit={() => void submit()}
      closeDisabledReason={busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant={kind === "stop" ? "danger" : "primary"}
            pressing={busy}
            blocked={tooLong}
            showRefusal={false}
            onPress={() => void submit()}
          >
            {kind === "resume" ? "Resume" : kind === "stop" ? "Pause and stop" : "Pause"}
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <p className="t-body">{body}</p>
        {!one && (
          <ul className="seat-control-seats" aria-label="Agents this applies to">
            {seats.map((s) => (
              <li key={s.seat.handle}>
                <SeatAvatar
                  name={s.seat.name}
                  kind="agent"
                  avatar={s.seat.avatar}
                  size="xs"
                  decorative
                />
                <span>{s.seat.name}</span>
              </li>
            ))}
          </ul>
        )}
        {kind !== "resume" && (
          <FormField
            label="Why"
            optional
            htmlFor="seat-control-reason"
            helper="One line, shown on the seat and in the feed."
            {...(long
              ? { error: `Keep the reason to ${PAUSE_REASON_MAX_RUNES} characters or fewer.` }
              : {})}
          >
            <Input
              id="seat-control-reason"
              value={reason}
              aria-invalid={long || undefined}
              onChange={(event) => setReason(event.target.value)}
            />
          </FormField>
        )}
        {kind === "pause" && running && (
          <Checkbox
            checked={alsoStop}
            onChange={(event) => setAlsoStop(event.target.checked)}
            label={one ? "Also stop the running turn" : "Also stop the running turns"}
            description="Otherwise a running turn finishes first. A stop also ends its executor at once. Unfinished work is lost."
          />
        )}
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/** What a screen keeps to control seats: which dialog is open, and the one press that needs none. */
export interface SeatControls {
  open: (kind: SeatControl, seats: readonly SeatLive[]) => void;
  steer: (s: SeatLive) => void;
  /**
   * Draws a dialog a control handed over (`dialogPresenter.tsx`), in the
   * screen's `dialog` rather than inside the table the control sits in.
   */
  present: (dialog: ReactNode | null) => void;
  /** The open dialog, for the screen to draw. */
  dialog: ReactNode;
}

/**
 * The controls' dialogs, held as the screen's own state. Resuming one seat is
 * one press, with its toast; every other control asks first, and a control
 * that applies to none of the seats does nothing.
 */
export function useSeatControls(): SeatControls {
  const [shown, setShown] = useState<{ kind: SeatControl; seats: SeatLive[] } | null>(null);
  const [steering, setSteering] = useState<SeatLive | null>(null);
  const [presented, setPresented] = useState<ReactNode | null>(null);
  const steerWrite = useAct("steer_turn");
  const resume = useAct("resume_seat");
  const toast = useToast();
  return {
    open: (kind, seats) => {
      const apply = appliesTo(kind, seats);
      if (apply.length === 0) return;
      if (kind === "resume" && apply.length === 1) {
        // ONE PRESS, AND ITS ANSWER SAID: a refusal has no dialog to stand in, so it is the toast.
        const only = apply[0]!;
        void resume.run({ handle: only.seat.handle }, { done: "", quiet: true }).then((result) => {
          if (result === null) return;
          const ok = result.kind === "applied" || result.kind === "pending";
          const why =
            result.kind === "refused"
              ? result.sentence
              : result.kind === "unknown"
                ? `Not confirmed. ${result.reason}`
                : "";
          const said = controlOutcome(
            "resume",
            false,
            ok ? [only] : [],
            ok ? [] : [{ s: only, why }],
          );
          resume.dismiss();
          toast.show({
            id: `seat-control-${only.seat.handle}`,
            variant: said.variant,
            title: said.title,
            ...(said.message ? { message: said.message } : {}),
          });
        });
        return;
      }
      setShown({ kind, seats: apply });
    },
    steer: (s) => setSteering(s),
    present: setPresented,
    dialog: (
      <>
        {presented}
        {shown && (
          <SeatControlDialog kind={shown.kind} seats={shown.seats} onClose={() => setShown(null)} />
        )}
        {steering && steering.row?.turn && (
          <SteerTurnDialog
            turnId={steering.row.turn.turn_id}
            seat={steering.seat.name}
            write={steerWrite}
            onClose={() => {
              steerWrite.dismiss();
              setSteering(null);
            }}
          />
        )}
      </>
    ),
  };
}

/**
 * A seat's own controls: Pause or Resume, Pause and stop while it runs, and
 * Steer on a turn that has not ended. `labelled` draws words (the panel, the
 * page), otherwise icons with their names in a tooltip (a table row);
 * `compact` keeps Pause or Resume alone, for a row whose menu holds the rest.
 */
export function SeatControlButtons({
  s,
  controls,
  labelled = false,
  compact = false,
}: {
  s: SeatLive;
  controls: SeatControls;
  labelled?: boolean;
  compact?: boolean;
}) {
  const pauseAccess = useWriteAccess("pause_seat");
  const resumeAccess = useWriteAccess("resume_seat");
  const steerAccess = useWriteAccess("steer_turn");
  if (s.seat.kind === "human") return null;
  // EACH CONTROL BY ITS OWN TOOL'S ACCESS, and the seat's own rule for the reader.
  const held = controlHeld(s.paused ? resumeAccess : pauseAccess, s);
  const stopHeld =
    controlHeld(pauseAccess, s) ?? (isRunning(s) ? undefined : "It is not on a turn.");
  const steerHeld = controlHeld(steerAccess, s);
  // A TURN THAT HAS NOT ENDED takes a note; a reader who may not steer it sees Steer held.
  const steers = !!s.row?.turn;
  const name = s.seat.name;
  const toggle = s.paused
    ? {
        kind: "resume" as const,
        word: "Resume",
        aria: `Resume ${name}`,
        icon: <RotateCwGlyph size={labelled ? "sm" : undefined} />,
      }
    : {
        kind: "pause" as const,
        word: "Pause",
        aria: `Pause ${name}`,
        icon: <PauseGlyph size={labelled ? "sm" : undefined} />,
      };
  // A LABELLED CONTROL'S NAME STARTS WITH ITS VISIBLE WORDS, then names the seat.
  if (labelled) {
    return (
      <span className="seat-controls">
        <Button
          tooltip
          size="small"
          variant="secondary"
          leadingIcon={toggle.icon}
          disabledReason={held}
          aria-label={toggle.aria}
          onClick={() => controls.open(toggle.kind, [s])}
        >
          {toggle.word}
        </Button>
        {!compact && isRunning(s) && (
          <Button
            tooltip
            size="small"
            variant="secondary"
            leadingIcon={<CircleStopGlyph size="sm" />}
            disabledReason={stopHeld}
            aria-label={`Pause and stop ${possessive(name)} turn`}
            onClick={() => controls.open("stop", [s])}
          >
            Pause and stop
          </Button>
        )}
        {!compact && steers && (
          <Button
            tooltip
            size="small"
            variant="secondary"
            leadingIcon={<CompassGlyph size="sm" />}
            disabledReason={steerHeld}
            aria-label={`Steer ${possessive(name)} turn`}
            onClick={() => controls.steer(s)}
          >
            Steer
          </Button>
        )}
      </span>
    );
  }
  return (
    <span className="seat-controls">
      <IconButton
        tooltip
        size="sm"
        variant="ghost"
        label={toggle.aria}
        icon={toggle.icon}
        disabledReason={held}
        onClick={() => controls.open(toggle.kind, [s])}
      />
      {!compact && (
        <IconButton
          tooltip
          size="sm"
          variant="ghost"
          label={`Pause ${name} and stop its turn`}
          icon={<CircleStopGlyph />}
          disabledReason={stopHeld}
          onClick={() => controls.open("stop", [s])}
        />
      )}
      {!compact && steers && (
        <IconButton
          tooltip
          size="sm"
          variant="ghost"
          label={`Steer ${possessive(name)} turn`}
          icon={<CompassGlyph />}
          disabledReason={steerHeld}
          onClick={() => controls.steer(s)}
        />
      )}
    </span>
  );
}

/**
 * The remedy a table row offers beside Pause: review a held call, or raise a
 * spent budget. A plain question is not one: answering stays in the seat panel
 * and the inbox, so a row's controls say the same thing in every state.
 */
export function rowRemedy(s: SeatLive): string | null {
  if (s.ask?.held_call) return "Review call";
  if (!s.ask && s.row?.stopped_reason === "budget") return "Raise budget";
  return null;
}

/**
 * What a seat's state asks of a person: answer its executor, allow or deny
 * the call it holds, or raise its spent ceiling. A row too narrow for a held
 * call's three answers offers one button to the seat panel that holds them.
 */
export function SeatRemedy({ s, compact = false }: { s: SeatLive; compact?: boolean }) {
  const peek = usePeekControls();
  const held = s.ask?.held_call;
  if (held && compact) {
    return (
      <Button
        size="small"
        variant="secondary"
        aria-label={`Review call for ${s.seat.name}`}
        onClick={() => peek.open({ kind: "seat", id: s.seat.handle })}
      >
        Review call
      </Button>
    );
  }
  if (s.ask) return <AnswerRunButton run={s.ask} seat={s.seat.name} />;
  if (s.row?.stopped_reason === "budget") {
    return (
      <RaiseBudgetButton
        handle={s.seat.handle}
        name={s.seat.name}
        window={waitedOn(s.row.budget?.windows, "refusing")}
      />
    );
  }
  return null;
}

/**
 * The bar a selection of agents raises: each control names how many seats it
 * changes. A `folded` bar, on a phone's table, keeps Pause and Resume, by
 * their words alone, counted only where they change fewer seats than are
 * picked, and holds Pause and stop in its "More": all three, or the two with
 * their glyphs beside the "More", wrapped the bar onto a second row over the
 * list it acts on.
 */
export function SeatSelectionBar({
  selected,
  onClear,
  controls,
  folded = false,
}: {
  selected: readonly SeatLive[];
  onClear: () => void;
  controls: SeatControls;
  folded?: boolean;
}) {
  const counts = {
    pause: appliesTo("pause", selected).length,
    resume: appliesTo("resume", selected).length,
    stop: appliesTo("stop", selected).length,
  };
  const pauseAccess = useWriteAccess("pause_seat");
  const resumeAccess = useWriteAccess("resume_seat");
  // DOCKED AT A PHONE'S FOOT, where the stack stands: the toast a press raises
  // stands above the bar rather than over its next press.
  useToastClearOf(() => document.querySelector<HTMLElement>(".seat-selection-bar"), folded);
  // THE READER'S ACCESS FIRST, then whether any picked seat is in the state a control changes.
  const held = {
    pause:
      accessHeldReason(pauseAccess) ?? (counts.pause ? undefined : "None of these can be paused."),
    resume:
      accessHeldReason(resumeAccess) ?? (counts.resume ? undefined : "None of these is paused."),
    stop:
      accessHeldReason(pauseAccess) ?? (counts.stop ? undefined : "None of these is on a turn."),
  };
  const stopLabel = counts.stop ? `Pause and stop ${counts.stop}` : "Pause and stop";
  // A PHONE'S BAR COUNTS ONCE, in its count, where a control changes every seat
  // picked: at 360px the two counts wrapped its "More" onto a row of its own.
  const worded = (word: string, count: number) =>
    !count || (folded && count === selected.length) ? word : `${word} ${count}`;
  return (
    <SelectionBar
      size="compact"
      className="seat-selection-bar"
      count={selected.length}
      onClear={onClear}
      labels={{ label: "Selected agents" }}
    >
      <Button
        tooltip
        size="small"
        variant="secondary"
        leadingIcon={folded ? undefined : <PauseGlyph size="sm" />}
        disabledReason={held.pause}
        onClick={() => controls.open("pause", selected)}
      >
        {worded("Pause", counts.pause)}
      </Button>
      <Button
        tooltip
        size="small"
        variant="secondary"
        leadingIcon={folded ? undefined : <RotateCwGlyph size="sm" />}
        disabledReason={held.resume}
        onClick={() => controls.open("resume", selected)}
      >
        {worded("Resume", counts.resume)}
      </Button>
      {folded ? (
        <Menu
          label="More for the selected agents"
          icon={<EllipsisGlyph size="sm" />}
          triggerVariant="ghost"
          align="end"
          items={[
            {
              key: "stop",
              label: stopLabel,
              icon: <CircleStopGlyph />,
              danger: true,
              onSelect: () => controls.open("stop", selected),
              ...(held.stop ? { disabled: true, description: held.stop } : {}),
            },
          ]}
        />
      ) : (
        <Button
          tooltip
          size="small"
          variant="danger"
          leadingIcon={<CircleStopGlyph size="sm" />}
          disabledReason={held.stop}
          onClick={() => controls.open("stop", selected)}
        >
          {stopLabel}
        </Button>
      )}
    </SelectionBar>
  );
}
