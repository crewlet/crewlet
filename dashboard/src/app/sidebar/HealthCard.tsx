/**
 * The engine's own state, at the foot of the sidebar: one line, one detail,
 * and the way to the nodes.
 *
 * # One state, chosen by precedence
 *
 * A node can be several things at once — reconnecting AND draining, configured
 * AND diverged — and a card that listed them all is a card nobody reads. So
 * the card says the ONE that decides what a reader should do, in this order:
 *
 *   1. nobody is signed in — the engine accepted no session from this
 *      browser, nothing else on screen can be trusted, and the card is the
 *      button that goes to the sign-in;
 *   2. the engine knows who this is and will not serve them — a seat taken
 *      out of the chart, a session without `state:read` — which no press
 *      here repairs, so the card is a statement and the state bar beside it
 *      carries the retry and the sign-out;
 *   3. the socket is reconnecting — everything shown is the last push;
 *   4. this node is draining — it is leaving and hands its seats on;
 *   5. no company is configured — nothing runs, whatever else is true;
 *   6. the node's posture diverged from the fleet (`shed`, `stuck`,
 *      `isolated`) — it is running a configuration nobody else is;
 *   7. no health push yet — connected, and nothing to report on, which is
 *      not "healthy";
 *   8. serving — a success dot, the detail "3 nodes · config epoch 42" (or
 *      "node count unavailable" where the presence read failed, never a
 *      zero), and the title "Engine healthy" ONLY when the alarm table was
 *      evaluated and nothing in it fires; otherwise "Engine serving".
 *
 * THE DOT IS THE POSTURE, AND ONLY THE POSTURE. The alarm line is a second
 * fact with its OWN tone and glyph, drawn beneath, rather than a recoloured
 * dot: borrowing the dot drew a green title beside an amber dot, a title and
 * a colour contradicting each other. But "HEALTHY" IS A CLAIM THE ALARMS CAN
 * REFUTE, and "Engine healthy" directly above "5 alarms · oldest: backup age"
 * was the same contradiction in words. So the title says what the posture
 * says — the node is serving — and keeps "healthy" for the one state that
 * earns it. It never repeats the alarm line ("serving, with alarms"), which
 * would say nothing the line under it does not.
 *
 * Every word comes off the ONE health push (`useEngineHealth`) and the
 * connection; nothing here polls. The ALARM LINE is `health.alarms`, the same
 * evaluation the engine's gauge and log lines come from: a count and the one
 * that has stood longest — with the log it stands on, where the table keeps
 * that alarm per log, because two logs' `trim_blocked` are two conditions
 * with two remedies — never the rows, because the push is public. An
 * ABSENT table is its own line: the engine has not evaluated it yet, which
 * the contract (`contract/health.ts`) says is not the same as nothing firing.
 */

import type { ReactNode } from "react";
import { StatusDot } from "@crewlethq/ui";
import { ChevronRightGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { nodeCountLabel, plural } from "~/lib/format.ts";
import type { EngineHealth } from "~/contract/health.ts";

/** The postures a node holds while serving the configuration the fleet agreed on. */
const AGREED = new Set(["", "serve", "wait"]);

export interface HealthReading {
  tone: "success" | "warning" | "danger";
  title: string;
  detail: string;
  /**
   * The alarm line, where there is something to say about the alarm table:
   * `warning` while any alarm stands, `neutral` while the engine has not
   * evaluated the table yet (which is not the same as nothing firing, and not
   * a fault either).
   */
  alarms?: AlarmLine;
  /**
   * What the card does when pressed: `sign-in` goes to the sign-in screen,
   * `none` is a statement with nothing a press can fix, and absent is the
   * ordinary link to the nodes.
   */
  press?: "sign-in" | "none";
}

export interface AlarmLine {
  tone: "warning" | "neutral";
  text: string;
}

/** What the card says, decided once and testable without a render. */
export function healthReading(input: {
  connected: boolean;
  authRejected: boolean;
  /** Why the engine will not serve a browser it knows, or null. */
  accessRefused?: string | null;
  health: EngineHealth | null;
}): HealthReading {
  const { connected, authRejected, accessRefused = null, health } = input;
  const alarms = alarmLine(health);
  if (authRejected) {
    return {
      tone: "danger",
      title: "Not signed in",
      detail: "The engine accepted no session from this browser — sign in",
      press: "sign-in",
    };
  }
  if (accessRefused !== null) {
    return {
      tone: "danger",
      title: "Access refused",
      detail: `The engine knows who you are and will not serve you (${accessRefused})`,
      press: "none",
    };
  }
  if (!connected) {
    return { tone: "warning", title: "Reconnecting", detail: "Showing the last state received" };
  }
  if (health?.shutting_down) {
    return {
      tone: "warning",
      title: "Draining",
      detail: "This node is shutting down and handing its seats on",
      ...(alarms ? { alarms } : {}),
    };
  }
  if (health?.configured === false) {
    return {
      tone: "warning",
      title: "No configuration",
      detail: "No company is active, so no seat runs",
      ...(alarms ? { alarms } : {}),
    };
  }
  const posture = health?.posture ?? "";
  if (!AGREED.has(posture)) {
    return {
      tone: "danger",
      title: `Engine ${posture}`,
      detail: "This node is not serving the configuration the fleet agreed on",
      ...(alarms ? { alarms } : {}),
    };
  }
  if (!health) {
    return { tone: "warning", title: "Waiting for the engine", detail: "No health report yet" };
  }
  const epoch = health.applied_epoch;
  return {
    tone: "success",
    // HEALTHY ONLY WITH NOTHING TO REFUTE IT: an alarm line — standing, or a
    // table not evaluated yet — is exactly what the word would contradict.
    title: alarms ? "Engine serving" : "Engine healthy",
    detail:
      epoch !== undefined
        ? `${nodeCountLabel(health.nodes)} · config epoch ${epoch}`
        : nodeCountLabel(health.nodes),
    ...(alarms ? { alarms } : {}),
  };
}

function alarmLine(health: EngineHealth | null): AlarmLine | undefined {
  if (!health) return undefined;
  const alarms = health.alarms;
  if (!alarms) return { tone: "neutral", text: "Alarms not evaluated yet" };
  if (alarms.count === 0) return undefined;
  // "5 alarms · oldest: backup age" — the one that has stood longest, named
  // as that; "backup age longest" read as a broken sentence.
  const domain = alarms.worst_domain ? ` (${alarms.worst_domain})` : "";
  const worst = alarms.worst ? ` · oldest: ${alarms.worst.replace(/_/g, " ")}${domain}` : "";
  return { tone: "warning", text: `${plural(alarms.count, "alarm")}${worst}` };
}

export function HealthCard({
  reading,
  onSignIn,
}: {
  reading: HealthReading;
  onSignIn: () => void;
}) {
  const body: ReactNode = (
    <>
      {/* ON THE TITLE'S LINE, because it is the title's tone: centred on
          the card it drifted down beside the alarm line once there was one,
          and read as that line's colour. The card is a grid whose first row
          is the title's, so the dot is centred on that line's own box — see
          `.health-card` in frame.css. */}
      <span className="health-card-dot">
        <StatusDot tone={reading.tone} />
      </span>
      <span className="health-card-text">
        <span className="health-card-title">{reading.title}</span>
        <span className="health-card-detail">{reading.detail}</span>
        {reading.alarms && (
          // ITS OWN TONE AND ITS OWN GLYPH, never colour alone: the glyph is
          // what says "alarm" to a reader who cannot tell amber from grey.
          <span className="health-card-alarm" data-tone={reading.alarms.tone}>
            {reading.alarms.tone === "warning" && (
              <TriangleAlertGlyph size="xs" aria-hidden="true" />
            )}
            {reading.alarms.text}
          </span>
        )}
      </span>
      <ChevronRightGlyph size="sm" className="health-card-go" />
    </>
  );
  // NOBODY SIGNED IN IS THE ONE STATE A PRESS CAN FIX, so the card is that
  // button. A refusal of somebody the engine knows is fixed by an
  // administrator, so the card says it and leads nowhere — the nodes' page
  // would be one more refusal. Every other state is a fact about the fleet,
  // and its page is the nodes.
  if (reading.press === "sign-in") {
    return (
      <button type="button" className="health-card" data-tone={reading.tone} onClick={onSignIn}>
        {body}
      </button>
    );
  }
  if (reading.press === "none") {
    return (
      <div className="health-card" data-tone={reading.tone}>
        {body}
      </div>
    );
  }
  return (
    <a className="health-card" data-tone={reading.tone} href={href(["settings", "nodes"])}>
      {body}
    </a>
  );
}
