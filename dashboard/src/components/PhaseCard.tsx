/**
 * One phase of one turn, rendered as a stable round ledger.
 *
 * Five rules, each of which fixes a specific way the previous surface either
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
 */

import { useEffect, useRef, useState } from "react";
import { PhaseTag } from "./PhaseTag.tsx";
import { fmtCount, fmtDateTime, fmtDuration, tsKey } from "~/lib/format.ts";
import {
  decisionLabel,
  ledgerOf,
  phaseDuration,
  type PhaseRecord,
  type Round,
} from "~/lib/phases.ts";
import { staleness } from "~/lib/seats.ts";
import { href, useIsCurrent } from "~/app/router.tsx";
import { CodeBlock, Disclosure, RelativeTime, Tag, cx, useNow } from "@crewlethq/ui";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";
import {
  ChevronRightGlyph,
  ErrorGlyph,
  InfoGlyph,
  KeyboardArrowDownGlyph,
  TerminalGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";

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
        title={name}
        // THE WORD, not a red glyph. A failed call was marked with a glyph
        // that carried no name at all, so a screen reader was told nothing
        // about it, and the hue was the only signal a sighted reader got.
        // Inside the title the mark also wrapped, putting the alert on its
        // own line above the tool; the design system draws it after the name,
        // as part of what the control is called.
        meta={failed ? "failed" : undefined}
      >
        <div className="col gap-1">
          <div className="t-label">Arguments</div>
          <CodeBlock plain wrap code={args || "{}"} maxHeight={RECORD_MAX_HEIGHT} />
          <div className="t-label">{failed ? "Error" : "Result"}</div>
          <CodeBlock plain wrap code={result || "(empty)"} maxHeight={RECORD_MAX_HEIGHT} />
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
            <Disclosure title="Thinking" count={`${thinking.length} chars`} variant="aside">
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
        {record.decision && (
          <Tag variant={record.decision === "self_iterate" ? "warning" : "neutral"}>
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

        <span className="phase-meta mono">{record.model || "—"}</span>
        <span className="phase-meta t-num" title="tool rounds used">
          {ledger.length || record.roundNum > 0
            ? `${Math.max(ledger.length, record.roundNum)}r`
            : "—"}
        </span>
        <span className="phase-meta t-num" title="total tokens">
          {record.totalTokens ? fmtCount(record.totalTokens) : "—"}
        </span>
        {/* HOW LONG THIS PHASE TOOK. Only derivable since the phase's own
            `agent_phase_started` is folded onto its record (see `withStarts`)
            — `agent_phase_completed` carries the instant it landed and
            nothing else, so a finished phase had no duration anywhere on this
            dashboard. On a self-iterating turn that is the number that says
            WHICH round was expensive, which is the question the token total
            makes a reader ask and could not answer. Absent on a nested call,
            which publishes no start. */}
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
          {record.live ? (
            <RelativeTime mode="elapsed" value={record.startedAt} now={now} />
          ) : (
            <RelativeTime value={record.at} now={now} />
          )}
        </time>
      </header>

      {open && (
        <div className="phase-body">
          {record.failed && record.error && (
            <div className="banner critical">
              <ErrorGlyph size="sm" />
              <span>{record.error}</span>
            </div>
          )}
          {record.notes && (
            <div className="banner neutral">
              <InfoGlyph size="sm" />
              <span>{record.notes}</span>
            </div>
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

          {(record.systemPrompt || record.userPrompt) && (
            <Disclosure title="Prompt" count={`${record.phase} phase`}>
              <div className="col gap-3">
                {record.systemPrompt && (
                  <div className="col gap-1">
                    <div className="t-label">System</div>
                    <CodeBlock
                      plain
                      wrap
                      code={record.systemPrompt}
                      maxHeight={RECORD_MAX_HEIGHT}
                    />
                  </div>
                )}
                {record.userPrompt && (
                  <div className="col gap-1">
                    <div className="t-label">User</div>
                    <CodeBlock plain wrap code={record.userPrompt} maxHeight={RECORD_MAX_HEIGHT} />
                  </div>
                )}
              </div>
            </Disclosure>
          )}

          {(record.toolsAvailable.length > 0 || record.toolCatalogue.length > 0) && (
            <Disclosure
              title="Tool surface"
              count={record.toolsAvailable.length + record.toolCatalogue.length}
            >
              <div className="col gap-2">
                {record.toolsAvailable.length > 0 && (
                  <div className="col gap-1">
                    <div className="t-label">
                      Callable this round
                      <span className="muted"> · full JSON schemas were sent</span>
                    </div>
                    <div className="row wrap gap-1">
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

          {!!nested?.length && (
            <Disclosure
              title="Delegated to"
              count={`${nested.length} · ${fmtCount(
                nested.reduce((n, r) => n + r.totalTokens, 0),
              )} tokens`}
              defaultOpen
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
                href={href(["events", record.eventId])}
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
