/**
 * One strip under the page bar carrying the answer's own honesty.
 *
 * Three facts that were scattered across five screens and thrown away by five
 * more, in one place, on every screen:
 *
 *  - **Degradation** — the socket is down, nobody is signed in, no company
 *    configuration is active. These are about the CONNECTION rather than about
 *    the answer, and they outrank it: a stale badge on a screen whose socket
 *    has been down for a minute is describing the wrong problem.
 *  - **Coverage**: `read_level` as served, and `complete: false` as a banner
 *    saying the answer may be incomplete. The flag is the ENGINE's, so it is
 *    always a banner and never a threshold this client invented.
 *  - **Freshness** — how far this node has applied, stated as a number.
 *
 * LAG IS A NUMBER, NEVER AN ALARM. This dashboard has no threshold of its own
 * for how far behind is too far, and the engine's alarm table is about a
 * node's duty rather than about one read — so a lagging read is stated plainly
 * and only an alarm the engine actually raised is drawn red.
 */

import type { ReactNode } from "react";
import { Button, Callout, Spinner } from "@crewlethq/ui";
import { CoverageTags, INCOMPLETE_NOTE, oddLevel, type CoverageFacts } from "~/components/work.tsx";

export interface Degradation {
  /**
   * The strip's own variant, in uilet's spelling: the two bad states, and a
   * first dial still out, which is no fault yet.
   */
  variant: "neutral" | "warning" | "danger";
  message: string;
  action?: { label: string; onClick: () => void };
  /**
   * The first dial, which nothing is behind yet and which ends by itself
   * within a moment: said in the page bar ([FirstDialChip]) wherever the
   * frame draws one, never as a strip over the page or a line that pushes
   * it down.
   */
  firstDial?: boolean;
}

/**
 * The one degradation worth reporting, chosen in the order a reader can act
 * on them.
 *
 * NOBODY SIGNED IN FIRST, because it is the only one that resolves for
 * nobody: every other degraded state repairs itself when the engine comes
 * back, and showing three banners at once buries the one with an affordance.
 * The app sends a reader it resolved nobody for to the sign-in screen as soon
 * as it learns so, so this is the frame's word for the moment in between.
 */
export function degradationOf({
  authRejected,
  accessRefused = null,
  connected,
  everConnected = true,
  configured,
  onSignIn,
  onConfig,
}: {
  authRejected: boolean;
  /**
   * Why the engine will not serve this surface to a browser it knows, or
   * null. Said in place of the screen (`AccessRefused`), never here: it is
   * not a degradation the strip can describe beside a screen that will
   * never be answered.
   */
  accessRefused?: string | null;
  connected: boolean;
  /**
   * Whether this tab's socket has ever been answered. Before it has, nothing
   * was received to show, so the first dial is said as connecting.
   */
  everConnected?: boolean;
  configured: boolean | undefined;
  onSignIn: () => void;
  onConfig: () => void;
}): Degradation | null {
  if (authRejected) {
    return {
      variant: "danger",
      message: "You’re signed out.",
      action: { label: "Sign in", onClick: onSignIn },
    };
  }
  // A REFUSED BROWSER'S SOCKET STOPS, so "reconnecting" would be false: the
  // screen says what is true instead — see `AccessRefused`.
  if (accessRefused !== null) return null;
  if (!connected) {
    // THE FIRST DIAL IS NO RECONNECT, and there is no last state to show yet.
    if (!everConnected) {
      return {
        variant: "neutral",
        message: "Connecting to the engine.",
        firstDial: true,
      };
    }
    return {
      variant: "warning",
      message: "Reconnecting. Showing the last state received.",
    };
  }
  if (configured === false) {
    return {
      variant: "warning",
      message: "No company is active, so no seats are running.",
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
      {degraded && <DegradedNotice degraded={degraded} />}

      {/* THE ENGINE'S OWN FLAG, so it is a banner rather than a badge: rows
          may be missing, rows that should have gone may still be here, and
          every total on this screen was computed over the incomplete set. */}
      {incomplete && (
        <Callout variant="warning" layout="banner">
          {INCOMPLETE_NOTE}
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

/**
 * The degradation's banner.
 *
 * The first dial is the page bar's ([FirstDialChip]) wherever the frame draws
 * one, and the frame hands this bar no first dial there. Where a screen draws
 * its own head (an open artifact) there is no bar yet while the first dial is
 * out, since that head is drawn from what the engine answers, so it stands
 * here in the flow under the empty head, over a screen that is still its
 * skeleton.
 */
function DegradedNotice({ degraded }: { degraded: Degradation }) {
  return (
    <Callout
      variant={degraded.variant}

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
  );
}

/**
 * The first dial, said in the page bar beside the frame's controls.
 *
 * IT USED TO FLOAT over the page's top edge (in the flow it pushed every page
 * down on a cold load and pulled it back up when the engine answered), and a
 * strip floated under the page bar lies over the first lines of the screen:
 * it struck through Home's greeting. The bar's row is room the frame already
 * holds, so a chip there covers nothing and moves nothing. A status region,
 * so a screen reader hears it once; its words fold away on a phone's bar,
 * where the ring and the name say it.
 */
export function FirstDialChip({ degraded }: { degraded: Degradation }) {
  return (
    <span className="first-dial" role="status" aria-label={degraded.message}>
      <Spinner size="xs" aria-hidden="true" />
      <span className="page-wide-words" aria-hidden="true">
        {degraded.message}
      </span>
    </span>
  );
}
