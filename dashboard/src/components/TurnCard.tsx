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
import { Badge, PhaseTag, cx } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { PhaseCard } from "./PhaseCard.tsx";
import { fmtCount, fmtDateTime, fmtDuration, fmtElapsed, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { href } from "~/app/router.tsx";
import type { TurnGroup } from "~/lib/phases.ts";

export function TurnCard({
  group,
  defaultOpen,
  showRole,
}: {
  group: TurnGroup;
  defaultOpen?: boolean;
  showRole?: boolean;
}) {
  const [open, setOpen] = useState(!!defaultOpen);
  const now = useNow();

  // The turn's own wall time, from its first phase to its last. The engine's
  // `turn_completed` carries an exact `duration_ms`, but it is a separate
  // event; where the turn's phases are all we have, this is the honest
  // measure and it is labelled as spanning them.
  const first = group.phases[0];
  const last = group.phases[group.phases.length - 1];
  const span =
    first && last && tsKey(last.at) > tsKey(first.at) ? tsKey(last.at) - tsKey(first.at) : null;

  const trigger = group.trigger;

  return (
    <article className={cx("turn-card", group.live && "live", group.failed && "failed")}>
      <header className="turn-head" onClick={() => setOpen((v) => !v)}>
        <Icon name={open ? "chevronDown" : "chevronRight"} size="sm" />
        <div className="col" style={{ gap: 2, flex: 1, minWidth: 0 }}>
          <div className="row gap-1">
            {showRole && group.role && <strong className="t-cell">{group.role}</strong>}
            <span className="truncate t-cell secondary">
              {trigger?.summary || trigger?.type || "turn"}
            </span>
          </div>
          <div className="row gap-1">
            {group.phases.map((p) => (
              <PhaseTag key={p.key} phase={p.phase} />
            ))}
            {trigger?.integration && (
              <Badge outline mono>
                {trigger.integration}
              </Badge>
            )}
          </div>
        </div>
        <span className="spacer" />
        {group.live && (
          <Badge tone="info" dot>
            running
          </Badge>
        )}
        {group.failed && <Badge tone="critical">failed</Badge>}
        <span className="phase-meta t-num" title="tokens across every phase of this turn">
          {group.totalTokens ? fmtCount(group.totalTokens) : "—"}
        </span>
        {span != null && (
          <span className="phase-meta t-num" title="from the first phase to the last">
            {fmtDuration(span)}
          </span>
        )}
        {/* Running for HOW LONG, or landed WHEN — measured from a start
            that does not move. Against `at`, which advances on every
            streamed frame, a live turn read "just now" forever. */}
        <time
          className="phase-meta"
          dateTime={group.live ? group.startedAt : group.at}
          title={fmtDateTime(group.live ? group.startedAt : group.at)}
        >
          {group.live ? fmtElapsed(now - tsKey(group.startedAt)) : relTime(group.at, now)}
        </time>
      </header>

      {open && (
        <div className="turn-body">
          {group.phases.map((p, i) => (
            <PhaseCard key={p.key} record={p} defaultOpen={i === 0 && group.phases.length === 1} />
          ))}
          <footer className="phase-foot">
            <a className="t-caption mono" href={href(["turns", group.turnId])}>
              turn {group.turnId.slice(0, 8)} →
            </a>
            <span className="spacer" />
            <span className="t-caption">
              every event of this turn, including the ones no phase carries
            </span>
          </footer>
        </div>
      )}
    </article>
  );
}
