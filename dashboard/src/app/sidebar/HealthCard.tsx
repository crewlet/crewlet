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
 *   1. the token was refused — nothing else on screen can be trusted, and the
 *      card is the button that asks for another;
 *   2. the socket is reconnecting — everything shown is the last push;
 *   3. this node is draining — it is leaving and hands its seats on;
 *   4. no company is configured — nothing runs, whatever else is true;
 *   5. the node's posture diverged from the fleet (`shed`, `stuck`,
 *      `isolated`) — it is running a configuration nobody else is;
 *   6. no health push yet — connected, and nothing to report on, which is
 *      not "healthy";
 *   7. serving — "Engine healthy", a success dot, and the detail "3 nodes ·
 *      config epoch 42" (or "node count unavailable" where the presence read
 *      failed, never a zero).
 *
 * THE DOT AND THE TITLE ARE THE POSTURE, AND ONLY THE POSTURE. The alarm line
 * is a second fact with its OWN tone and glyph, drawn beneath: a serving node
 * with a standing alarm is healthy AND has an alarm, and the card says both
 * rather than recolouring the one to carry the other. Borrowing the dot for
 * the alarms is what drew "Engine healthy" beside an amber dot — a title and a
 * colour contradicting each other — and retitling it "Engine serving, with
 * alarms" to match only moved the contradiction: the posture line then said
 * something about the alarms and nothing a reader could act on differently
 * from the line under it.
 *
 * Every word comes off the ONE health push (`useEngineHealth`) and the
 * connection; nothing here polls. The ALARM LINE is `health.alarms`, the same
 * evaluation the engine's gauge and log lines come from: a count and the one
 * that has stood longest, never the rows, because the push is public. An
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
  /** The card is a button that asks for a token, rather than a link. */
  asksForToken?: boolean;
}

export interface AlarmLine {
  tone: "warning" | "neutral";
  text: string;
}

/** What the card says, decided once and testable without a render. */
export function healthReading(input: {
  connected: boolean;
  authRejected: boolean;
  health: EngineHealth | null;
}): HealthReading {
  const { connected, authRejected, health } = input;
  const alarms = alarmLine(health);
  if (authRejected) {
    return {
      tone: "danger",
      title: "Token refused",
      detail: "The engine refused this browser's token — set another",
      asksForToken: true,
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
    title: "Engine healthy",
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
  const worst = alarms.worst ? ` · oldest: ${alarms.worst.replace(/_/g, " ")}` : "";
  return { tone: "warning", text: `${plural(alarms.count, "alarm")}${worst}` };
}

export function HealthCard({
  reading,
  onSetToken,
}: {
  reading: HealthReading;
  onSetToken: () => void;
}) {
  const body: ReactNode = (
    <>
      {/* ON THE TITLE'S LINE, because it is the title's tone: centred on
          the card it drifted down beside the alarm line once there was one,
          and read as that line's colour. */}
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
  // A REFUSED TOKEN IS THE ONE STATE A PRESS CAN FIX, so the card is that
  // button. Every other state is a fact about the fleet, and its page is the
  // nodes.
  if (reading.asksForToken) {
    return (
      <button type="button" className="health-card" data-tone={reading.tone} onClick={onSetToken}>
        {body}
      </button>
    );
  }
  return (
    <a className="health-card" data-tone={reading.tone} href={href(["settings", "nodes"])}>
      {body}
    </a>
  );
}
