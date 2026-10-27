/**
 * A turn's Timeline tab: the waterfall, and the one span a reader opened.
 *
 * THE MODEL IS `lib/waterfall.ts` — every placement is decided there, over
 * values, and this file only draws it. A row is a button: pressing it opens
 * the span's detail beside the waterfall and puts it in the URL (`span=`), so
 * a colleague can be sent to the exact call that was slow.
 *
 * # One tab stop, and Escape closes
 *
 * The rows are ONE tab stop — the open span's row, else the last one a reader
 * moved to — and Up, Down, Home and End move between them, as a list of
 * choices does everywhere else. Each row a tab stop of its own made a turn of
 * sixteen spans sixteen presses to walk past on the way to the tabs below.
 * Escape closes the open span from anywhere in the timeline, and closing puts
 * focus back on the row it was opened from, so a keyboard reader never lands
 * on the page's top after reading a call.
 *
 * # The detail is brought to the reader when it cannot sit beside the rows
 *
 * Beside the waterfall from a laptop's page width; UNDER it on a phone or a
 * laptop with the peek open, where it lands below every row — out of view on
 * a turn of any length. So a span a reader OPENS (not one a link arrived
 * with) is scrolled into view and focused when it stacks, which is the
 * difference between a click that visibly did something and one that seemed
 * to do nothing.
 *
 * # What a span's detail says
 *
 * Its offsets on the turn's own clock, and what only that kind of span has: a
 * model call's words and tokens, a tool call's input and output with "Why
 * {agent} called it" — the narration of the ROUND that asked for it, labelled
 * as that round's and saying how many calls the round asked for, because a
 * round that asked for four explains all four at once and a sentence quoted
 * under one of them would read as its alone — and a coding run's box facts.
 *
 * # A running coding run is watched, not replayed
 *
 * While a coding run's span is open AND a reader has it open, its live output
 * is asked of the node that owns the run every [SANDBOX_TAIL_POLL_MS]
 * (`sandbox_tail`), and the view holds what it is sent — up to what the run's
 * record will hold — following the end unless the reader scrolled up. Nothing
 * else asks: closing the span, or the run stopping, stops the poll; a job still
 * being set up is not a stopped one. The answer is honest about the ways it can
 * be empty — a run that has not written anything yet, a job not started yet,
 * and an owner that did not answer — and names the owner in the last, because
 * "nothing to show" and "the node that could show it is silent" send a reader
 * to opposite places.
 */

import {
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { Button, Callout, Card, CodeBlock, Tag, cx, InlineCode } from "@crewlethq/ui";
import { ClockGlyph, CircleAlertGlyph, SquareTerminalGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { NowLine } from "~/components/time/NowLine.tsx";
import { SpanBar } from "~/components/time/SpanBar.tsx";
import { TimeAxis } from "~/components/time/TimeAxis.tsx";
import {
  fmtBytes,
  fmtCount,
  fmtDuration,
  fmtElapsed,
  fmtTime,
  plural,
  relTime,
  utf8Bytes,
} from "~/lib/format.ts";
import { deliveredLine, outcomeWords, rounds, type PhaseRecord } from "~/lib/phases.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useLiveTail, type LiveView } from "~/lib/useLiveTail.ts";
import { SANDBOX_TAIL_POLL_MS } from "~/contract/sandbox.ts";
import { STEER_BOUNDARIES } from "~/contract/steer.ts";
import {
  fraction,
  phaseLabel,
  type Span,
  type SpanKind,
  type Waterfall as Model,
} from "~/lib/waterfall.ts";
import type { EventRecord, SandboxTailAnswer } from "~/protocol/index.ts";

/** What each kind of span is called in its detail's eyebrow. */
const KIND_LABEL: Record<SpanKind, string> = {
  turn: "Turn",
  context: "Context",
  phase: "Phase",
  invocation: "CLI run",
  model: "Model call",
  tool: "Tool call",
  worker: "Worker",
  judge: "Judge",
  run: "Coding run",
  reflection: "Reflection",
  pending: "Waiting",
};

/** A kind of step a note is read at (`types.SteerBoundary`). */
type SteerBoundary = (typeof STEER_BOUNDARIES)[number];

/**
 * How the trace says each kind of step that read a note. A read INSIDE A CODING
 * RUN is the run's — its brief, a batch of its tool calls, its finish, its next
 * step — and is drawn on the run's bar, said as the step of the phase's run
 * (*read by the coding run of Execute as it was finishing*); a round is the
 * engine's own loop's (*read at round 2 of Review*). The compiler holds every
 * boundary of the contract to its words.
 */
const READ_AT: Record<SteerBoundary, { inRun: boolean; words: string }> = {
  round: { inRun: false, words: "read at round" },
  brief: { inRun: true, words: "in its brief" },
  tool_batch: { inRun: true, words: "after a tool batch" },
  finish: { inRun: true, words: "as it was finishing" },
  step: { inRun: true, words: "at its next step" },
};

/**
 * The kind of step a delivered record names. TWO RECORDS THIS BUILD CANNOT
 * NAME, told apart: one with NO boundary is from a build before the field,
 * whose only step was a round, so it is a round; one naming a boundary this
 * build does not know is from a NEWER build, which added a kind of step — said
 * as nothing more than the turn having read it, and never as a round.
 */
function boundaryOf(raw: unknown): SteerBoundary | "unknown" {
  if (typeof raw !== "string" || raw === "") return "round";
  return (STEER_BOUNDARIES as readonly string[]).includes(raw) ? (raw as SteerBoundary) : "unknown";
}

/** A person's note, as the turn's own record says what became of it. */
export interface SteerMark {
  noteId: string;
  outcome: "delivered" | "expired";
  note: string;
  /** Who sent it, as the record names its author (`steered_by`, from
   *  `iam.ActorFor`): a seat's HANDLE where the sender is bound to one — which
   *  a reader sees as the seat's name — else the credential's own login. */
  by: string;
  /** Whether `by` is a seat's handle: the author kind the engine recorded
   *  beside it (`steered_by_kind`) is `human` or `agent`, never `operator`. */
  bySeat: boolean;
  /** The span that read it — a round's model call, or the coding run's bar for
   *  a read inside the run — or "" for an expired one. */
  spanId: string;
  round: number;
  phase: string;
  iteration: number;
  /** The kind of step that read it; "round" for a record from before the
   *  field, "unknown" for one a newer build names, and "" for an expired
   *  note, which nothing read. */
  boundary: SteerBoundary | "unknown" | "";
  /** The coding run's job that read it, for a read inside a run. */
  launchId: string;
  at: string;
}

/** Every `agent_turn_steered` the turn recorded, placed at the round that read it. */
export function steerMarks(events: readonly EventRecord[], turnId: string): SteerMark[] {
  return events
    .filter((e) => e.type === "agent_turn_steered")
    .map((e) => {
      const p = (e.payload ?? {}) as Record<string, unknown>;
      const outcome = p.outcome === "delivered" ? "delivered" : "expired";
      const phase = typeof p.phase === "string" ? p.phase : "";
      const iteration = typeof p.iteration === "number" ? p.iteration : 0;
      const round = typeof p.round === "number" ? p.round : 0;
      // THE AUTHOR AND ITS KIND, the two halves `iam.ActorFor` records: a bound
      // person's note is written AS THEIR SEAT (kind `human`), so its author is
      // a handle to name; an unbound credential's is its own login.
      const by = typeof p.steered_by === "string" ? p.steered_by : "";
      const kind = typeof p.steered_by_kind === "string" ? p.steered_by_kind : "";
      const launchId = typeof p.launch_id === "string" ? p.launch_id : "";
      // A DELIVERED NOTE NAMES ITS STEP; one from a build before the field was
      // read at a round, which was the only step there was.
      const boundary = outcome === "delivered" ? boundaryOf(p.boundary) : "";
      // A READ INSIDE A CODING RUN is drawn on that run's bar (`run:<job>`,
      // the span the trace gives a job), not on a round of the engine's loop,
      // which was parked while the run worked. A step this build does not
      // know is placed on the run its record names, and nowhere without one:
      // drawn on a round, it would claim the engine's loop read a note it
      // never saw.
      const inRun =
        boundary === "unknown" ? launchId !== "" : boundary !== "" && READ_AT[boundary].inRun;
      return {
        noteId: String(p.note_id ?? e.id),
        outcome,
        note: String(p.note ?? ""),
        by,
        bySeat: by !== "" && (kind === "human" || kind === "agent"),
        spanId:
          outcome !== "delivered"
            ? ""
            : inRun
              ? launchId
                ? `run:${launchId}`
                : ""
              : boundary !== "unknown" && phase && round
                ? `${turnId}|${phase}|${iteration}.r${round}`
                : "",
        round,
        phase,
        iteration,
        boundary,
        launchId,
        at: e.timestamp,
      } satisfies SteerMark;
    });
}

export function Waterfall({
  model,
  phases,
  marks,
  agent,
  selected,
  onSelect,
  now,
  turnId,
}: {
  model: Model;
  phases: readonly PhaseRecord[];
  marks: readonly SteerMark[];
  /** Whose turn — "Why {agent} called it". */
  agent: string;
  selected: string;
  onSelect: (id: string) => void;
  now: number;
  turnId: string;
}) {
  const { from, to, spans, untimed } = model;
  const open = spans.find((s) => s.id === selected) ?? null;
  const marked = useMemo(() => new Set(marks.map((m) => m.spanId).filter(Boolean)), [marks]);
  const headId = useId();
  const rowsRef = useRef<HTMLOListElement>(null);
  const railRef = useRef<HTMLDivElement>(null);
  const detailRef = useRef<HTMLDivElement>(null);

  // THE ONE TAB STOP: the open span's row, else the last row a reader moved
  // to, else the first. A cursor naming a span that is gone (a running turn's
  // placeholder round replaced by its record) falls back the same way.
  const [cursor, setCursor] = useState("");
  const stop = spans.some((s) => s.id === selected)
    ? selected
    : spans.some((s) => s.id === cursor)
      ? cursor
      : (spans[0]?.id ?? "");
  const rowOf = (id: string) =>
    rowsRef.current?.querySelector<HTMLButtonElement>(`[data-span="${CSS.escape(id)}"]`) ?? null;

  // A SPAN A READER OPENED is brought to them when it stacks under the rows —
  // see the file's doc. Only on an OPEN by this page's own rows: a span a
  // link arrived with is where the reader was sent, and scrolling the page
  // away from its header on load would be the page choosing for them.
  const revealing = useRef(false);
  useLayoutEffect(() => {
    if (!open || !revealing.current) return;
    revealing.current = false;
    const detail = detailRef.current;
    const rail = railRef.current;
    if (!detail || !rail) return;
    const stacked = detail.getBoundingClientRect().top >= rail.getBoundingClientRect().bottom - 1;
    if (!stacked) return;
    detail.scrollIntoView({ block: "nearest" });
    detail.focus({ preventScroll: true });
  }, [open]);

  const choose = (id: string) => {
    revealing.current = id !== "";
    onSelect(id);
  };
  // CLOSING RETURNS FOCUS to the row the span was opened from — by Escape or
  // by the detail's own close button, which unmounts under the reader's focus.
  const close = () => {
    const id = selected;
    choose("");
    setCursor(id);
    requestAnimationFrame(() => rowOf(id)?.focus());
  };

  const move = (event: KeyboardEvent<HTMLOListElement>) => {
    const at = spans.findIndex((s) => s.id === stop);
    let next = -1;
    if (event.key === "ArrowDown") next = Math.min(spans.length - 1, at + 1);
    else if (event.key === "ArrowUp") next = Math.max(0, at - 1);
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = spans.length - 1;
    if (next < 0) return;
    event.preventDefault();
    const id = spans[next]!.id;
    setCursor(id);
    rowOf(id)?.focus();
  };

  if (!spans.length && !untimed.length) {
    return <p className="t-caption waterfall-empty">Nothing recorded yet.</p>;
  }

  return (
    <div
      className={cx("trace-timeline", open && "has-detail")}
      onKeyDown={(event) => {
        if (event.key !== "Escape" || !open || event.defaultPrevented) return;
        event.preventDefault();
        event.stopPropagation();
        close();
      }}
    >
      <div
        ref={railRef}
        className="waterfall"
        role="group"
        aria-label="The turn's spans on one clock"
      >
        {spans.length > 0 && (
          <div className="waterfall-body">
            <div className="waterfall-head">
              <span className="waterfall-head-label">Span</span>
              <div className="waterfall-head-axis">
                <TimeAxis from={from} to={to} />
              </div>
            </div>
            <ol
              ref={rowsRef}
              className="waterfall-rows"
              aria-label="Spans. Up and Down move between them, Enter opens one."
              onKeyDown={move}
            >
              {spans.map((s) => (
                <li key={s.id}>
                  <button
                    type="button"
                    className={cx("wf-row", s.id === selected && "selected")}
                    data-kind={s.kind}
                    data-span={s.id}
                    tabIndex={s.id === stop ? 0 : -1}
                    aria-pressed={s.id === selected}
                    title={`${s.label}${s.sub ? ` · ${s.sub}` : ""} — ${
                      s.open
                        ? `running for ${fmtDuration(now - s.start)}`
                        : fmtDuration(s.end - s.start)
                    }`}
                    onFocus={() => setCursor(s.id)}
                    onClick={() => (s.id === selected ? close() : choose(s.id))}
                  >
                    <span
                      className="wf-label"
                      style={{ paddingInlineStart: `calc(${s.depth} * var(--wf-indent))` }}
                    >
                      <span className="wf-name truncate">{s.label}</span>
                      {s.sub && <span className="wf-sub truncate">{s.sub}</span>}
                      {marked.has(s.id) && (
                        <span className="wf-steer" title="Your note was read here">
                          note
                        </span>
                      )}
                    </span>
                    <span className="wf-track">
                      <SpanBar
                        kind={s.kind}
                        left={fraction(s.start, from, to)}
                        width={fraction(s.end, from, to) - fraction(s.start, from, to)}
                        open={s.open}
                        failed={s.failed}
                        selected={s.id === selected}
                      />
                    </span>
                    <span className="wf-dur">
                      {s.open ? fmtElapsed(now - s.start) : fmtDuration(s.end - s.start)}
                    </span>
                  </button>
                </li>
              ))}
            </ol>
            {model.open && (
              <div className="waterfall-now" aria-hidden>
                <span className="waterfall-now-track">
                  <NowLine now={now} from={from} to={to} />
                </span>
              </div>
            )}
          </div>
        )}
        {untimed.length > 0 && (
          <section className="waterfall-untimed" aria-labelledby={`${headId}-untimed`}>
            <h2 className="t-label waterfall-subhead" id={`${headId}-untimed`}>
              Not placed on this clock
            </h2>
            <ul>
              {untimed.map((u) => (
                <li key={u.id} className="t-caption">
                  {KIND_LABEL[u.kind]}: {u.label}
                  {u.sub ? ` · ${u.sub}` : ""}
                </li>
              ))}
            </ul>
          </section>
        )}
        {marks.length > 0 && <Notes marks={marks} />}
      </div>
      {open && (
        <SpanDetail
          key={open.id}
          ref={detailRef}
          span={open}
          from={from}
          now={now}
          phases={phases}
          agent={agent}
          turnId={turnId}
          onClose={close}
        />
      )}
    </div>
  );
}

/** The notes people sent this turn, and what became of each. */
function Notes({ marks }: { marks: readonly SteerMark[] }) {
  // A SECTION NAMED BY ITS OWN VISIBLE HEADING. It was a `div` with an
  // `aria-label`, which a screen reader ignores on a generic element — the
  // name announced nothing, and the list had no landmark to be found by.
  const headId = useId();
  // A SEAT BY ITS NAME, as every other attribution here is drawn: the record
  // carries the sender's handle, and "from jane-founder" beside "Jane Founder"
  // everywhere else read as two people.
  const badgeOf = useSeatBadgeOf();
  return (
    <section className="waterfall-notes" aria-labelledby={headId}>
      <h2 className="t-label waterfall-subhead" id={headId}>
        Notes sent to this turn
      </h2>
      <ul>
        {marks.map((m) => (
          <li key={m.noteId} className="steer-mark">
            <Tag variant={m.outcome === "delivered" ? "info" : "warning"} size="sm">
              {m.outcome}
            </Tag>
            <span className="steer-mark-text">
              {m.outcome === "delivered" ? readAt(m) : "the turn finished before it read the note"}
              {m.by ? ` · from ${m.bySeat ? badgeOf(m.by).name : m.by}` : ""}
              {m.note && <q className="steer-mark-note">{m.note}</q>}
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * Where a delivered note was read, in words: the round of a phase, the step of a
 * phase's coding run, or — for a step a newer build names — the turn in that
 * phase, never a round.
 */
function readAt(m: SteerMark): string {
  const phase = m.phase ? phaseLabel({ phase: m.phase, iteration: m.iteration }) : "";
  if (m.boundary === "unknown") return `read by the turn${phase ? ` in ${phase}` : ""}`;
  const of = phase ? ` of ${phase}` : "";
  if (m.boundary === "" || m.boundary === "round") return `read at round ${m.round}${of}`;
  return `read by the coding run${of} ${READ_AT[m.boundary].words}`;
}

function Facts({ rows }: { rows: [string, ReactNode][] }) {
  const shown = rows.filter(([, v]) => v !== "" && v !== null && v !== undefined);
  if (!shown.length) return null;
  return (
    <dl className="span-facts">
      {shown.map(([k, v]) => (
        <div key={k} className="span-fact">
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

function SpanDetail({
  ref,
  span,
  from,
  now,
  phases,
  agent,
  turnId,
  onClose,
}: {
  /** The box `Waterfall` brings into view and focuses when it stacks. */
  ref: RefObject<HTMLDivElement | null>;
  span: Span;
  from: number;
  now: number;
  phases: readonly PhaseRecord[];
  agent: string;
  turnId: string;
  onClose: () => void;
}) {
  const record = phases.find((p) => p.key === span.phaseKey) ?? null;
  const length = span.open ? Math.max(0, now - span.start) : span.end - span.start;
  const offsets: [string, ReactNode][] = [
    [
      "Starts",
      `+${fmtDuration(Math.max(0, span.start - from))} · ${fmtTime(new Date(span.start).toISOString())}`,
    ],
    [span.open ? "Running for" : "Took", fmtDuration(length)],
  ];
  // THE KIND IS THE EYEBROW ONLY WHERE IT SAYS SOMETHING the title does not:
  // the context span's name IS its kind, and "Context" over "Context" read as
  // a stutter rather than as two facts.
  const kind = KIND_LABEL[span.kind];
  const eyebrow = kind.toLowerCase() === span.label.toLowerCase() ? undefined : kind;
  return (
    <div
      ref={ref}
      className="span-detail"
      tabIndex={-1}
      role="region"
      aria-label={eyebrow ? `${kind}: ${span.label}` : span.label}
    >
      <Card padding="sm">
        <Card.Header
          icon={span.failed ? <CircleAlertGlyph size="sm" /> : <ClockGlyph size="sm" />}
          {...(eyebrow ? { subtitle: eyebrow } : {})}
          actions={
            <Button
              size="small"
              variant="ghost"
              aria-label="Close the span"
              title="Close the span (Escape)"
              leadingIcon={<XGlyph size="sm" />}
              onClick={onClose}
            />
          }
        >
          <Card.Title>{span.label}</Card.Title>
        </Card.Header>
        <div className="col gap-3">
          <div className="row gap-2 wrap">
            {span.open && (
              <Tag variant="info" dot>
                running
              </Tag>
            )}
            {span.failed && <Tag variant="danger">failed</Tag>}
            {span.placed && (
              <Tag appearance="outline" title="Start time unknown">
                placed after the model&rsquo;s answer
              </Tag>
            )}
          </div>
          <Facts rows={offsets} />
          {span.kind === "invocation" && record && <InvocationDetail span={span} record={record} />}
          {span.kind === "model" && record && <ModelDetail span={span} record={record} />}
          {span.kind === "tool" && record && (
            <ToolDetail span={span} record={record} agent={agent} />
          )}
          {(span.kind === "phase" || span.kind === "worker" || span.kind === "judge") && record && (
            <PhaseDetail record={record} />
          )}
          {span.kind === "run" && (
            <RunDetail span={span} record={record} turnId={turnId} now={now} />
          )}
        </div>
      </Card>
    </div>
  );
}

/**
 * One CLI run: which entry served it, the rounds it made, whether its CLI
 * showed them, how it ended and what it spent. The run in flight is on no
 * record yet, and says so.
 */
function InvocationDetail({ span, record }: { span: Span; record: PhaseRecord }) {
  const run = record.invocations.find((r) => r.run === span.invocation);
  if (!run) {
    return (
      <span className="t-caption">
        Still running — what it made and spent lands when its CLI exits.
      </span>
    );
  }
  const made = run.firstRound > 0 ? run.lastRound - run.firstRound + 1 : 0;
  return (
    <Facts
      rows={[
        ["Entry", run.providerKey],
        ["Model", run.model],
        ["Ended", outcomeWords(run.outcome).label],
        [
          "Rounds",
          made === 0
            ? "none — it made no model request"
            : made === 1
              ? `round ${run.firstRound}`
              : `rounds ${run.firstRound}–${run.lastRound}`,
        ],
        [
          "Requests",
          run.requestsObserved ? "" : "not shown by its CLI — the whole run is one round",
        ],
        [
          "Tokens",
          run.inputTokens + run.outputTokens > 0
            ? `${fmtCount(run.inputTokens)} in · ${fmtCount(run.outputTokens)} out`
            : "",
        ],
        ["Tool calls", String(run.toolCalls)],
      ]}
    />
  );
}

function ModelDetail({ span, record }: { span: Span; record: PhaseRecord }) {
  const timed = record.timedRounds.find((r) => r.round === span.round);
  const said = rounds(record.tools, record.narration).find((r) => r.round === span.round);
  const words = said?.content.trim() ?? "";
  const run = record.invocations.find((r) => r.run === span.invocation);
  return (
    <>
      <Facts
        rows={[
          ["CLI run", span.invocation > 0 ? String(span.invocation) : ""],
          [
            "Timed",
            run && !run.requestsObserved
              ? "its CLI showed no requests, so this round is the whole run"
              : "",
          ],
          [
            "Ended",
            timed?.stopReason === "cap"
              ? "the CLI’s own turn cap ended its run here, with the model still working"
              : "",
          ],
          ["Model", timed?.model || record.model],
          [
            "Tokens",
            timed ? `${fmtCount(timed.inputTokens)} in · ${fmtCount(timed.outputTokens)} out` : "",
          ],
          [
            "Cache",
            timed && timed.cacheReadTokens > 0
              ? `${fmtCount(timed.cacheReadTokens)} read from cache`
              : "",
          ],
          ["Tool calls", timed ? String(timed.toolCalls) : ""],
        ]}
      />
      {words && (
        <div className="col gap-1">
          <span className="t-label">What it said</span>
          <p className="span-words">{words}</p>
        </div>
      )}
    </>
  );
}

function ToolDetail({ span, record, agent }: { span: Span; record: PhaseRecord; agent: string }) {
  const call = span.toolIndex >= 0 ? record.tools[span.toolIndex] : undefined;
  const round = rounds(record.tools, record.narration, null, record.runningCalls).find(
    (r) => r.round === span.round,
  );
  // Asked for: the calls that answered and the ones still running alike.
  const asked = (round?.tools.length ?? 0) + (round?.running.length ?? 0);
  const why = round?.content.trim() ?? "";
  // THE CALL THIS SPAN IS, by its own index: a round runs its calls side by
  // side, so its round alone no longer says which one is running.
  const running = !call && span.runningIndex >= 0 ? record.runningCalls[span.runningIndex] : null;
  return (
    <>
      <Facts
        rows={[
          [
            "Origin",
            call?.origin === "builtin"
              ? "built in"
              : call?.server
                ? `MCP · ${call.server}`
                : (call?.origin ?? ""),
          ],
          ["Round", String(span.round)],
        ]}
      />
      <div className="col gap-1">
        <span className="t-label">Why {agent || "the agent"} called it</span>
        {why ? (
          <>
            <p className="span-words">{why}</p>
            <span className="t-caption">
              from round {span.round}
              {asked > 1 ? ` · this round asked for ${asked} calls` : ""}
            </span>
          </>
        ) : (
          <span className="t-caption">Round {span.round} said nothing before this call.</span>
        )}
      </div>
      {(call?.args || running?.arguments) && <span className="t-label">Input</span>}
      {/* HALF THE RECORD CEILING EACH, derived from the one the shell declares
          (`--record-ceiling`, frame.css) rather than restated: Input and
          Output are read as a pair, so both fit on one screen together. */}
      {(call?.args || running?.arguments) && (
        <CodeBlock
          plain
          selectable
          copyable={false}
          maxHeight="calc(var(--record-ceiling) / 2)"
          label="Input"
          code={call?.args || running?.arguments || ""}
        />
      )}
      {call ? (
        call.result ? (
          <>
            <span className="t-label">Output</span>
            <CodeBlock
              plain
              selectable
              copyable={false}
              maxHeight="calc(var(--record-ceiling) / 2)"
              label="Output"
              code={call.result}
            />
          </>
        ) : (
          <span className="t-caption">The call returned nothing.</span>
        )
      ) : (
        <span className="t-caption">Still running.</span>
      )}
    </>
  );
}

function PhaseDetail({ record }: { record: PhaseRecord }) {
  return (
    <Facts
      rows={[
        ["Model", record.model],
        ["Worker", record.worker],
        ["Rounds", record.roundsUsed > 0 ? String(record.roundsUsed) : ""],
        [
          "Tokens",
          record.totalTokens > 0
            ? `${fmtCount(record.inputTokens)} in · ${fmtCount(record.outputTokens)} out`
            : "",
        ],
        ["Decided", record.decision],
        ["Error", record.error],
      ]}
    />
  );
}

function RunDetail({
  span,
  record,
  turnId,
  now,
}: {
  span: Span;
  record: PhaseRecord | null;
  turnId: string;
  now: number;
}) {
  return (
    <>
      <Facts
        rows={[
          ["Coding agent", span.sub],
          ["Box", record?.sandboxId ? <InlineCode>{record.sandboxId}</InlineCode> : ""],
          ["Job", span.launchId ? <InlineCode>{span.launchId}</InlineCode> : ""],
          ["Delivered", record ? deliveredLine(record) : ""],
          ["Tokens", record && record.totalTokens > 0 ? fmtCount(record.totalTokens) : ""],
        ]}
      />
      {span.open && span.launchId ? (
        <LiveOutput turnId={turnId} launchId={span.launchId} now={now} />
      ) : span.open ? (
        <NoJobCaption />
      ) : record?.transcript ? (
        <CodeBlock plain selectable copyable label="What the run did" code={record.transcript} />
      ) : (
        <span className="t-caption">The run&rsquo;s record carries no transcript.</span>
      )}
    </>
  );
}

/**
 * What stands where a running coding run's live output would, when its
 * announcement names no job to ask for: the Timeline's span detail and a
 * parked turn's Transcript say it in the same words.
 */
export function NoJobCaption() {
  return (
    <span className="t-caption">
      No live output. It appears on the run&rsquo;s record when collected.
    </span>
  );
}

/**
 * A running coding run's live output, asked of the node that owns it every
 * [SANDBOX_TAIL_POLL_MS] for as long as this is mounted — which is as long as
 * a reader has the span (or the run's page) open and the job has not stopped.
 *
 * It HOLDS what it has been sent, up to what the run's record will hold, and is
 * sent only what it lacks (`lib/useLiveTail.ts`). It FOLLOWS THE END — the
 * newest line is the one that answers "what is it doing now" — unless the
 * reader has scrolled up, in which case the text they are reading stays where
 * it is. And it says what it does not hold, from the answer and from what it
 * dropped, rather than spelling a bound of its own.
 *
 * A `replaced` answer is reported through `onReplaced`, for a screen that names
 * the job by the run's current one: the run's page reads the board again and
 * follows the new job.
 */
export function LiveOutput({
  turnId,
  launchId,
  now,
  onReplaced,
}: {
  turnId: string;
  launchId: string;
  now: number;
  onReplaced?: () => void;
}) {
  const tail = useLiveTail(turnId, launchId);
  const answer = tail.answer;
  const replaced = answer?.outcome === "not_running" && answer.status === "replaced";
  useEffect(() => {
    if (replaced) onReplaced?.();
  }, [replaced, onReplaced]);

  const frame = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  // The block is drawn once there is text; its scroll is listened to from then.
  const showing = tail.text !== "";
  // THE READER'S SCROLL DECIDES. At the bottom (within a line of it), the
  // block follows the end; anywhere else, it stays where they put it.
  useEffect(() => {
    const pre = frame.current?.querySelector("pre");
    if (!pre) return;
    const onScroll = () => {
      following.current = pre.scrollHeight - pre.scrollTop - pre.clientHeight < FOLLOW_SLACK_PX;
    };
    pre.addEventListener("scroll", onScroll);
    return () => pre.removeEventListener("scroll", onScroll);
  }, [showing]);
  useLayoutEffect(() => {
    const pre = frame.current?.querySelector("pre");
    if (pre && following.current) pre.scrollTop = pre.scrollHeight;
  }, [tail.text]);

  let notice: ReactNode = null;
  if (!answer) {
    notice = tail.error ? (
      <Callout variant="warning">The run&rsquo;s output could not be read: {tail.error}.</Callout>
    ) : (
      <span className="t-caption">Asking the node that runs it…</span>
    );
  } else if (tail.error) {
    notice = <Callout variant="warning">Last read failed ({tail.error}). Retrying.</Callout>;
  } else if (answer.outcome === "owner_silent") {
    notice = (
      <Callout variant="warning">
        {answer.node ? `${answer.node} didn’t answer. Retrying.` : "Recovering this run."}
      </Callout>
    );
  } else if (answer.outcome === "launching") {
    notice = <span className="t-caption">Setting up. Output appears when the agent starts.</span>;
  } else if (answer.outcome === "box_paused") {
    notice = <span className="t-caption">Finished. Output appears on the run&rsquo;s record.</span>;
  } else if (answer.outcome === "not_running") {
    notice = (
      <span className="t-caption">
        {answer.status === "awaiting_clarification" || answer.status === "reseed"
          ? "Waiting for an answer."
          : replaced
            ? "A later job replaced this one on the same run."
            : "Stopped. Output appears on its record."}
      </span>
    );
  } else if (tail.text === "") {
    notice = <span className="t-caption">No output yet.</span>;
  }
  const out = answer?.output;
  // WHAT IS ANNOUNCED IS THE OUTCOME, never the output. The whole block was a
  // polite live region, re-rendered every poll: the "read 4s ago" caption,
  // each new line of output and the busy flag changed every three seconds, so
  // a screen reader read the clock and the transcript aloud for as long as
  // the span stayed open. Only a change of STATE — the owner went silent, the
  // run stopped, it is showing output again — is news; the output itself is
  // there to be read at the reader's own pace.
  const state = liveState(answer, !!tail.error);
  const holding = holdingLine(tail);
  return (
    <div className="col gap-2 live-output">
      <span className="row gap-2 baseline">
        <SquareTerminalGlyph size="sm" />
        <span className="t-label">Live output</span>
        {out && (
          <span className="t-caption">
            {holding ? `${holding} · ` : ""}read {relTime(out.as_of, now)}
            {answer?.node ? ` on ${answer.node}` : ""}
            {out.finished ? " · finished, waiting to be collected" : ""}
          </span>
        )}
      </span>
      <span className="sr-only" role="status">
        {state}
      </span>
      {notice}
      {showing && (
        <div ref={frame}>
          <CodeBlock
            plain
            selectable
            copyable
            label={out?.source === "stderr" ? "Live output (its error stream)" : "Live output"}
            code={tail.text}
          />
        </div>
      )}
      {tail.held > 0 && !tail.final && (
        <span className="t-caption">{fmtBytes(tail.held)} more held until it can be redacted.</span>
      )}
      {!tail.final && (
        <span className="t-caption">
          Refreshes every {plural(SANDBOX_TAIL_POLL_MS / 1000, "second")}.
        </span>
      )}
    </div>
  );
}

/** How near the bottom a reader's scroll counts as following the end, in px —
 *  about a line, so a block that grew by one line under a reader at the bottom
 *  still reads as one they were following. */
const FOLLOW_SLACK_PX = 24;

/**
 * What the view holds, said when it is not the whole of what the run wrote —
 * from the answer's own bound and the view's own count, never a figure spelled
 * here.
 */
export function holdingLine(view: LiveView): string {
  const parts: string[] = [];
  if (view.dropped > 0) {
    parts.push(
      `the last ${fmtBytes(utf8Bytes(view.text))} held; ${fmtBytes(view.dropped)} earlier not held here`,
    );
  }
  if (view.front) parts.push("read from partway through the run");
  return parts.join("; ");
}

/**
 * The one sentence a screen reader is told about a live tail: which STATE it
 * is in. It changes only when the state does, so a live region holding it
 * speaks once per change rather than once per poll.
 */
export function liveState(answer: SandboxTailAnswer | null | undefined, failed: boolean): string {
  if (!answer)
    return failed ? "The run's output could not be read." : "Asking the node that runs it.";
  switch (answer.outcome) {
    case "owner_silent":
      return "The node that owns this run did not answer.";
    case "launching":
      return "The job is being set up.";
    case "box_paused":
      return "The run's box is paused; its job has finished.";
    case "not_running":
      return "The run is no longer running.";
    default:
      return !answer.output || answer.output.source === "none"
        ? "The run has not written anything yet."
        : answer.output.finished
          ? "The run finished; its output is shown."
          : "Showing the run's live output.";
  }
}
