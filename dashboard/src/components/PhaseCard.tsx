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
 *  3. **Each round is one block: what the model said, then the tools it
 *     called** — in that order, which is the order they happened. Rounds only
 *     append, so nothing above the insertion point can move.
 *
 *     The engine sends `round_narration` for exactly this. Before it did, the
 *     only text was `response`, the JOIN of every round's turn, and a join
 *     cannot be undone: prose contains blank lines, so nothing could say which
 *     round said what, and a reader saw the model's words in a block detached
 *     from the tool calls they were about.
 *
 *     Grouping the data was only half of it: the block has to LOOK like one
 *     block. It did not, and a reader said so — the words and the call they
 *     asked for read as two unrelated rows. The devices meant to
 *     bound a round both described the sequence instead (a rail drawn only
 *     between rounds, a tint only on even ones), so a phase with a single
 *     round had neither. The rail is a bracket around each round now and the
 *     round's content shares one left edge; see `.round` in screens.css.
 *
 *  4. **The model's words are set as prose, not as code — and as the markdown
 *     they are.** Its speech and an executor's report get a
 *     proportional face, a real line height and a bounded measure, and are
 *     rendered as markdown read by a model's habits (`Words`): its fenced
 *     JSON is a code block rather than three backticks and a wall of braces,
 *     its single newlines are line breaks, and its headings are styled lines
 *     rather than headings of the page. Monospace stays where it means
 *     something: tool arguments and tool results, which are JSON.
 *  5. **The header says the same things in the same places, always** — phase,
 *     model, rounds, tokens, decision — so a live phase and a finished one are
 *     the same shape and the row does not change height when it completes.
 *  6. **The prompt is a document, not a wall of text.** Every prompt this
 *     engine builds is markdown with parts its builder can name, so the Prompt
 *     fold reads it through a map of the whole request, an outline of its
 *     sections (the builder's own map where the record carries one, the
 *     headings otherwise) and ONE section at a time rendered as the markdown it
 *     is, with a find over all of it — and keeps the verbatim record as its
 *     other view. See `PromptDoc.tsx`. It was one 30 kB scroller, which is
 *     where a reader went to answer "what was this phase told about X" and
 *     scrolled looking for a heading; then a stack of folds cut at every `##`,
 *     including the ones inside the content a prompt quotes.
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

import { useEffect, useMemo, useRef, useState } from "react";
import {
  ArtifactKindGlyph,
  Callout,
  CodeBlock,
  cx,
  Disclosure,
  EmptyValue,
  Link,
  Tag,
} from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  ChevronDownGlyph,
  SquareTerminalGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
// STILL OURS. `PhaseTag` HAS a peer — uilet's `Tag` carries `phase-onboarding`,
// `phase-execute` and `phase-review` — but it is a primitive in `~/ui`, and
// porting it THERE moves this card, the turn card and the seat screen in one
// change rather than leaving three inlined copies of one variant table behind.
import { PhaseTag, uiletTone } from "~/ui/primitives.tsx";
import {
  fmtBytes,
  fmtCount,
  fmtDateTime,
  fmtDuration,
  fmtElapsed,
  plural,
  relTime,
  tsKey,
  utf8Bytes,
} from "~/lib/format.ts";
import {
  decisionLabel,
  decisionTone,
  deliveredLine,
  outcomeWords,
  phaseDuration,
  roundTokens,
  runGroups,
  transcriptLength,
  type Invocation,
  type PhaseRecord,
  type Round,
  rounds,
} from "~/lib/phases.ts";
import { indentJSON } from "~/lib/jsontext.ts";
import { MODEL_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { failureWords, indexOrg, seatPath, staleness } from "~/lib/seats.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useNow } from "~/lib/clock.ts";
import { href, useIsCurrent } from "~/app/router.tsx";
import { pathOf } from "~/app/frame/objects.ts";
import { PromptRecord } from "~/components/PromptDoc.tsx";
import { WITHHELD_WHY, WITHHELD_WORDS } from "~/lib/access.ts";
import { artifactPath } from "~/app/routes.ts";
import { writtenArtifact, type WrittenArtifact } from "~/lib/artifacts/transcript.ts";

/**
 * A call's two records, formatted.
 *
 * ITS OWN COMPONENT, mounted INSIDE the lazy disclosure, and that placement is
 * the whole reason it exists. `useMemo` in the row above would run for every
 * row on the phase's first render however many were closed, because `lazy`
 * defers a disclosure's CHILDREN and not its parent's body — so a forty-round
 * phase paid the full parse of every argument and every result before the
 * reader opened anything. Here the work happens when a row is opened, which is
 * what the flag was for.
 *
 * INDENTED FOR READING, NEVER RE-ENCODED. The engine records a call's
 * arguments compactly, so what arrives is the whole call on ONE line with the
 * argument a reader came for somewhere in the middle of it. Every other JSON
 * block in this product is written at two spaces, and these were the exception
 * rather than a decision. `indentJSON` walks the TEXT and copies every literal
 * across byte for byte, which is what the alternative — decode, re-encode —
 * cannot do: it rounds an id wider than 2^53 to a different id. Its own doc
 * has the rest, including why a long string VALUE is the wrapping's problem
 * rather than the indent's.
 *
 * Still memoised, because a live phase re-broadcasts twice per round and an
 * OPEN row is re-rendered every time.
 */
function ToolRecords({
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
  const prettyArgs = useMemo(() => indentJSON(args), [args]);
  // THE RESULT TOO, by the same rule: an MCP server's answer is a JSON
  // document as often as the call was. What is not one — a stack trace, a
  // sentence, a page of markdown — comes back untouched.
  const prettyResult = useMemo(() => indentJSON(result), [result]);
  return (
    <div className="col gap-1">
      <div className="t-label">Arguments</div>
      {/* NAMED WITH THE TOOL. A screen reader landing on a scrollable block
          announces the name and nothing around it, and half a dozen regions
          called "Arguments" on one round is the same as none.

          `plain` MEANS SOMETHING ELSE OVER HERE: ours turned wrapping off,
          theirs drops the header. The header is what goes — the block is bare
          in this design.

          WRAPPING IS BACK ON, and indenting is what pays for it. It was off
          because these arguments were "aligned JSON", which one minified line
          never was: the block simply scrolled sideways. Indented, the
          structure is carried by the newlines, nothing is aligned in columns
          any more, and the only thing left that can overrun the box is a
          single long string value — a message body, a page of markdown —
          which is exactly uilet's own case for wrapping, a line nobody finds
          the end of.

          NO `maxHeight` HERE, and that is the ceiling working: the record
          ceiling is declared once in the cascade (`--record-ceiling`,
          frame.css) as what the view allows, and a running ledger narrows it
          to what its box allows (`.tail-scroll.tailing`), because a record
          taller than the box that scrolls it is the transcript's own trap one
          level down — the wheel inside the record while its bottom is below
          the box's. */}
      <CodeBlock plain selectable label={`${name} · arguments`} code={prettyArgs || "{}"} />
      <div className="t-label">{failed ? "Error" : "Result"}</div>
      <CodeBlock
        plain
        selectable
        label={`${name} · ${failed ? "error" : "result"}`}
        code={prettyResult || "(empty)"}
      />
    </div>
  );
}

/**
 * What a call still running shows opened: what it was handed, and that it has
 * no result yet — said, rather than drawn as an empty result, which reads as a
 * call that returned nothing. A withheld body has no arguments to show, and
 * the card's own note already says what was withheld.
 */
function RunningRecords({ name, args }: { name: string; args: string }) {
  const prettyArgs = useMemo(() => indentJSON(args), [args]);
  return (
    <div className="col gap-1">
      {args && (
        <>
          <div className="t-label">Arguments</div>
          <CodeBlock plain selectable label={`${name} · arguments`} code={prettyArgs} />
        </>
      )}
      <span className="t-caption">Still running: its result lands here when it returns.</span>
    </div>
  );
}

/**
 * One tool call of a round — answered, or still running (`running`).
 *
 * ONE COMPONENT FOR BOTH, because a call is one row for its whole life: a CLI
 * runs a response's calls side by side, each listed while it runs and answered
 * when its result lands, and a row that changed component as it answered would
 * be unmounted and mounted again, closing what the reader had opened.
 */
function ToolRow({
  name,
  args,
  result,
  failed,
  running,
}: {
  name: string;
  args: string;
  result: string;
  failed: boolean;
  /** The call is still in flight: since when, and the clock it is read on. */
  running?: { since: number; now: number };
}) {
  return (
    <div className={cx("tool-row", failed && "failed", running && "running")}>
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
        //
        // A CALL STILL RUNNING says so the same way, with how long: a call
        // four minutes in is the answer to "why is this phase slow", and it
        // should not take opening the row to find it.
        meta={
          running
            ? running.since > 0
              ? `running ${fmtElapsed(running.now - running.since)}`
              : "running"
            : failed
              ? "failed"
              : undefined
        }
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
        {running ? (
          <RunningRecords name={name} args={args} />
        ) : (
          <ToolRecords name={name} args={args} result={result} failed={failed} />
        )}
      </Disclosure>
      {!running && <WrittenArtifactLink artifact={writtenArtifact(name, args, result, failed)} />}
    </div>
  );
}

/**
 * The head of one CLI run's rounds: which entry served it, how many rounds it
 * made, how long it lived, what it spent and how it ended — or, for the run
 * still in flight, that it is running.
 *
 * A RUN IS WHAT THE ENGINE STARTS AND ENDS, where a round is one model request
 * the CLI's record shows; most phases are one run, and a phase that was
 * steered, delegated or corrected is several — each a CLI process handed the
 * conversation so far. A run whose CLI showed no requests is ONE round, and its
 * head says so, since that round's timing is the run's.
 */
function RunHead({
  run,
  ordinal,
  live,
}: {
  run: Invocation | null;
  ordinal: number;
  live: boolean;
}) {
  if (!run) {
    return (
      <div className="run-head">
        <span className="run-name">CLI run {ordinal}</span>
        {live && <Tag variant="info">running</Tag>}
      </div>
    );
  }
  const ended = outcomeWords(run.outcome);
  const made = run.firstRound > 0 ? run.lastRound - run.firstRound + 1 : 0;
  const tokens = run.inputTokens + run.outputTokens;
  return (
    <div className="col gap-1">
      <div className="run-head">
        <span className="run-name">CLI run {run.run}</span>
        {run.providerKey && (
          <span className="t-caption mono" title="the providers.llm entry that served it">
            {run.providerKey}
          </span>
        )}
        <span className="t-caption t-num" title="the rounds this run made">
          {made === 0
            ? "no request"
            : made === 1
              ? `round ${run.firstRound}`
              : `rounds ${run.firstRound}–${run.lastRound}`}
        </span>
        {run.durationMs > 0 && (
          <span className="t-caption t-num" title="how long the CLI ran">
            {fmtDuration(run.durationMs)}
          </span>
        )}
        {tokens > 0 && (
          <span className="t-caption t-num" title="what the run spent">
            {`${fmtCount(tokens)} tokens`}
          </span>
        )}
        <Tag variant={uiletTone(ended.tone)} appearance="outline">
          {ended.label}
        </Tag>
      </div>
      {!run.requestsObserved && made > 0 && (
        <p className="round-note">
          Its CLI’s record showed no requests, so the whole run is one round.
        </p>
      )}
    </div>
  );
}

/**
 * The artifact a call wrote, as a link beside the call: a version that landed
 * is the outcome a reader came to the transcript for, and an id inside a
 * folded result is not a way to reach it. Outside the disclosure, so a closed
 * row still says where its work went.
 */
function WrittenArtifactLink({ artifact }: { artifact: WrittenArtifact | null }) {
  if (!artifact) return null;
  const named = artifact.title ? `“${artifact.title}”` : "the artifact";
  return (
    <Link
      className="tool-row-artifact"
      variant="quiet"
      size="caption"
      href={href(artifactPath(artifact.id))}
    >
      {artifact.kind && <ArtifactKindGlyph kind={artifact.kind} size="xs" />}
      {`Open ${named}, version ${artifact.version}`}
    </Link>
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
 *
 * Returned as a CALLBACK REF, and the element is state. A ref object only says
 * where the box is once something re-runs the effect, and the box is mounted
 * conditionally — the rounds section exists only once a round does — so a live
 * phase opened before its first round came back mounted the box AFTER the
 * effect had run against nothing and never followed at all.
 */
function useTail(active: boolean) {
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);
  const following = useRef(true);

  useEffect(() => {
    if (!scroller || !active) return;
    // EVERY ACTIVATION STARTS AT THE TAIL. The flag outlives the element — a
    // reader who scrolled up, closed the card and opened it again got a NEW
    // box that inherited "not following" from the old one and never stuck
    // again. Opening a live phase is asking to watch it.
    following.current = true;

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
    // attempt counted the round's `content` — so a phase streaming into a
    // block that field did not cover grew for a minute without the effect
    // ever re-running, and the box sat still while text poured into it. A size observer fires
    // for every reason the content can get taller: a fragment landing, a new
    // round, a disclosure opening, text rewrapping.
    //
    // THE BOX ITSELF AS WELL AS WHAT IT HOLDS. Its bound is the scroller's
    // view (`.tail-scroll.tailing` in screens.css), so a window made shorter
    // or taller resizes the BOX while its content stays the size it was —
    // and a tail measured only on the children came unstuck by the
    // difference, leaving the newest round below the box's new bottom.
    //
    // Guarded because jsdom has no ResizeObserver: the box then simply does
    // not follow, which is the same as a phase that is not live.
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(stick);
    if (observer) {
      observer.observe(scroller);
      for (const child of Array.from(scroller.children)) observer.observe(child);
    }
    return () => {
      scroller.removeEventListener("scroll", onScroll);
      observer?.disconnect();
    };
  }, [scroller, active]);

  return setScroller;
}

/**
 * What became of a round that answered in prose where the phase had to end in
 * a tool call — derived from the ledger rather than carried on the wire,
 * because the ledger already says it: a later round means the engine asked
 * again, and a settled phase whose LAST round declined spent its bound there.
 * `open` is the one case nothing can say yet: the newest round of a phase still
 * running, which may be asked again or may have been the last.
 */
type Declined = "asked" | "spent" | "open";

/**
 * The sentence under a declined round's words.
 *
 * IN WORDS, NOT ONLY A COLOUR, and it names the mechanism, because what a
 * reader sees without it is a model printing its own submission — a fenced
 * `{"outcome": "delivered", …}` block — as though that were the answer, and
 * nothing saying why the engine did not take it: prose is not a call, and
 * this phase finishes only by one.
 */
function DeclinedNote({ fate, rescued }: { fate: Declined; rescued: boolean }) {
  const said = "It answered in prose and called no tool";
  if (fate === "asked") {
    return <p className="round-note">{said}. It didn’t submit, so it was asked again.</p>;
  }
  if (fate === "open") {
    return <p className="round-note">{said}. It didn’t submit.</p>;
  }
  // CAUTION, NOT CRITICAL: the turn goes on — a rescued executor is still
  // judged by the reviewer, a rescued reviewer sends the turn round again. The
  // glyph carries the hue's meaning to a reader who cannot see the hue.
  return (
    <p className="round-note caution">
      <TriangleAlertGlyph size="xs" /> {said}. Last round
      {rescued ? ", so the engine filled in the outcome." : "."}
    </p>
  );
}

/**
 * A model's words — its speech, an executor's report — as the markdown it
 * wrote.
 *
 * MARKDOWN, because that is what models write: the design doc said a model's
 * speech "is not markdown" and a round's fenced ```json block was printed as
 * three backticks, a language tag and a wall of braces. Read by a MODEL'S
 * habits rather than a document's (`MODEL_WORDS`): every newline is the line
 * break it meant, and a "## Summary" is a styled line rather than an `h2` in
 * the turn page's outline — a transcript item is not a section of the page,
 * the rule a tool row's `headingLevel="none"` keeps one level up. `.prose`'s
 * measure and leading stay; `.md` hands line breaking to the renderer.
 *
 * Memoised on the text, because a live phase re-renders on every streamed
 * frame and an open round's words are re-parsed only when they changed.
 */
function Words({ text, streaming }: { text: string; streaming?: boolean }) {
  const nodes = useMemo(() => renderMarkdown(text, MODEL_WORDS), [text]);
  return <div className={cx("prose md", streaming && "stream")}>{nodes}</div>;
}

/**
 * The sentence under a round the CLI's own turn cap ended (`stop_reason: cap`).
 *
 * THE ONE STOP REASON THAT ENDS A ROUND UNFINISHED, which is why the reason
 * gate (`contract/stops.ts`) asks for a sentence: an answer and a set of calls
 * are rounds that finished and say nothing, while here the CLI stopped asking
 * with the model still working — the run ended for want of turns, not because
 * the model was done. CAUTION, as a declined round's last word is: the phase
 * goes on to whatever its cap decides.
 */
function CapNote() {
  return (
    <p className="round-note caution">
      <TriangleAlertGlyph size="xs" /> The CLI’s own turn cap ended its run here, with the model
      still working.
    </p>
  );
}

/** One round: what the model said, then the calls that round asked for. */
function RoundBlock({
  round,
  live,
  declined,
  rescued,
  capped,
  now,
}: {
  round: Round;
  live: boolean;
  /** What became of this round when it declined to call a tool; absent otherwise. */
  declined?: Declined;
  rescued: boolean;
  /** The CLI's own turn cap ended its run at this round. */
  capped: boolean;
  now: number;
}) {
  const said = round.content.trim();
  // Marked on the ROUND, not just on the row inside it: "which round went
  // wrong" is the question a reader brings to a stuck turn, and the answer
  // used to be an icon inside a collapsed row they had to open to find.
  const errored = round.tools.some((t) => t.failed);
  return (
    <li className={cx("round", errored && "errored", declined && "declined", live && "live")}>
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
            flaky provider needs.

            PLAIN TEXT, unlike every other block of a model's words here: it
            is EVIDENCE of a failure, cut wherever the provider died, and it
            never becomes the round's answer. Rendered, a cut mid-construct
            would be formatted into something the model never finished; the
            streaming text below is rendered because it IS about to become
            the committed answer, and rendering it now keeps the block from
            reshaping at the moment it commits. */}
        {round.abandoned.map((a, i) => (
          <div key={i} className="abandoned">
            <div className="t-caption">
              <TriangleAlertGlyph size="xs" /> this attempt was abandoned mid-answer and retried
            </div>
            {a.content.trim() && <p className="prose muted">{a.content.trim()}</p>}
          </div>
        ))}
        {said && <Words text={said} streaming={round.streaming} />}
        {declined && <DeclinedNote fate={declined} rescued={rescued} />}
        {capped && <CapNote />}
        {round.tools.length + round.running.length > 0 && (
          <div className="round-tools">
            {/* ONE COMPONENT FOR A CALL ANSWERED AND A CALL RUNNING, in ONE
                list, keyed by the call's own id where it has one — the id its
                running entry and its answered row share. So a call that
                answers is the SAME row, open if the reader opened it: React
                matches keys within one array only, and a row that moved from
                a list of running calls to a list of answered ones would be
                unmounted and another mounted in its place. */}
            {[
              ...round.tools.map((t, i) => (
                <ToolRow
                  key={t.callId ? `call-${t.callId}` : `${t.name}-${i}`}
                  name={t.name}
                  args={t.args}
                  result={t.result}
                  failed={t.failed}
                />
              )),
              ...round.running.map((c, i) => (
                <ToolRow
                  key={c.id ? `call-${c.id}` : `running-${c.name}-${i}`}
                  name={c.name}
                  args={c.arguments}
                  result=""
                  failed={false}
                  running={{ since: tsKey(c.startedAt), now }}
                />
              )),
            ]}
          </div>
        )}
      </div>
    </li>
  );
}

/**
 * Why a phase's body is hidden, naming who can read it: the seat's lead, by
 * name and with a link, so the reader knows whom to ask. The seat is found by
 * its agent id, never its role name, which two seats may share; where the
 * chart names no lead for it, the general sentence stands.
 */
function WithheldNote({ agentId }: { agentId: string }) {
  const agents = useAgents();
  const org = useOrg();
  const lead = useMemo(() => {
    const handle = agentId ? agents.find((a) => a.agent_id === agentId)?.handle : undefined;
    return handle ? (indexOrg(org).byHandle.get(handle)?.manager ?? null) : null;
  }, [agentId, agents, org]);
  if (!lead) return <Callout variant="neutral">{`${WITHHELD_WORDS}. ${WITHHELD_WHY}`}</Callout>;
  return (
    <Callout variant="neutral">
      Only <Link href={href(seatPath(lead))}>{lead.name}</Link> (this seat’s lead) and admins can
      see what it says.
    </Callout>
  );
}

/**
 * What one launch of an executor left: the report it wrote back, what it
 * delivered, and its own account of what it did in its box.
 *
 * A LAUNCH IS NOT A MODEL CALL. Its record has no rounds because the engine
 * drove none — the executor's own loop happened in its box — so its
 * `response` is the report the executor wrote back, and it carries an activity
 * log instead. ONE BODY
 * for a launch drawn inside its round's card and one drawn on its own (its
 * event's page, or a round whose own record is not in hand), so the two never
 * say different things about one launch.
 */
function LaunchBody({ record }: { record: PhaseRecord }) {
  const delivered = deliveredLine(record);
  return (
    <>
      {record.response.trim() && (
        <section className="col gap-1">
          <div className="t-label">
            Report
            <span className="muted"> · what the executor wrote back</span>
          </div>
          <Words text={record.response.trim()} />
        </section>
      )}
      {delivered && (
        <div className="t-caption">
          <span className="t-label">Delivered</span> {delivered}
        </div>
      )}
      {record.transcript && (
        // LAZY and closed: the engine bounds it at 256 KiB — its start and
        // its end — which is a long log to mount for a reader who came for
        // the report. The count says when it is not the whole.
        <Disclosure title="Activity" count={transcriptLength(record)} lazy>
          <p className="prose mono">{record.transcript}</p>
        </Disclosure>
      )}
    </>
  );
}

/**
 * A launch's CLI turns, as a header states a phase's rounds: the turns its
 * CLI said its own loop took, or the mark that it said none.
 */
function LaunchTurns({ record }: { record: PhaseRecord }) {
  return record.harnessTurns > 0 ? (
    <span className="phase-meta t-num" title="the model turns its CLI's own loop took">
      {plural(record.harnessTurns, "turn")}
    </span>
  ) : (
    <span className="phase-meta" title="its CLI reported no turns">
      …
    </span>
  );
}

/**
 * The launches of an executor's round, inside the round's card: one entry
 * per launch, oldest first — the CLI it ran as, how it ended, what it spent —
 * over what it left ([LaunchBody]).
 *
 * WHY INSIDE. The round is one card because it is one round: a launch is the
 * work the round's executor did, and a round can take several — an answer
 * carries an executor that parked on a question on in a second launch, and a
 * launch lost with its host is relaunched. Drawn as siblings of the round, a turn read
 * as phases nobody ran, and the round's own card as an executor that spent
 * nothing.
 */
function Launches({ launches, now }: { launches: readonly PhaseRecord[]; now: number }) {
  return (
    <section className="col gap-1">
      <div className="t-label">
        {launches.length === 1 ? "Its launch" : `Its ${launches.length} launches`}
        <span className="muted"> · what its CLI did in its box</span>
      </div>
      <ol className="run-ledger">
        {launches.map((l, i) => {
          const took = phaseDuration(l);
          return (
            <li key={l.key} className="run">
              <div className="run-head">
                <span className="run-name">
                  {launches.length > 1 ? `Launch ${i + 1}` : "Launch"}
                </span>
                {l.harness && (
                  <Tag variant="info" leadingIcon={<SquareTerminalGlyph />}>
                    {l.harness}
                  </Tag>
                )}
                {l.model && (
                  <span className="t-caption mono" title="the model it ran under">
                    {l.model}
                  </span>
                )}
                <LaunchTurns record={l} />
                <span className="phase-meta t-num" title="what the launch spent">
                  {`${fmtCount(l.totalTokens)} tokens`}
                </span>
                {took != null && (
                  <span className="phase-meta t-num" title="from its launch to its collection">
                    {fmtDuration(took)}
                  </span>
                )}
                <time className="phase-meta" dateTime={l.at} title={fmtDateTime(l.at)}>
                  {relTime(l.at, now)}
                </time>
                {l.failed ? (
                  <Tag variant="danger">{failureWords(l.errorKind).tag}</Tag>
                ) : (
                  <Tag appearance="outline" title="read off its box when it ended">
                    collected
                  </Tag>
                )}
              </div>
              {l.failed && l.error && <Callout variant="danger">{l.error}</Callout>}
              {l.notes && <Callout variant="neutral">{l.notes}</Callout>}
              <LaunchBody record={l} />
            </li>
          );
        })}
      </ol>
    </section>
  );
}

export function PhaseCard({
  record,
  defaultOpen,
  showRole,
  nested,
  launches,
}: {
  record: PhaseRecord;
  defaultOpen?: boolean;
  showRole?: boolean;
  /** The calls this phase made — the workers a `delegate` call ran, the
      round-cap judge. Rendered INSIDE this card, because that is what
      `host_phase` has always meant and rendering them as siblings left
      the reader working out which round each belonged to. */
  nested?: PhaseRecord[];
  /** The launches of this executor's round, oldest first — what each
      spent, reported and did in its box. Rendered INSIDE this card, for
      the reason `nested` is: they are this round's work. */
  launches?: PhaseRecord[];
}) {
  // Latched: seeded from `defaultOpen` and then owned by the reader. A phase
  // completing is not a reason to hide it.
  const [open, setOpen] = useState(!!defaultOpen);
  const now = useNow();
  const ledger = rounds(record.tools, record.narration, record.partial, record.runningCalls);
  // THE ROUNDS UNDER THE CLI RUN THAT MADE THEM, by the round span each run
  // names. Headed wherever the phase's runs are known: a phase that recorded
  // its runs, and a live one, whose run in flight heads the rounds past every
  // run that has ended — so a running phase and the record it becomes are one
  // shape, and nothing moves when a run ends but its head's words.
  const groups = runGroups(ledger, record.invocations);
  const headed = record.invocations.length > 0 || record.live;
  const capped = new Set(
    record.timedRounds.filter((r) => r.stopReason === "cap").map((r) => r.round),
  );
  // WHAT THE ROUND SPENT: the record's own tokens and its run's launches' —
  // the executor's record carries none of its own ([roundTokens]).
  const spent = roundTokens(record, launches);
  const streaming = ledger.some((r) => r.streaming);
  // A PARKED TURN'S CALL IS SILENT ON PURPOSE: the executor launched in its
  // box and its round will not move until that launch is collected, so the
  // stage is passed and the alarm stays down for as long as the executor
  // takes.
  const parked = record.live && record.stage === "parked";
  const stale = record.live ? staleness(record.at, now, record.stage) : "";
  const took = phaseDuration(record);
  // THE EVENT'S ADDRESS, FROM THE ONE MAP, for the link and for the guard
  // alike. The guard spelled the address itself and kept the old one when the
  // event log moved under Live — so it never matched, and the card drew
  // "event →" to the page it was already on.
  const eventPath = pathOf({ kind: "event", id: record.eventId });
  const onOwnEventPage = useIsCurrent(eventPath);
  // The last round is the live one while the phase runs: rounds only append,
  // so "newest" and "last" are the same row and stay the same row.
  const tailRef = useTail(open && record.live);
  const promptSize = useMemo(
    () =>
      [
        record.systemPrompt && `${fmtBytes(utf8Bytes(record.systemPrompt))} system`,
        record.userPrompt && `${fmtBytes(utf8Bytes(record.userPrompt))} user`,
      ]
        .filter(Boolean)
        .join(" · "),
    [record.systemPrompt, record.userPrompt],
  );
  // Rounds that answered in prose where a call was owed. Counted on the
  // ledger, live and settled alike, so the chip and the per-round notes are
  // one reading of one list.
  const declined = ledger.filter((r) => r.declined).length;
  const last = ledger.at(-1)?.round;
  const fateOf = (r: Round): Declined | undefined => {
    if (!r.declined) return undefined;
    if (r.round !== last) return "asked";
    return record.live ? "open" : "spent";
  };
  // One run's rounds — or, on a phase whose runs are not known, all of them.
  const roundList = (list: readonly Round[]) => (
    <ol className="round-ledger">
      {list.map((r) => (
        <RoundBlock
          key={r.round}
          round={r}
          live={record.live && r.round === last}
          declined={fateOf(r)}
          rescued={record.rescueFired}
          capped={capped.has(r.round)}
          now={now}
        />
      ))}
    </ol>
  );

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
        {open ? <ChevronDownGlyph size="xs" /> : <ChevronRightGlyph size="xs" />}
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
        {/* WHICH LAUNCH, on a launch drawn on its own: its phase word is its
            round's, and the launch id is what tells it from the round's record. */}
        {record.launch && (
          <span className="t-cell mono truncate" title="which launch this was">
            {record.launchId ? `launch ${record.launchId.slice(0, 8)}` : "launch"}
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
          <Tag variant={uiletTone(decisionTone(record.phase, record.decision, record.rescueFired))}>
            {decisionLabel(record.phase, record.decision, record.rescueFired)}
          </Tag>
        )}
        {record.exhaustedRounds && (
          <Tag variant="warning" title="the phase ran out of tool rounds">
            round cap
          </Tag>
        )}
        {record.emptyAnswerRounds > 0 && (
          <Tag variant="warning" title="Empty answers, asked again">
            {record.emptyAnswerRounds} empty
          </Tag>
        )}
        {/* "ANSWERED IN PROSE", not the wire's `declined`: the word a reader
            needs is what the model DID — wrote its answer as text, often the
            submission itself as a JSON block — rather than a verb that reads
            as the model refusing the work. Beside `empty`, its sibling: both
            are rounds that ended without the call the phase needed, one with
            words and one without. */}
        {declined > 0 && (
          <Tag variant="warning" title="Answered in text instead of submitting">
            {declined} answered in prose
          </Tag>
        )}
        {/* THE TITLE SAYS WHAT RESCUED MEANS. It said "did not submit on its
            first run and was re-asked", and both halves were wrong: nothing
            re-asks a phase as a whole, and the flag is set when the phase has
            ENDED without its submission succeeding — the engine then writes
            the decision itself, which is what the decision chip beside this
            one is reporting. */}
        {record.rescueFired && (
          <Tag variant="warning" title="No submission, so the engine filled in the outcome">
            rescued
          </Tag>
        )}
        {/* THE BOX A RUN RAN IN, named by the CLI that ran there — on the
            executor that collected a run and on each launch's own record. A
            phase that ran on the engine host names neither, so it has no
            badge. */}
        {(record.harness !== "" || record.sandboxId !== "") && (
          <Tag variant="info" leadingIcon={<SquareTerminalGlyph />}>
            {record.harness || "sandbox"}
          </Tag>
        )}
        {record.failed && <Tag variant="danger">{failureWords(record.errorKind).tag}</Tag>}
        {record.live && (
          <Tag variant={stale === "stalled" ? "danger" : stale ? "warning" : "info"} dot>
            {parked
              ? "running in its box"
              : stale === "stalled"
                ? "no update in 10m"
                : stale
                  ? "no update in 2m"
                  : "running"}
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
            phase whose first round had not come back, precisely the case this
            mark is for, fell through and rendered "0r". And the count below it
            was one short on a live phase whose rounds narrated nothing. */}
        {/* A LAUNCH'S TURNS WHERE A PHASE'S ROUNDS ARE: its rounds were its
            CLI's own, in its box, and the engine drove none of them. */}
        {record.launch ? (
          <LaunchTurns record={record} />
        ) : record.live && !ledger.length && record.roundsUsed === 0 ? (
          <span className="phase-meta" title="this phase has not finished a round yet">
            …
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
            dash claims the first about the second. An ellipsis marks the one
            case where the zero really is an absence. */}
        {record.live && spent === 0 ? (
          <span className="phase-meta" title="this phase has not reported its usage yet">
            …
          </span>
        ) : (
          <span
            className="phase-meta t-num"
            title={launches?.length ? "total tokens, its launches included" : "total tokens"}
          >
            {fmtCount(spent)}
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

          {/* A WITHHELD BODY IS SAID, never drawn as a phase that was told
              nothing and answered nothing: the engine sent this reader the
              phase's status alone (`bodyWithheld`), and every section below
              that is body is absent for that reason, not empty. */}
          {record.bodyWithheld && <WithheldNote agentId={record.agentId} />}

          {(record.systemPrompt || record.userPrompt) && (
            // A REAL SECTION OF THIS CARD, so it keeps uilet's default heading
            // — unlike a tool row, which is a transcript item. `lazy` matches
            // what ours did: a closed fold mounted nothing, and a seat's system
            // prompt is tens of kilobytes nobody asked for.
            //
            // What is INSIDE it is a document rather than a wall of text:
            // `PromptRecord` draws the request as a map and an outline and
            // shows one section, so "what was this phase told about X" is one
            // click rather than a scroll through 30 kB. It keeps the verbatim
            // record as its other view — the tallest block on the page by a
            // wide margin, and the one that most needed to stay one selection.
            //
            // THE COUNT IS ITS SIZE, in the bytes the Turn screen's Context tab
            // counts. It was the phase's name, which the tag at the head of
            // this same card already says.
            <Disclosure title="Prompt" count={promptSize} lazy>
              <PromptRecord
                phase={record.phase}
                system={record.systemPrompt}
                user={record.userPrompt}
                systemSections={record.systemSections}
                userSections={record.userSections}
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
                {/* WHAT THE PHASE NEVER HELD, not what its prompt said: the
                    record's catalogue is its universe less the tools above,
                    and its prompt names an MCP server's tools only by the
                    server. "Offered as prose" was true while that prompt
                    listed every builtin it already held, and false for every
                    name here once it stopped. */}
                {record.toolCatalogue.length > 0 && (
                  <div className="col gap-1">
                    <div className="t-label">
                      Reachable by discovery
                      <span className="muted"> · not activated this phase</span>
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

          {/* The transcript. One block per round — speech, then calls —
              in the order they happened, under the CLI run that made them.
              Rounds append, so nothing above an insertion can move, which is
              the whole point. */}
          {ledger.length > 0 && (
            <section className="col gap-1">
              <div className="t-label">Rounds</div>
              <div ref={tailRef} className={cx("tail-scroll", record.live && "tailing")}>
                {headed ? (
                  <ol className="run-ledger">
                    {groups.map((g, i) => {
                      // THE RUN IN FLIGHT is the trailing group no ended run
                      // claims, on a live phase. A group like it anywhere
                      // else is rounds no run named, drawn without a head
                      // rather than credited to a run that did not make them.
                      const inFlight = !g.run && record.live && i === groups.length - 1;
                      // KEYED BY THE RUN'S ORDINAL, the in-flight run by the
                      // one it will have once it ends — so its rounds stay
                      // mounted, and open where the reader opened them.
                      const ordinal = g.run?.run ?? record.invocations.length + 1;
                      return (
                        <li
                          key={
                            g.run || inFlight
                              ? `run-${ordinal}`
                              : `rounds-${g.rounds[0]?.round ?? i}`
                          }
                          className="run"
                        >
                          {(g.run || inFlight) && (
                            <RunHead run={g.run} ordinal={ordinal} live={inFlight} />
                          )}
                          {g.rounds.length > 0 && roundList(g.rounds)}
                        </li>
                      );
                    })}
                  </ol>
                ) : (
                  roundList(ledger)
                )}
              </div>
            </section>
          )}

          {record.launch && <LaunchBody record={record} />}
          {!!launches?.length && <Launches launches={launches} now={now} />}

          {/* The only genuinely empty state. A ROUND is never empty —
              `narrations()` drops an entry blank in both fields and a round
              built from a tool call has tools — so a per-round placeholder
              was unsatisfiable. A PHASE with no rounds yet is real: the
              provider call has not returned, and until the engine streams
              tokens there is nothing else to show for it. */}
          {record.live && !ledger.length && (
            <div className="row gap-2">
              <span className="waiting-dot" aria-hidden="true" />
              <span className="t-caption">Waiting for the first round…</span>
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
                href={href(eventPath)}
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
