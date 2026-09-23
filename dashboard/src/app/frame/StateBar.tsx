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
import { Button, Callout } from "@crewlethq/ui";
import { KeyGlyph, RefreshGlyph, TuneGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
import { CoverageTags, oddLevel, type CoverageFacts } from "~/components/work.tsx";

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
  identityUnverifiable = false,
  configured,
  onSetToken,
  onConfig,
}: {
  authRejected: boolean;
  connected: boolean;
  /** The engine could not verify this socket's credential at its last check. */
  identityUnverifiable?: boolean;
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
  if (identityUnverifiable) {
    return {
      variant: "warning",
      icon: <RefreshGlyph size="md" />,
      message:
        "The engine cannot verify your session right now, so live updates are paused and questions wait. It checks again every minute — nothing to do.",
    };
  }
  if (configured === false) {
    return {
      variant: "warning",
      icon: <TuneGlyph size="md" />,
      message:
        "No company configuration is active: no seats are running, and inbound webhooks are refused with a 503 their sender will retry.",
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
  // A LEVEL THIS SURFACE EXPECTS IS NOT NEWS, and the bar must not open a band
  // for one: `stale` is the dashboard's own default, so a `read_level` test
  // here drew an empty facts row — a rule, a background and a padding band —
  // under the page bar of every screen in the product. See [CoverageTags].
  const odd = oddLevel(coverage?.read_level);
  const anything = degraded || incomplete || odd || behind || extra;
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

      {(odd || behind || extra) && (
        <div className="state-facts">
          {/* THE FACTS ARE `CoverageTags`', not a second copy. This bar drew
              them as `xs` outline chips and `components/work.tsx` drew the
              same fact at the default size inside a screen's own toolbar —
              one answer, two renderings, and on Inbox and My work two
              different corners of the chrome one click apart. */}
          <CoverageTags answer={coverage} />
          {extra}
        </div>
      )}
    </div>
  );
}
