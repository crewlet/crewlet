/**
 * One turn: its phases, in the order they ran.
 *
 * A turn is read FORWARDS — execute, then review, with a first-turn
 * onboarding pass ahead of both — which is the opposite of the feed it sits
 * in. The turn list is newest first; inside a turn, oldest first.
 *
 * The card's open state is latched, like a phase's. The previous surface
 * derived it (`isLive || is the newest failed turn`), recomputed on every
 * render, so a turn's whole transcript vanished the moment its last phase
 * completed and a new failure elsewhere silently re-opened a different card
 * and shoved everything below it down the page.
 */

import { useState } from "react";
import { Button, cx, EmptyValue, Tag } from "@crewlethq/ui";
import { ChevronRightGlyph, KeyboardArrowDownGlyph, LayersGlyph } from "@crewlethq/icons/glyphs";
// STILL OURS, and for the reason PhaseCard gives at its own import: `PhaseTag`
// has a peer, but it is a `~/ui` primitive, so its port belongs to that file
// rather than to a third inlined copy of the phase variant table.
import { PhaseTag } from "~/ui/primitives.tsx";
import { PhaseCard } from "./PhaseCard.tsx";
import { fmtCount, fmtDateTime, fmtDuration, fmtElapsed, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { useNavigator } from "~/app/router.tsx";
import { triggerHeadline, type Attempt, type TurnGroup } from "~/lib/phases.ts";
import type { TurnRow } from "~/protocol/index.ts";

export function TurnCard({
  group,
  row,
  attempt,
  defaultOpen,
}: {
  group: TurnGroup;
  /** Which attempt at this trigger the turn was, when the screen holds more
   *  than one — see `attempts`. Absent for the ordinary turn that ran once. */
  attempt?: Attempt;
  /** The engine's own settled row for this turn, where the screen holds one.
   *  The card and the turns table above it are ONE turn on ONE screen and were
   *  reporting different numbers for it. */
  row?: TurnRow;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(!!defaultOpen);
  const now = useNow();
  const nav = useNavigator();

  // THE ENGINE'S OWN FIGURES WHERE A RECORD CARRIES THEM, the window across this
  // card's phases otherwise — the rule `turnFacts` states for the Turn screen,
  // applied here because the table above prints the same turn.
  //
  // `started_at` is the turn's FIRST RECORDED EVENT — its prefetch lands before
  // the first model call, so the table said "started 6m ago" beside a card
  // counting 3m 15s from the phase that is running. The phase window is the
  // fallback for the one turn the store cannot answer for: the one that began
  // after the table was answered, and every turn on a node keeping no event log.
  //
  // The length used to be `last.at - first.at`, two LANDING instants, which
  // drops the first phase's own length: a 60s review over a 3m execute rendered
  // as "1m 0s" above a phase card reading 3m.
  const measured = !!row?.complete && (row?.duration_ms ?? 0) > 0;
  const took = measured ? row!.duration_ms : group.span;
  const startedAt = row?.started_at || group.startedAt;
  const began = tsKey(startedAt);

  // THE ENGINE'S OWN MARK, OR THE PHASES', WHICHEVER SAYS SO — the same rule
  // the duration above follows, and for a sharper reason.
  //
  // `group.failed` is `phases.some(p => p.failed)`, which cannot see a turn the
  // engine killed BETWEEN phases: a panic recovered outside the loop, a
  // detached sandbox run that failed. The store's aggregate reads those (a
  // failure BY TYPE — see `events.Failed`), so the Turns grid one panel up
  // marked them failed and this card, drawn from the same screen's data, did
  // not. One turn, two answers, side by side.
  //
  // EITHER, never the row alone: the row is polled and the phases are pushed,
  // so a phase that failed a moment ago is red here before the next poll
  // carries it. The row is a superset once it arrives, so the union settles on
  // the row's answer rather than oscillating.
  const failed = !!row?.failed || group.failed;

  const trigger = group.trigger;
  const headline = triggerHeadline(trigger);

  return (
    <article className={cx("turn-card", group.live && "live", failed && "failed")}>
      <header className="turn-head" onClick={() => setOpen((v) => !v)}>
        {open ? <KeyboardArrowDownGlyph size="sm" /> : <ChevronRightGlyph size="sm" />}
        <div className="col" style={{ gap: 2, flex: 1, minWidth: 0 }}>
          {/* THE ONE SENTENCE A COLLAPSED CARD CARRIES, and it CLAMPS where it
              used to be cut at a line. `.truncate` is the cell rule: it is right
              in the Turns grid one panel up, where a row has a fixed height and
              a column can be widened. Here nothing constrains the height — the
              card grows to its content and the metadata beside it is centred
              against whatever this is — so "Message from founder: " plus a task
              title (up to tracker.MaxTitle, 256) lost its subject at about the
              fortieth character with empty card underneath, and the only way to
              learn what the turn was about was to open it.
              TWO LINES, not free-flowing: this is one card in a feed of forty
              and the head's rule is that the same facts sit in the same places,
              so one long title may not push every card below it down. What two
              lines still cannot hold is on `title`, and that is a POINTER
              affordance only — a clamp is visual, so the whole sentence stays in
              the DOM and a screen reader has it either way. */}
          <span className="clamp t-cell secondary" title={headline}>
            {headline}
          </span>
          <div className="row gap-1">
            {group.phases.map((p) => (
              <PhaseTag key={p.key} phase={p.phase} />
            ))}
          </div>
        </div>
        <span className="spacer" />
        {/* WHERE THIS TURN CAME FROM, in the metadata cluster with the turn's
            other attributes — how much, how long, when.
            It has been three other places and each was worse for the same
            reason: it moved. Beside the phase tags it read as a third phase;
            in front of the trigger text the eye hit a label before the
            sentence it labels; after that text it sat wherever the sentence
            happened to end, which is a different spot on every card. The rule
            this header already keeps is that the same facts are in the same
            places always, so a reader scanning a list can compare a column
            rather than hunt a row — and a source is exactly the kind of thing
            somebody scans down. */}
        {/* NEUTRAL. A vendor is an identity, which uilet's tone doc names
            among the four things a hue must never carry. */}
        {trigger?.integration && (
          <Tag appearance="outline" monospace title="where this turn's trigger came from">
            {trigger.integration}
          </Tag>
        )}
        {/* A RE-RUN SAYS SO. A trigger whose turn fails without reaching
            outside the engine is redelivered, so it runs again under a new id
            — and two rows for one message, each with its own outcome, is
            exactly what an operator reads as the engine having done the work
            twice. Neutral, because being a second attempt is a fact about the
            trigger rather than a fault. */}
        {attempt && (
          <Tag
            appearance="outline"
            title={
              `attempt ${attempt.index} of ${attempt.total} at this trigger, among the turns ` +
              `on this screen — a turn that failed without acting is redelivered and runs again`
            }
          >
            attempt {attempt.index}/{attempt.total}
          </Tag>
        )}
        {group.live && (
          <Tag variant="info" dot>
            running
          </Tag>
        )}
        {failed && <Tag variant="danger">failed</Tag>}
        {/* ZERO IS A NUMBER, AND ONLY A LIVE TURN'S ZERO IS AN ABSENCE. This
            read `totalTokens ? … : "—"`, so a turn that genuinely spent
            nothing — every phase on a subscription CLI, which reports no
            usage at all, or a turn the engine stopped before its first call
            came back — rendered as "not recorded". That is the confusion
            `app/frame/cells.tsx` exists to end: absent and zero are different
            facts and a dash claims the first about the second. The dash stays
            for the one case where the zero really is an absence — a turn
            still running, whose phases have not reported their usage yet. */}
        {group.live && group.totalTokens === 0 ? (
          <span className="phase-meta" title="no phase has reported its usage yet">
            —
          </span>
        ) : (
          <span className="phase-meta t-num" title="tokens across every phase of this turn">
            {fmtCount(group.totalTokens)}
          </span>
        )}
        {took != null && (
          <span
            className="phase-meta t-num"
            title={
              measured
                ? "what the engine measured"
                : "across this turn's phases — its own record is not in hand"
            }
          >
            {fmtDuration(took)}
          </span>
        )}
        {/* Running for HOW LONG, or landed WHEN — from a start that does not
            move, and the SAME start the turns table prints. Against `at`, which
            advances on every streamed frame, a live turn read "just now"
            forever; and against the phase's own start rather than the turn's,
            it disagreed with the table beside it by the length of the prefetch.
            An unreadable instant draws the absent mark rather than counting
            from 1970. */}
        <time
          className="phase-meta"
          dateTime={group.live ? startedAt : group.at}
          title={fmtDateTime(group.live ? startedAt : group.at)}
        >
          {group.live ? (
            began > 0 ? (
              fmtElapsed(now - began)
            ) : (
              <EmptyValue label="Started at an instant this build could not read" />
            )
          ) : (
            relTime(group.at, now)
          )}
        </time>
      </header>

      {open && (
        <div className="turn-body">
          {group.phases.map((p, i) => (
            <PhaseCard
              key={p.key}
              record={p}
              nested={group.nested.get(p.key)}
              defaultOpen={i === 0 && group.phases.length === 1}
            />
          ))}
          {/* THE WAY OUT OF THIS CARD, drawn as a control rather than as a
              mono caption in a footer corner. It was a link styled like debug
              output, with the sentence explaining it pushed to the OPPOSITE
              end of the row — so the most useful action on the card looked
              like a row of hex, and the promise it makes was too far away to
              read as its label. */}
          <footer className="phase-foot">
            {/* `secondary`, NOT the uilet default. Their Button defaults to
                `primary` — the one action on a screen — and this card can be
                one of forty in a feed. Ours defaulted to the quiet recipe, and
                `secondary` is that recipe's name over here. */}
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<LayersGlyph />}
              onClick={() => nav.to(["activity", "turns", group.turnId])}
              title={`turn ${group.turnId}`}
            >
              Open the whole turn
            </Button>
            <span className="t-caption">
              what woke it, what it was given, and what it learned — the events no phase carries
            </span>
            <span className="spacer" />
          </footer>
        </div>
      )}
    </article>
  );
}
