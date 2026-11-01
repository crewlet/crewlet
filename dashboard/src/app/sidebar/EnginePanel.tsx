/**
 * The engine's own state, at the end of the sidebar head's line, beside the
 * company's name: one compact status, and the panel it opens.
 *
 * # One status, one width
 *
 * The status is the kit's head status (`AppShell.RailStatus`): a dot, and,
 * while alarms stand, how many. Its words ("Engine serving, 1 alarm") are its
 * name and its title rather than a line of the rail, and it reserves its
 * width, so the company's name never loses a character when the engine's
 * state changes. Pressing it opens the ENGINE PANEL below it (the kit's
 * `Popover`): the state again with this node's name, the fleet, the
 * configuration and the uptime, the alarms by name, and a way to each place
 * that acts on them.
 *
 * Two states are not about the engine at all but about THIS BROWSER, and the
 * engine serves it nothing to put in a panel. Nobody signed in is the one a
 * press can fix, so there the status is the button that goes to the sign-in and
 * opens no panel. A refusal of somebody the engine knows is fixed by an
 * admin: the status STATES it and is no control at all (the kit's static
 * head status: no pointer, never focused), and the panel drawn in place
 * of the screen carries the retry and the sign-out. A statement wearing a
 * control's marks reads as a control that does nothing.
 *
 * # One state, chosen by precedence
 *
 * A node can be several things at once (unreachable AND draining, configured
 * AND diverged), and a status that listed them all is one nobody reads. So the
 * status says the ONE that decides what a reader should do, in this order:
 *
 *   1. nobody is signed in: the engine accepted no session from this
 *      browser, and nothing else on screen can be trusted;
 *   2. the engine knows who this is and will not serve them (a seat taken out
 *      of the chart), in danger — never for want of access, since the socket
 *      serves every signed-in reader the overview;
 *   3. the engine was lost: everything shown is the last push;
 *   4. nothing has reported yet: the tab is still connecting, which is not
 *      "healthy" and not a fault either, so its dot is neutral;
 *   5. this node is draining: it is leaving and hands its seats on;
 *   6. this node is FALLING BEHIND: its watched duty has missed a beat, and
 *      the push's `stall_lag_seconds` says for how long. The figure climbs
 *      towards the lease TTL, at which the watchdog ends the process, so it
 *      outranks every state below, each of which waits for a person;
 *   7. no configuration is applied: STARTING while its first apply runs (the
 *      posture says `wait`, and the dot is neutral, since nothing needs a
 *      person), and no company otherwise;
 *   8. the node's posture diverged from the fleet (`shed`, `stuck`,
 *      `isolated`): it is running a configuration nobody else is;
 *   9. serving: "Engine healthy" ONLY when the alarm table was evaluated and
 *      nothing in it fires, and "Engine serving" otherwise.
 *
 * THE DOT IS THE POSTURE, AND ONLY THE POSTURE. A standing alarm is a second
 * fact, the count beside the words with its own tone and glyph, rather than
 * a recoloured dot: borrowing the dot drew a green title beside an amber dot.
 * But "HEALTHY" IS A CLAIM THE ALARMS CAN REFUTE, so over a standing alarm,
 * or a table not evaluated yet, the words say what the posture says, that
 * the node is serving.
 *
 * # Where each fact comes from
 *
 * Every word the status says comes off the ONE health push (`useEngineHealth`) and
 * the connection; nothing there polls. The alarm COUNT is the push's, which
 * is the evaluation the engine's gauge and log lines come from. Their NAMES
 * are not on the push, which is public and carries only the one that has
 * stood longest; they are the retention report, the same document Settings ›
 * Backups & retention lists them from, asked for when the panel opens and
 * again when the count moves, and only of a reader holding the access that
 * section opens on (Admin). Each is named by the rule in
 * `lib/alarms.ts`. A reader without that grant, or a report not answered yet,
 * gets the longest-standing alarm by name and how many more stand.
 *
 * # What it cannot say
 *
 * A NODE DOWN is a smaller count and nothing more: the push counts the nodes
 * holding a presence lease and does not know how many a fleet should have, so
 * the panel says "2 live" rather than a count of the missing it cannot know.
 */

import { useEffect, useRef } from "react";
import {
  AppShell,
  Button,
  ButtonLink,
  DescriptionList,
  Inline,
  Popover,
  Spacer,
  Stack,
  StatusDot,
  Text,
} from "@crewlethq/ui";
import { KeyGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { alarmKindTitle, alarmsLink, namedAlarms } from "~/lib/alarms.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtElapsed, fmtUptime, relTime, tsKey } from "~/lib/format.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { EngineHealth } from "~/contract/health.ts";

/** The postures a node holds while serving the configuration the fleet agreed on. */
const AGREED = new Set(["", "serve", "wait"]);

/** Which of the engine's states the status says. */
export type EngineState =
  | "signed-out"
  | "refused"
  | "unreachable"
  | "connecting"
  | "draining"
  | "behind"
  | "starting"
  | "unconfigured"
  | "diverged"
  | "serving"
  | "healthy";

export interface EngineReading {
  state: EngineState;
  /** The dot's tone, which is the posture's. */
  tone: "success" | "warning" | "danger" | "neutral";
  /** The state in words: the status's name, and the panel's head. A few, so it reads as one. */
  title: string;
  /** What the panel says under its head, where the state needs it said. */
  lead?: string;
  /** The push the reading was made from, or null where there is none to show. */
  health: EngineHealth | null;
  /** When the engine was lost, for "Last update"; null while it is not. */
  lostAt: number | null;
}

/** What the status says, decided once and testable without a render. */
export function engineReading(input: {
  connected: boolean;
  authRejected: boolean;
  /** Why the engine will not serve a browser it knows, or null. */
  accessRefused?: string | null;
  lostAt: number | null;
  everConnected: boolean;
  health: EngineHealth | null;
}): EngineReading {
  const { connected, authRejected, accessRefused = null, lostAt, everConnected, health } = input;
  if (authRejected) {
    return {
      state: "signed-out",
      tone: "danger",
      title: "Not signed in",
      lead: "The engine accepted no session from this browser",
      health: null,
      lostAt,
    };
  }
  if (accessRefused !== null) {
    return {
      state: "refused",
      tone: "danger",
      title: "Blocked",
      lead: accessRefused ? `Signed in, but blocked: ${accessRefused}` : "Signed in, but blocked",
      health: null,
      lostAt: null,
    };
  }
  if (!connected && lostAt !== null) {
    return {
      state: "unreachable",
      tone: "warning",
      title: "Engine unreachable",
      // TRUE ONLY OF A TAB THAT RECEIVED ONE: a tab whose first dial failed
      // is showing nothing, not the last of anything.
      ...(everConnected ? { lead: "Showing the last state received" } : {}),
      health: null,
      lostAt,
    };
  }
  if (!connected || !health) {
    return {
      state: "connecting",
      tone: "neutral",
      title: "Connecting",
      lead: "No health report yet",
      health: null,
      lostAt: null,
    };
  }
  const reading = { health, lostAt: null };
  if (health.shutting_down) {
    return {
      ...reading,
      state: "draining",
      tone: "warning",
      title: "Draining",
      lead: "Shutting down",
    };
  }
  // PRESENT ONLY WHILE THE DUTY IS BEHIND: the engine sends the field once
  // the watched duty has missed its beat, and then the whole time since that
  // beat, so its presence is the state and its value the lead.
  const behind = health.stall_lag_seconds;
  if (behind != null && behind > 0) {
    return {
      ...reading,
      state: "behind",
      tone: "warning",
      title: "Falling behind",
      lead: `${fmtElapsed(behind * 1000)} behind`,
    };
  }
  if (health.configured === false) {
    // ITS FIRST APPLY IS RUNNING: the activation pointer names a revision
    // this node has not applied yet, which the posture reports as `wait` for
    // the whole of the apply. A node that boots spends seconds here, and none
    // of it needs a person.
    if (health.posture === "wait") {
      return { ...reading, state: "starting", tone: "neutral", title: "Engine starting" };
    }
    return {
      ...reading,
      state: "unconfigured",
      tone: "warning",
      title: "No configuration",
      lead: "No company is active, so no seat runs",
    };
  }
  const posture = health.posture ?? "";
  if (!AGREED.has(posture)) {
    return {
      ...reading,
      state: "diverged",
      tone: "danger",
      title: `Engine ${posture}`,
      lead: "Not on the current configuration",
    };
  }
  // HEALTHY ONLY WITH NOTHING TO REFUTE IT: a standing alarm, or a table not
  // evaluated yet, is exactly what the word would contradict.
  return health.alarms?.count === 0
    ? { ...reading, state: "healthy", tone: "success", title: "Engine healthy" }
    : { ...reading, state: "serving", tone: "success", title: "Engine serving" };
}

export function EnginePanel({
  reading,
  readsReport,
  onSignIn,
}: {
  reading: EngineReading;
  /**
   * Whether this reader may ask the retention report, which names the alarms:
   * whether they hold the access Settings › Backups & retention opens on.
   */
  readsReport: boolean;
  onSignIn: () => void;
}) {
  // NOBODY SIGNED IN IS THE ONE STATE A PRESS CAN FIX, so the status is that
  // button, named for what pressing it does.
  if (reading.state === "signed-out") {
    return (
      <AppShell.RailStatus
        icon={<StatusDot tone="danger" />}
        trailing={<KeyGlyph size="xs" />}
        label={`${reading.title}. Sign in`}
        onClick={onSignIn}
      >
        {reading.title}
      </AppShell.RailStatus>
    );
  }
  // A STATEMENT WEARS NOTHING A CONTROL WEARS. A refusal of somebody the
  // engine knows is repaired by an admin: the status says so, and is the
  // kit's static status, with no pointer and no
  // place in the tab order. The panel in place of the screen says the rest.
  if (reading.state === "refused") {
    return (
      <AppShell.RailStatus icon={<StatusDot tone={reading.tone} />}>
        {reading.title}
      </AppShell.RailStatus>
    );
  }
  const count = reading.health?.alarms?.count ?? 0;
  return (
    <Popover
      label="Engine"
      // BELOW THE STATUS IN EITHER SHAPE: on the head's line in the full
      // sidebar, and on its own row under the mark in the compact rail.
      side="bottom"
      // INTO THE ROOM BESIDE THE RAIL: the status ends on the rail's inner
      // edge, so a panel aligned to its start opens over the screen rather
      // than back across the rail (and is clamped to the drawer on a phone).
      align="start"
      trigger={(_open, toggle) => (
        <AppShell.RailStatus
          icon={<StatusDot tone={reading.tone} />}
          count={
            count > 0
              ? {
                  value: count,
                  label: count === 1 ? "alarm" : "alarms",
                  mark: <TriangleAlertGlyph size="xs" />,
                  tone: "warning",
                }
              : undefined
          }
          onClick={toggle}
        >
          {reading.title}
        </AppShell.RailStatus>
      )}
    >
      {(close) => <PanelBody reading={reading} readsReport={readsReport} close={close} />}
    </Popover>
  );
}

/** What the panel says. Mounted only while it is open, so nothing here runs for a closed panel. */
function PanelBody({
  reading,
  readsReport,
  close,
}: {
  reading: EngineReading;
  readsReport: boolean;
  close: () => void;
}) {
  const now = useNow();
  const { socket } = useClient();
  const { health } = reading;
  const count = health?.alarms?.count ?? 0;
  const alarms = alarmsLink();
  return (
    <Stack gap={3} className="engine-panel">
      <Inline gap={2}>
        <StatusDot tone={reading.tone}>
          <Text variant="cell">{reading.title}</Text>
        </StatusDot>
        {health ? (
          <>
            <Spacer />
            <Text variant="caption" mono>
              {health.node}
            </Text>
          </>
        ) : null}
      </Inline>
      {reading.lead ? (
        <Text variant="caption" tone="secondary">
          {reading.lead}
        </Text>
      ) : null}
      {health ? (
        <DescriptionList fit dense>
          <DescriptionList.Item term="Nodes">
            {/* THE LIVE COUNT, said as live. Absent where the presence read
                failed, which is not a zero. */}
            {health.nodes != null && health.nodes > 0
              ? `${health.nodes} live`
              : "Count unavailable"}
          </DescriptionList.Item>
          <DescriptionList.Item term="Config">
            {health.configured
              ? `Epoch ${health.applied_epoch}`
              : reading.state === "starting"
                ? "Applying"
                : "None applied"}
          </DescriptionList.Item>
          <DescriptionList.Item term="Up">
            {fmtUptime(now - tsKey(health.started_at))}
          </DescriptionList.Item>
          <DescriptionList.Item term="Alarms">
            {/* AN ABSENT TABLE IS NOT AN EMPTY ONE: the engine has not
                evaluated it, mid-boot or on a node running no state log. */}
            {!health.alarms ? (
              "Not evaluated"
            ) : count === 0 ? (
              "None"
            ) : (
              <AlarmNames count={count} worst={health.alarms.worst} readsReport={readsReport} />
            )}
          </DescriptionList.Item>
        </DescriptionList>
      ) : reading.state === "unreachable" && reading.lead && reading.lostAt !== null ? (
        <DescriptionList fit dense>
          <DescriptionList.Item term="Last update">
            {relTime(new Date(reading.lostAt).toISOString(), now)}
          </DescriptionList.Item>
        </DescriptionList>
      ) : null}
      {reading.state === "unreachable" ? (
        <Inline gap={2} wrap>
          <Button
            variant="secondary"
            size="small"
            onClick={() => {
              // NOW, rather than at the end of the backoff, which reaches half
              // a minute between tries. What follows is the status's to say.
              socket.reconnect();
              close();
            }}
          >
            Reconnect now
          </Button>
        </Inline>
      ) : health ? (
        <Inline gap={2} wrap>
          <ButtonLink
            variant="secondary"
            size="small"
            href={href(["settings", "nodes", health.node])}
            onClick={close}
          >
            Open node
          </ButtonLink>
          {count > 0 ? (
            <ButtonLink
              variant="secondary"
              size="small"
              href={href(alarms.path, alarms.query)}
              onClick={close}
            >
              Open alarms
            </ButtonLink>
          ) : null}
        </Inline>
      ) : null}
    </Stack>
  );
}

/**
 * The standing alarms by name, the longest-standing first.
 *
 * Asked of the retention report while the panel is open, and asked again when
 * the push's count moves, so the names follow the count beside them rather
 * than a poll of their own.
 */
function AlarmNames({
  count,
  worst,
  readsReport,
}: {
  count: number;
  worst: string | undefined;
  readsReport: boolean;
}) {
  const report = useQuery("retention", undefined, { enabled: readsReport });
  const { refetch } = report;
  const seen = useRef(count);
  useEffect(() => {
    if (seen.current === count) return;
    seen.current = count;
    refetch();
  }, [count, refetch]);
  const named = report.data ? namedAlarms(report.data.alarms ?? [], worst) : [];
  if (named.length > 0) {
    return (
      <Stack as="ul" gap={1}>
        {named.map((alarm) => (
          <li key={alarm.key}>{alarm.title}</li>
        ))}
      </Stack>
    );
  }
  // THE PUSH'S OWN, where the names cannot be asked or have not come: the one
  // that has stood longest by name, and how many more stand.
  const more = worst ? count - 1 : count;
  return (
    <Stack as="ul" gap={1}>
      {worst ? <li>{alarmKindTitle(worst)}</li> : null}
      {more > 0 ? (
        <li>
          <Text variant="caption">{worst ? `${more} more` : `${more} standing`}</Text>
        </li>
      ) : null}
    </Stack>
  );
}
