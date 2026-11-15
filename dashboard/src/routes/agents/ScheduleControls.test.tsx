/**
 * A schedule's controls after a change saved from them: what they offer before
 * this node applies it, and the read that brings the page up to it.
 *
 * The page draws this node's applied company, read every thirty seconds, so a
 * pause saved here lands in it only once the node applies the revision. The
 * controls stopped at "saved" and went on deciding from the row as it was: a
 * schedule paused a moment ago still offered Run now, which the engine refused
 * because the schedule is switched off, and Pause again.
 */

import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ScheduleRow } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Schedules } from "./Schedules.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const SCHEDULE = { name: "standup", cron: "0 9 * * *", task: "Run the standup." };

function row(over: Partial<ScheduleRow> = {}): ScheduleRow {
  return {
    scope_type: "role",
    scope_id: "pm",
    scope_name: "pm",
    name: "standup",
    cron: "0 9 * * *",
    timezone: "UTC",
    task: "Run the standup.",
    target: "",
    enabled: true,
    timeout_seconds: 1800,
    catchup: true,
    runners: ["pm"],
    next_run: "2026-06-16T09:00:00Z",
    ...over,
  };
}

/** The schedule's page, as an admin who may run it, over `served` schedules. */
async function mountPage(served: () => ScheduleRow[]) {
  const mounted = mountSettings(
    <Schedules scope={["role", "pm", "standup"]} />,
    {
      schedules: () => ({ schedules: served(), recent_runs: [] }),
      schedule_runs: { runs: [], truncated: false },
    },
    {
      hash: "#/company/schedules/role/pm/standup",
      viewer: { ...ADMIN_VIEWER, acts: ["run_schedule"] },
      org: {
        timezone: "UTC",
        roles: [{ name: "PM", handle: "pm", kind: "agent", schedules: [SCHEDULE] }],
      },
    },
  );
  await act(afterMicrotasks);
  return mounted;
}

/** The sentence a held button is described by. */
function heldReason(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

test("a pause just saved holds Run now with the reason, and the page is read again once applied", async () => {
  const calls = stubEngine((call) => {
    if (call.method === "GET") {
      return engineAnswer({ name: "PM", handle: "pm", schedules: [SCHEDULE] }, 200, {
        ETag: '"r1"',
      });
    }
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  let served = [row()];
  const { store, asked } = await mountPage(() => served);
  const runNow = () => screen.getByRole("button", { name: /Run now/ });
  expect(runNow().getAttribute("aria-disabled")).not.toBe("true");

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Pause/ }));
  });
  await act(afterMicrotasks);
  expect(calls.filter((c) => c.method === "PUT" && !c.path.includes("dry_run"))).toHaveLength(1);

  // SAVED, NOT YET APPLIED HERE: this node still serves the schedule as
  // switched on, and the controls draw the switch as saved.
  expect(runNow().getAttribute("aria-disabled")).toBe("true");
  expect(heldReason(runNow())).toContain("Paused. Resume it to run it.");
  expect(screen.getByRole("button", { name: /Resume/ })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Pause/ })).toBeNull();

  // APPLIED: the schedules are read again, and the page draws what was read.
  const before = asked.filter((q) => q === "schedules").length;
  served = [row({ enabled: false, next_run: "" })];
  await act(async () => {
    store.applyHealth(healthFrame({ applied_epoch: 5 }));
  });
  await act(afterMicrotasks);
  expect(asked.filter((q) => q === "schedules").length).toBeGreaterThan(before);
  expect(screen.getByText(/Nothing while it is paused/)).toBeTruthy();
  expect(runNow().getAttribute("aria-disabled")).toBe("true");
});
