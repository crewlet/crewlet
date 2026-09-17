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
import { Button, Callout, Tag } from "@crewlethq/ui";
import { KeyGlyph, RefreshGlyph, TuneGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
import type { CoverageFacts } from "~/components/work.tsx";

export interface Degradation {
  /** The strip's own variant, in uilet's spelling: two states, both of them bad. */
  variant: "warning" | "danger";
  /**
   * The mark, as a GLYPH rather than a name.
   *
   * It was a name out of our own icon set, chosen from a three-value union, and
   * the union was the whole type system this had: a fourth degradation would
   * have added a name here and a path in `~/ui/Icon.tsx`. A component is the
   * value uilet's `Callout` takes, and `degradationOf` below is the only thing
   * that ever builds one.
   */
  icon: ReactNode;
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
      variant: "danger",
      icon: <KeyGlyph size="md" />,
      message: "The engine refused this browser's API token.",
      action: { label: "Set token", onClick: onSetToken },
    };
  }
  if (!connected) {
    return {
      variant: "warning",
      icon: <RefreshGlyph size="md" />,
      message: "Reconnecting to the engine — showing the last state received, polling meanwhile.",
    };
  }
  if (configured === false) {
    return {
      variant: "warning",
      icon: <TuneGlyph size="md" />,
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
      {/* `Callout layout="banner"` IS `.degraded`: the same soft fill under
          the same measured ink, the same 8/16 padding, the same bottom rule
          and no radius. What it adds is the spacer we were spelling by hand —
          `action` is its own slot at the trailing edge. */}
      {degraded && (
        <Callout
          variant={degraded.variant}
          icon={degraded.icon}
          layout="banner"
          action={
            degraded.action && (
              <Button size="small" variant="secondary" onClick={degraded.action.onClick}>
                {degraded.action.label}
              </Button>
            )
          }
        >
          {degraded.message}
        </Callout>
      )}

      {/* THE ENGINE'S OWN FLAG, so it is a banner rather than a badge: rows
          may be missing, rows that should have gone may still be here, and
          every total on this screen was computed over the incomplete set. */}
      {incomplete && (
        <Callout variant="warning" icon={<WarningGlyph size="md" />} layout="banner">
          <span>
            {/* THE BOLD STAYS IN THE SENTENCE rather than becoming Callout's
                `title`. Its title is a lead-in label with its own trailing
                space; this is one running sentence whose opening clause is
                emphasised and whose next word is an em dash or a full stop. */}
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
        </Callout>
      )}

      {(coverage?.read_level || behind || extra) && (
        <div className="state-facts">
          {/* NEUTRAL, both of them. A read level and an apply position are
              facts about the answer, not states of it — and lag alone is never
              an alarm here, which is what the module doc above says. */}
          {coverage?.read_level && (
            <Tag
              appearance="outline"
              size="xs"
              title="How fresh this answer is, as the engine actually served it"
            >
              {coverage.read_level}
            </Tag>
          )}
          {behind && (
            <Tag
              appearance="outline"
              size="xs"
              title="This node holds records it has not applied yet"
            >
              applied through {coverage?.applied_through} of {coverage?.log_seq}
            </Tag>
          )}
          {extra}
        </div>
      )}
    </div>
  );
}
