/**
 * One strip under the page bar carrying the answer's own honesty.
 *
 * Three facts that were scattered across five screens and thrown away by five
 * more, in one place, on every screen:
 *
 *  - **Degradation** — the socket is down, the token was refused, no company
 *    configuration is active. These are about the CONNECTION rather than about
 *    the answer, and they outrank it: a stale badge on a screen whose socket
 *    has been down for a minute is describing the wrong problem.
 *  - **Coverage** — `read_level` as served, and `complete: false` as a banner
 *    naming what could not be accounted for. The flag is the ENGINE's, so it
 *    is always a banner and never a threshold this client invented.
 *  - **Freshness** — how far this node has applied, stated as a number.
 *
 * LAG IS A NUMBER, NEVER AN ALARM. This dashboard has no threshold of its own
 * for how far behind is too far, and the engine's alarm table is about a
 * node's duty rather than about one read — so a lagging read is stated plainly
 * and only an alarm the engine actually raised is drawn red.
 */

import type { ReactNode } from "react";
import { Icon } from "~/ui/Icon.tsx";
import { Badge, Button, cx } from "~/ui/primitives.tsx";
import type { CoverageFacts } from "~/components/work.tsx";

export interface Degradation {
  tone: "caution" | "critical";
  icon: "key" | "refresh" | "sliders";
  message: string;
  action?: { label: string; onClick: () => void };
}

/**
 * The one degradation worth reporting, chosen in the order a reader can act
 * on them.
 *
 * A REFUSED TOKEN FIRST, because it is the only one that resolves for nobody:
 * every other degraded state repairs itself when the engine comes back, and
 * showing three banners at once buries the one with an affordance.
 */
export function degradationOf({
  authRejected,
  connected,
  configured,
  onSetToken,
  onConfig,
}: {
  authRejected: boolean;
  connected: boolean;
  configured: boolean | undefined;
  onSetToken: () => void;
  onConfig: () => void;
}): Degradation | null {
  if (authRejected) {
    return {
      tone: "critical",
      icon: "key",
      message: "The engine refused this browser's API token.",
      action: { label: "Set token", onClick: onSetToken },
    };
  }
  if (!connected) {
    return {
      tone: "caution",
      icon: "refresh",
      message: "Reconnecting to the engine — showing the last state received, polling meanwhile.",
    };
  }
  if (configured === false) {
    return {
      tone: "caution",
      icon: "sliders",
      message:
        "No company configuration is active: no seats are running and inbound webhooks are being dropped.",
      action: { label: "Configuration", onClick: onConfig },
    };
  }
  return null;
}

export function StateBar({
  degraded,
  coverage,
  extra,
}: {
  degraded: Degradation | null;
  /** The answer this screen is drawn from, where it has one. */
  coverage?: CoverageFacts | null;
  /** A screen's own freshness line — "as of", "aggregated through". */
  extra?: ReactNode;
}) {
  const behind =
    coverage?.applied_through !== undefined &&
    coverage.log_seq !== undefined &&
    coverage.applied_through < coverage.log_seq;
  const incomplete = coverage?.complete === false;
  const anything = degraded || incomplete || coverage?.read_level || behind || extra;
  if (!anything) return null;

  return (
    <div className="state-bar">
      {degraded && (
        <div className={cx("degraded", degraded.tone)}>
          <Icon name={degraded.icon} size="sm" />
          <span>{degraded.message}</span>
          {degraded.action && (
            <>
              <span className="spacer" />
              <Button size="sm" onClick={degraded.action.onClick}>
                {degraded.action.label}
              </Button>
            </>
          )}
        </div>
      )}

      {/* THE ENGINE'S OWN FLAG, so it is a banner rather than a badge: rows
          may be missing, rows that should have gone may still be here, and
          every total on this screen was computed over the incomplete set. */}
      {incomplete && (
        <div className="degraded caution">
          <Icon name="alert" size="sm" />
          <span>
            <strong>This answer is incomplete</strong>
            {coverage?.incomplete
              ? ` — ${coverage.incomplete.records} record(s) this build cannot read.`
              : "."}{" "}
            Rows may be missing, rows that should have gone may still be here, and the counts were
            computed over what is shown.
            {coverage?.incomplete?.scope?.length
              ? ` Affected: ${coverage.incomplete.scope.join(", ")}.`
              : ""}
            {coverage?.incomplete
              ? ` Record version ${coverage.incomplete.version}, from sequence ${coverage.incomplete.from.seq} — a build that can read it is what resolves this, not a refresh.`
              : ""}
          </span>
        </div>
      )}

      {(coverage?.read_level || behind || extra) && (
        <div className="state-facts">
          {coverage?.read_level && (
            <Badge outline title="How fresh this answer is, as the engine actually served it">
              {coverage.read_level}
            </Badge>
          )}
          {behind && (
            <Badge outline title="This node holds records it has not applied yet">
              applied through {coverage?.applied_through} of {coverage?.log_seq}
            </Badge>
          )}
          {extra}
        </div>
      )}
    </div>
  );
}
