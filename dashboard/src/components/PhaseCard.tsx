/**
 * One phase of one turn, rendered as a stable round ledger.
 *
 * Seven rules, each of which fixes a specific way the previous surface either
 * moved under the reader or told them something untrue:
 *
 *  1. **Identity is `turn|phase|iteration`** (see lib/phases.ts), so a phase
 *     that finishes updates IN PLACE instead of being removed and re-inserted
 *     somewhere else in the list.
 *  2. **Open/closed is latched.** Once a reader opens a phase it stays open
 *     until they close it. The previous surface defaulted a row to open only
 *     while it was live and closed the instant it completed — hiding the
 *     transcript at exactly the moment it became complete — and slammed the
 *     whole turn card shut when its last phase finished, for the same reason.
 *  3. **Each round is one block: what the model thought, what it said, then
 *     the tools it called** — in that order, which is the order they happened.
 *     Rounds only append, so nothing above the insertion point can move.
 *
 *     The engine sends `round_narration` for exactly this. Before it did, the
 *     only text was `response`, the JOIN of every round's turn, and a join
 *     cannot be undone: this card split it on a leading `<think>` tag, so
 *     round 1's thinking rendered as "the reasoning" and every later round's
 *     thinking rendered as "the model output", tags and all. A reader saw the
 *     model deliberating about which tool to try, labelled as its answer, in a
 *     block detached from the tool calls that deliberation was about.
 *
 *     Grouping the data was only half of it: the block has to LOOK like one
 *     block. It did not, and a reader said so — a thought and the call it
 *     asked for read as two unrelated collapsed rows. The devices meant to
 *     bound a round both described the sequence instead (a rail drawn only
 *     between rounds, a tint only on even ones), so a phase with a single
 *     round had neither. The rail is a bracket around each round now and the
 *     round's content shares one left edge; see `.round` in screens.css.
 *
 *  4. **The model's words are set as prose, not as code.** Its reasoning and
 *     its speech are natural language and get a proportional face, a real
 *     line height and a bounded measure. Monospace stays where it means
 *     something: tool arguments and tool results, which are JSON.
 *  5. **The header says the same things in the same places, always** — phase,
 *     model, rounds, tokens, decision — so a live phase and a finished one are
 *     the same shape and the row does not change height when it completes.
 *  6. **The prompt is a document, not a wall of text.** Every prompt this
 *     engine builds is markdown, so the Prompt fold folds each half on the
 *     headings the builders wrote, renders each section as the markdown it is,
 *     and keeps the verbatim record as its other view — see `PromptDoc.tsx`.
 *     It was one 30 kB scroller, which is where a reader went to answer "what
 *     was this phase told about X" and scrolled looking for a heading.
 *  7. **The body reads in the order the phase happened**: what it was given
 *     (Prompt, Tool surface), then what it did (the rounds), then what it
 *     delegated. The transcript used to come first and its two inputs sat
 *     underneath it, so the question every round raises — what was it told,
 *     what was it allowed to call — was answered past the end of the answer,
 *     and a phase with forty rounds put a whole scroll between the two. Both
 *     inputs are closed folds, so what the order costs a reader who only wants
 *     the transcript is two header rows; what it buys is that the card can be
 *     read top to bottom as the story of one phase.
 *
 *     Their own order is the request's: the Prompt is what was sent, and the
 *     Tool surface is the schema array sent WITH it — one payload, described
 *     in two folds, so they belong side by side rather than either side of
 *     the transcript.
 */

import { useEffect, useRef, useState } from "react";
import { Callout, CodeBlock, cx, Disclosure, EmptyValue, Tag } from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  KeyboardArrowDownGlyph,
  TerminalGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
// STILL OURS. `PhaseTag` HAS a peer — uilet's `Tag` carries `phase-onboarding`,
// `phase-execute` and `phase-review` — but it is a primitive in `~/ui`, and
// porting it THERE moves this card, the turn card and the seat screen in one
// change rather than leaving three inlined copies of one variant table behind.
import { PhaseTag, uiletTone } from "~/ui/primitives.tsx";
import { fmtCount, fmtDateTime, fmtDuration, fmtElapsed, relTime, tsKey } from "~/lib/format.ts";
import {
  decisionLabel,
  decisionTone,
  ledgerOf,
  phaseDuration,
  type PhaseRecord,
  type Round,
} from "~/lib/phases.ts";
import { staleness } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { href, useIsCurrent } from "~/app/router.tsx";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";
import { PromptRecord } from "~/components/PromptDoc.tsx";

function ToolRow({
  name,
  args,
  result,
  failed,
}: {
  name: string;
  args: string;
  result: string;
  failed: boolean;
}) {
  return (
    <div className={cx("tool-row", failed && "failed")}>
      <Disclosure
        mono
        // THE WORD, not a glyph. `meta` IS our `mark`: a SIBLING of the name
        // rather than part of it, because inside the title the mark sat in a
        // truncating single-line span and was the thing that wrapped, so a
        // failed call showed its alert on its own line above the tool.
        //
        // But it was a red glyph carrying NO ACCESSIBLE NAME, so the row a
        // reader most needs to find was announced exactly like the one above
        // it, and the hue was the only signal anybody got. It is the word
        // "failed" now, after the tool's name and part of what the control is
        // called; the row keeps its own tint, so colour and word say it
        // together.
        meta={failed ? "failed" : undefined}
        title={name}
        // A TRANSCRIPT ITEM IS NOT A SECTION OF THE PAGE. uilet wraps a
        // disclosure's trigger in a real heading by default, which is right for
        // the card's own folds (Prompt, Tool surface, Delegated to) and wrong
        // for a tool call: a round with nine of them would put nine headings
        // into the document outline of one phase.
        headingLevel="none"
        // Our Disclosure mounted its children only while open. `lazy` is how
        // uilet spells that, and here it is the behaviour rather than an
        // optimisation — a closed tool row must not put its result into the
        // round's text.
        lazy
      >
        <div className="col gap-1">
          <div className="t-label">Arguments</div>
          {/* NAMED WITH THE TOOL. A screen reader landing on a scrollable
              block announces the name and nothing around it, and half a
              dozen regions called "Arguments" on one round is the same as
              none.

              `plain` MEANS SOMETHING ELSE OVER HERE: ours turned wrapping off,
              theirs drops the header. Both are wanted — the block is bare in
              this design and arguments are aligned JSON — so it is `plain` for
              the header and `wrap={false}` for the columns. `maxHeight` is our
              own `RECORD_MAX_HEIGHT`, and it has to be stated: without one a
              900-line record pushes the rest of the round off the screen. */}
          <CodeBlock
            plain
            wrap={false}
            maxHeight={RECORD_MAX_HEIGHT}
            selectable
            label={`${name} — arguments`}
            code={args || "{}"}
          />
          <div className="t-label">{failed ? "Error" : "Result"}</div>
          <CodeBlock
            plain
            maxHeight={RECORD_MAX_HEIGHT}
            selectable
            label={`${name} — ${failed ? "error" : "result"}`}
            code={result || "(empty)"}
          />
        </div>
      </Disclosure>
    </div>
  );
}

/**
 * Keep the newest round in view while a phase runs — but ONLY while the reader
 * is already at the bottom.
 *
 * That condition is the whole feature. A ledger that scrolls itself whatever
 * the reader is doing yanks them off the round they stopped to read, which is
 * worse than not following at all; one that never follows makes a running
 * phase look frozen. So "am I still tailing?" is a piece of reader state, set
 * by where they last left the scroll.
 */
function useTail(active: boolean) {
  // A ref to the SCROLLER itself, not to a marker inside it. The scroller is
  // conditionally a scroller — it only bounds its height while the phase is
  // live — so resolving it by `closest()` at mount found whatever happened to
  // exist then, and the listener outlived the element it was attached to.
  const box = useRef<HTMLDivElement | null>(null);
  const following = useRef(true);

  useEffect(() => {
    const scroller = box.current;
    if (!scroller || !active) return;

    const onScroll = () => {
      const slack = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
      following.current = slack < 48;
    };
    scroller.addEventListener("scroll", onScroll, { passive: true });

    // scrollTop, NEVER scrollIntoView. scrollIntoView scrolls every
    // scrollable ancestor, so following the newest round also dragged the
    // whole page. Setting scrollTop moves this box and nothing else.
    const stick = () => {
      if (!following.current) return;
      scroller.scrollTop = scroller.scrollHeight;
    };
    stick();

    // DRIVEN BY LAYOUT, not by a number derived from the data. Deriving it
    // meant naming in advance which field's growth counts, and the first
    // attempt counted the round's `content` — so a phase streaming a long
    // THINKING block grew for a minute without the effect ever re-running,
    // and the box sat still while text poured into it. A size observer fires
    // for every reason the content can get taller: a fragment landing, a new
    // round, a disclosure opening, the window narrowing and text rewrapping.
    // Guarded because jsdom has no ResizeObserver: the box then simply does
    // not follow, which is the same as a phase that is not live.
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(stick);
    if (observer) {
      for (const child of Array.from(scroller.children)) observer.observe(child);
    }
    return () => {
      scroller.removeEventListener("scroll", onScroll);
      observer?.disconnect();
    };
  }, [active]);

  return box;
}

/** One round: thinking, speech, then the calls that round asked for. */
function RoundBlock({ round, live }: { round: Round; live: boolean }) {
  const said = round.content.trim();
  const thinking = round.reasoning.trim();
  // Marked on the ROUND, not just on the row inside it: "which round went
  // wrong" is the question a reader brings to a stuck turn, and the answer
  // used to be an icon inside a collapsed row they had to open to find.
  const errored = round.tools.some((t) => t.failed);
  return (
    <li className={cx("round", errored && "errored", live && "live")}>
      <div className="round-rail">
        {/* The numeral is decorative — the rail draws it as a node — but WHICH
            round this is is the only thing tying the blocks below together,
            and a reader who cannot see the rail was getting "1", then two
            unrelated-sounding disclosures. Announced here, hidden from the
            node so it is not read twice. */}
        <span className="sr-only">Round {round.round}</span>
        <span className="round-node t-num" aria-hidden="true">
          {round.round}
        </span>
      </div>
      <div className="round-body">
        {/* An attempt a provider gave up on partway through. KEPT, not
            erased: a reader has already seen this text, and making it vanish
            reads as a glitch — while "this model wrote four hundred
            characters and then died" is exactly what an operator debugging a
            flaky provider needs. */}
        {round.abandoned.map((a, i) => (
          <div key={i} className="abandoned">
            <div className="t-caption">
              <WarningGlyph size="xs" /> this attempt was abandoned mid-answer and retried
            </div>
            {a.reasoning.trim() && <p className="prose muted">{a.reasoning.trim()}</p>}
            {a.content.trim() && <p className="prose muted">{a.content.trim()}</p>}
          </div>
        ))}
        {thinking &&
          (round.streaming ? (
            // Open while it streams: a collapsed disclosure whose only sign
            // of life is a character count is not "watching it think". It
            // carries `round-thinking` so it sits on the round's own left
            // edge — the same edge the disclosure that replaces it sits on,
            // so the block does not shift sideways when the round commits.
            <div className="col gap-1 round-thinking">
              <div className="t-label">Thinking</div>
              <p className="prose muted stream">{thinking}</p>
            </div>
          ) : (
            // `aside` IS our `tone="reasoning"`, and uilet describes it in our
            // own words — "what was considered rather than what was decided",
            // a rule down its edge so a reader can skip the block by its shape.
            // `round-thinking` stays on it for the reason the streaming branch
            // above gives: both sit on the round's own left edge, so the block
            // does not shift sideways when the round commits.
            <Disclosure
              className="round-thinking"
              title="Thinking"
              count={`${thinking.length} chars`}
              variant="aside"
              headingLevel="none"
              lazy
            >
              <p className="prose muted">{thinking}</p>
            </Disclosure>
          ))}
        {said && <p className={cx("prose", round.streaming && "stream")}>{said}</p>}
        {round.tools.length > 0 && (
          <div className="round-tools">
            {round.tools.map((t, i) => (
              <ToolRow key={`${t.name}-${i}`} {...t} />
            ))}
          </div>
        )}
      </div>
    </li>
  );
}

export function PhaseCard({
  record,
  defaultOpen,
  showRole,
  nested,
}: {
  record: PhaseRecord;
  defaultOpen?: boolean;
  showRole?: boolean;
  /** The calls this phase made — the workers a `delegate` call ran, the
      round-cap judge. Rendered INSIDE this card, because that is what
      `host_phase` has always meant and rendering them as siblings left
      the reader working out which round each belonged to. */
  nested?: PhaseRecord[];
}) {
  // Latched: seeded from `defaultOpen` and then owned by the reader. A phase
  // completing is not a reason to hide it.
  const [open, setOpen] = useState(!!defaultOpen);
  const now = useNow();
  const { ledger, legacy } = ledgerOf(record);
  const streaming = ledger.some((r) => r.streaming);
  const stale = record.live ? staleness(record.at, now) : "";
  const took = phaseDuration(record);
  const onOwnEventPage = useIsCurrent(["events", record.eventId]);
  // The last round is the live one while the phase runs: rounds only append,
  // so "newest" and "last" are the same row and stay the same row.
  const tailRef = useTail(open && record.live);

  return (
    <article
      className={cx(
        "phase-card",
        record.failed && "failed",
        record.live && "live",
        streaming && "streaming",
      )}
    >
      <header className="phase-head" onClick={() => setOpen((v) => !v)}>
        {open ? <KeyboardArrowDownGlyph size="xs" /> : <ChevronRightGlyph size="xs" />}
        <PhaseTag phase={record.phase} />
        {record.iteration > 1 && (
          <span className="t-caption" title="self-iterate round">
            iter {record.iteration}
          </span>
        )}
        {/* WHICH task and WHICH template. A delegate call of eight
            otherwise produces eight identical-looking rows, and the one
            the reader wants is the one that failed. */}
        {record.taskId && (
          <span className="t-cell mono truncate" title="delegated task id">
            {record.taskId}
          </span>
        )}
        {/* NEUTRAL, because a worker template is an identity. uilet's tone doc
            states the rule this card already kept: a tone says what a thing IS,
            never who it is. */}
        {record.worker && (
          <Tag appearance="outline" monospace title="worker template">
            {record.worker}
          </Tag>
        )}
        {!!nested?.length && (
          <Tag appearance="outline" title="calls this phase made">
            {nested.length} {nested.length === 1 ? "worker" : "workers"}
          </Tag>
        )}
        {showRole && record.role && <span className="t-cell truncate">{record.role}</span>}

        <span className="spacer" />

        {/* Everything below is present on BOTH a live and a finished phase, in
            the same order, so the row does not reshape when it completes. */}
        {/* THE DECISION IS THE PHASE'S OWN STATE, so it takes the state's tone
            — the design doc's rule for the turn header's outcome tile, which
            this chip is the per-phase form of. It was `=== "self_iterate" ?
            warning : neutral`, keyed on ONE value, so `blocked` (the executor
            reporting it could not do the work), the engine-written `incomplete`
            and the reviewer's `failed` all drew the ordinary grey pill. `failed`
            is the one that hid worst: a review record never sets the phase's own
            `failed` flag, so the danger tag above never fires for it and a turn
            the reviewer ended carried no red anywhere on the card. The table is
            in lib/phases.ts beside the words, because a decision's sentence and
            its hue are one fact. */}
        {record.decision && (
          <Tag variant={uiletTone(decisionTone(record.phase, record.decision))}>
            {decisionLabel(record.phase, record.decision)}
          </Tag>
        )}
        {record.exhaustedRounds && (
          <Tag variant="warning" title="the phase ran out of tool rounds">
            round cap
          </Tag>
        )}
        {record.emptyAnswerRounds > 0 && (
          <Tag
            variant="warning"
            title="the model answered with nothing — no response and no tool call — and was re-asked"
          >
            {record.emptyAnswerRounds} empty
          </Tag>
        )}
        {/* THE PHASE'S TEXT IS NOT ITS FINISHED ANSWER. A length stop arrives
            as an ordinary 200 with a short body, so without this chip a phase
            whose prose stops mid-word and one that decided it was done are the
            same card — and it is the transcript BELOW this header that the
            reader is about to take at face value. Latched over the phase
            rather than per round, which is how the loop records it: a severed
            round taints what the phase stands behind however many clean rounds
            follow. Beside the round cap because they are the two ways a phase
            ends short, and it is `danger` rather than `warning` because the
            round cap merely stopped the loop while this cut the answer itself.

            NOTHING IS RECOVERED BY CLICKING: what survives IS `response`,
            whole, and the rest was never generated. The fix is the entry's
            own `max_output_tokens`, which is what the title says. */}
        {record.outputTruncated && (
          <Tag
            variant="danger"
            title="the model hit its output cap mid-answer — what is below stops short of what it was writing; raise max_output_tokens on this provider entry"
          >
            output cut
          </Tag>
        )}
        {record.rescueFired && (
          <Tag variant="warning" title="the phase did not submit on its first run and was re-asked">
            rescued
          </Tag>
        )}
        {record.backend === "sandbox" && (
          <Tag variant="info" leadingIcon={<TerminalGlyph />}>
            {record.codingAgent || "sandbox"}
          </Tag>
        )}
        {record.failed && <Tag variant="danger">{record.errorKind || "failed"}</Tag>}
        {record.live && (
          <Tag variant={stale === "stalled" ? "danger" : stale ? "warning" : "info"} dot>
            {stale === "stalled" ? "no update in 10m" : stale ? "no update in 2m" : "running"}
          </Tag>
        )}

        <span className="phase-meta mono">
          {record.model || <EmptyValue label="No model recorded" />}
        </span>
        {/* AND THE SAME RULE ONE FIELD EARLIER. A settled phase whose
            `rounds_used` is 0 took no tool round — which is what a phase that
            died before its first call came back looks like, and is the fact
            that explains the failure below it. Rendering "—" there said the
            engine had not recorded the rounds when it recorded none.

            ON `roundsUsed`, which is the engine's own count on a live phase as
            well as a settled one. This tested `roundNum === 0`, a field that was
            zero-based while live and so held `-1` at the opening frame: the
            phase whose first round had not come back — precisely the case this
            dash is for — fell through and rendered "0r". And the count below it
            was one short on a live phase whose rounds narrated nothing. */}
        {record.live && !ledger.length && record.roundsUsed === 0 ? (
          <span className="phase-meta" title="this phase has not finished a round yet">
            —
          </span>
        ) : (
          <span className="phase-meta t-num" title="tool rounds used">
            {`${Math.max(ledger.length, record.roundsUsed)}r`}
          </span>
        )}
        {/* ZERO IS A NUMBER, AND ONLY A LIVE PHASE'S ZERO IS AN ABSENCE. This
            read `totalTokens ? … : "—"`, so a phase that genuinely spent
            nothing — one on a subscription CLI backend, which reports no
            usage at all, or one the engine stopped before its first call came
            back — rendered as "not recorded" on the card an operator opens to
            find out which phase was expensive. `TurnCard` states the rule for
            the turn header above it: absent and zero are different facts and a
            dash claims the first about the second. The dash stays for the one
            case where the zero really is an absence. */}
        {record.live && record.totalTokens === 0 ? (
          <span className="phase-meta" title="this phase has not reported its usage yet">
            —
          </span>
        ) : (
          <span className="phase-meta t-num" title="total tokens">
            {fmtCount(record.totalTokens)}
          </span>
        )}
        {/* HOW LONG THIS PHASE TOOK, straight off `duration_ms` — the
            engine measures the phase where the clock is and puts the answer
            on the record. On a self-iterating turn that is the number that
            says WHICH round was expensive, which is the question the token
            total makes a reader ask and could not answer. Present on a
            NESTED call too, now: a worker and the round-cap judge publish no
            start event, so the pairing this replaced could never give one a
            duration and "which worker was slow" had no answer anywhere. */}
        {took != null && (
          <span className="phase-meta t-num" title="how long this phase took">
            {fmtDuration(took)}
          </span>
        )}
        {/* Running for HOW LONG, or landed WHEN. A live phase measured
            against `at` — which moves on every streamed frame — flickered
            between "just now" and "in 1s" as the two clocks crossed. */}
        <time
          className="phase-meta"
          dateTime={record.live ? record.startedAt : record.at}
          title={fmtDateTime(record.live ? record.startedAt : record.at)}
        >
          {record.live ? fmtElapsed(now - tsKey(record.startedAt)) : relTime(record.at, now)}
        </time>
      </header>

      {open && (
        <div className="phase-body">
          {record.failed && record.error && <Callout variant="danger">{record.error}</Callout>}
          {record.notes && <Callout variant="neutral">{record.notes}</Callout>}

          {(record.systemPrompt || record.userPrompt) && (
            // A REAL SECTION OF THIS CARD, so it keeps uilet's default heading
            // — unlike a tool row, which is a transcript item. `lazy` matches
            // what ours did: a closed fold mounted nothing, and a seat's system
            // prompt is tens of kilobytes nobody asked for.
            //
            // What is INSIDE it is a document rather than a wall of text now:
            // `PromptRecord` folds each half on its own markdown headings and
            // renders each one, so "what was this phase told about X" is one
            // click rather than a scroll through 30 kB. It keeps the verbatim
            // record as its other view — the tallest block on the page by a
            // wide margin, and the one that most needed to stay one selection.
            <Disclosure title="Prompt" count={`${record.phase} phase`} lazy>
              <PromptRecord
                phase={record.phase}
                system={record.systemPrompt}
                user={record.userPrompt}
              />
            </Disclosure>
          )}

          {(record.toolsAvailable.length > 0 || record.toolCatalogue.length > 0) && (
            <Disclosure
              title="Tool surface"
              count={record.toolsAvailable.length + record.toolCatalogue.length}
              lazy
            >
              <div className="col gap-2">
                {record.toolsAvailable.length > 0 && (
                  <div className="col gap-1">
                    <div className="t-label">
                      Callable this round
                      <span className="muted"> · full JSON schemas were sent</span>
                    </div>
                    <div className="row wrap gap-1">
                      {/* A TOOL NAME IS AN IDENTITY, so it stays neutral —
                          uilet's tone doc names a tool among the four things
                          that must. */}
                      {record.toolsAvailable.map((t) => (
                        <Tag key={t} monospace appearance="outline">
                          {t}
                        </Tag>
                      ))}
                    </div>
                  </div>
                )}
                {record.toolCatalogue.length > 0 && (
                  <div className="col gap-1">
                    <div className="t-label">
                      Offered as prose
                      <span className="muted"> · discoverable, not yet callable</span>
                    </div>
                    <div className="row wrap gap-1">
                      {record.toolCatalogue.map((t) => (
                        <Tag key={t} monospace appearance="outline">
                          {t}
                        </Tag>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            </Disclosure>
          )}

          {/* The transcript. One block per round — thought, speech, calls —
              in the order they happened. Rounds append, so nothing above an
              insertion can move, which is the whole point. */}
          {ledger.length > 0 && (
            <section className="col gap-1">
              <div className="t-label">
                Rounds
                <span className="muted">
                  {" · "}
                  what the model thought, said and called, in order
                </span>
              </div>
              <div ref={tailRef} className={cx("tail-scroll", record.live && "tailing")}>
                <ol className="round-ledger">
                  {ledger.map((r, i) => (
                    <RoundBlock
                      key={r.round}
                      round={r}
                      live={record.live && i === ledger.length - 1}
                    />
                  ))}
                </ol>
              </div>
            </section>
          )}

          {/* A phase recorded before the engine sent per-round narration. The
              join cannot be undone, so it is shown whole rather than guessed
              apart — see `ledgerOf`. */}
          {legacy && (
            <>
              {legacy.thinking && (
                <Disclosure
                  title="Thinking"
                  count={`${legacy.thinking.length} chars`}
                  variant="aside"
                  headingLevel="none"
                  lazy
                >
                  <p className="prose muted">{legacy.thinking}</p>
                </Disclosure>
              )}
              {legacy.answer.trim() && (
                <section className="col gap-1">
                  <div className="t-label">
                    Transcript
                    <span className="muted"> · recorded before rounds were kept apart</span>
                  </div>
                  <p className="prose">{legacy.answer.trim()}</p>
                </section>
              )}
            </>
          )}

          {/* The only genuinely empty state. A ROUND is never empty —
              `narrations()` drops an entry blank in both fields and a round
              built from a tool call has tools — so a per-round placeholder
              was unsatisfiable. A PHASE with no rounds yet is real: the
              provider call has not returned, and until the engine streams
              tokens there is nothing else to show for it. */}
          {record.live && !ledger.length && !legacy && (
            <div className="row gap-2">
              <span className="waiting-dot" aria-hidden="true" />
              <span className="t-caption">
                The model is composing its first round. Nothing is published until it answers.
              </span>
            </div>
          )}

          {!!nested?.length && (
            <Disclosure
              title="Delegated to"
              count={`${nested.length} · ${fmtCount(
                nested.reduce((n, r) => n + r.totalTokens, 0),
              )} tokens`}
              defaultOpen
              lazy
            >
              <div className="phase-nest">
                {nested.map((r) => (
                  <PhaseCard key={r.key} record={r} />
                ))}
              </div>
            </Disclosure>
          )}
          <footer className="phase-foot">
            <span className="t-caption">
              {record.inputTokens ? `${fmtCount(record.inputTokens)} in` : ""}
              {record.outputTokens ? ` · ${fmtCount(record.outputTokens)} out` : ""}
              {record.providerKey ? ` · provider ${record.providerKey}` : ""}
            </span>
            <span className="spacer" />
            {/* The key, not a link. It named a thread on a screen that no
                longer views threads — and it is still worth showing, because
                it says WHICH external conversation this turn served. */}
            {record.conversationKey && (
              <span className="t-caption mono" title="the conversation this turn served">
                {record.conversationKey}
              </span>
            )}
            {/* NOT ON THE EVENT'S OWN PAGE. This card is rendered on the
                turn, on the seat and on the event itself, and only the first
                two are somewhere else — on the third the link points at the
                page already open, so a reader clicks it, nothing moves, and
                the only thing they learn is that the control was a lie. */}
            {record.eventId && !onOwnEventPage && (
              <a
                className="t-link"
                href={href(["activity", "events", record.eventId])}
                title="this phase's own event, in the log"
              >
                event →
              </a>
            )}
          </footer>
        </div>
      )}
    </article>
  );
}
