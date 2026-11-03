/**
 * The environment every other suite stands on, asserted rather than assumed.
 *
 * Three settings decide whether whole families of suites mean anything, and
 * each fails SILENTLY when it stops holding:
 *
 *  - THE ZONE. The suites run in Europe/Berlin, which has an offset all year
 *    and two transitions, because under UTC — the zone of every CI runner,
 *    container and release box — a local midnight is an exact multiple of a
 *    day apart and a value bucketed in the reader's zone is indistinguishable
 *    from one bucketed in the engine's. A config edit that dropped `env.TZ`
 *    would leave every date suite green and every date bug unreproducible.
 *  - THE KIT'S STYLESHEETS. Every uilet component imports its own CSS as a side
 *    effect, and Node has no loader for a `.css` file: without the kit inlined
 *    through Vite, every suite whose graph reaches a component fails to LOAD,
 *    and a suite that fails to load reports no tests rather than a failure
 *    anybody reads.
 *  - THE BOUNDS (`bounds.ts`). A wait and a case are hang detectors sized
 *    for a machine the gate shares; back at the tools' defaults, every suite
 *    stays green on an idle machine and goes red on a loaded one for a reason
 *    nothing on the page caused.
 *
 * So all three are held here, by what they produce rather than by reading the
 * config files: an offset the zone must give, a component the kit must render
 * with its own class names, and the bounds a wait, a case and a hook actually
 * get.
 */

import { cleanup, getConfig, render, screen } from "@testing-library/react";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  onTestFinished,
  test,
  TestRunner,
  vi,
} from "vitest";
import { Button } from "@crewlethq/ui";
import { CASE_BOUND_MS, HOOK_BOUND_MS, WAIT_BOUND_MS } from "./bounds.ts";
import { eventually } from "./settle.ts";

afterEach(cleanup);

test("the suite runs in Berlin, with its offset and both of its transitions", () => {
  expect(process.env.TZ).toBe("Europe/Berlin");
  // CET in winter, CEST in summer: getTimezoneOffset is minutes WEST of UTC.
  expect(new Date("2026-01-15T12:00:00Z").getTimezoneOffset()).toBe(-60);
  expect(new Date("2026-07-15T12:00:00Z").getTimezoneOffset()).toBe(-120);
  // THE SHORT DAY AND THE LONG ONE: local midnight to local midnight across
  // the spring transition is 23 hours and across the autumn one 25. A zone
  // without transitions answers 24 for both, which is exactly the arithmetic
  // this setting exists to catch.
  const hours = (from: Date, to: Date) => (to.getTime() - from.getTime()) / 3_600_000;
  expect(hours(new Date(2026, 2, 29), new Date(2026, 2, 30))).toBe(23);
  expect(hours(new Date(2026, 9, 25), new Date(2026, 9, 26))).toBe(25);
});

test("a kit component loads with its stylesheet and renders its own classes", () => {
  // Reaching this line at all is half the assertion: the import above pulls
  // `Button`'s own `.css`, which Node alone cannot load.
  render(<Button variant="secondary">Save</Button>);
  const button = screen.getByRole("button", { name: "Save" });
  expect(button.className).toContain("crewlet-btn");
  expect(button.className).toContain("crewlet-btn--secondary");
});

describe("the bounds", () => {
  // A hook registered with no bound of its own, so the runner records the
  // suite's: what the case below reads.
  beforeEach(() => {});

  test("a wait, a case and a hook get the bounds the suite sized, the wait well inside the case", ({
    task,
  }) => {
    expect(getConfig().asyncUtilTimeout).toBe(WAIT_BOUND_MS);
    expect(task.timeout).toBe(CASE_BOUND_MS);
    // A HOOK'S BOUND is read where the runner keeps it: Vitest stamps every
    // `beforeEach` it registers with the bound it was registered under, under
    // a registered symbol, because the hook's clean-up runs under the same
    // one. No public call reports it, and a release that stopped stamping it
    // would fail this line by reading nothing rather than pass it. The bound
    // sized happens to be Vitest's own default today (`bounds.ts` says why),
    // so this holds the figure in effect rather than the line that sets it: a
    // configuration naming another, or a release moving the default, fails it.
    const [hook] = TestRunner.getSuiteHooks(task.suite!).beforeEach;
    expect((hook as unknown as Record<symbol, unknown>)[Symbol.for("VITEST_CLEANUP_TIMEOUT")]).toBe(
      HOOK_BOUND_MS,
    );
    // A hung wait must fail as itself, with the DOM it searched, rather than
    // be overtaken by the case's bound — and a case holds several waits.
    expect(3 * WAIT_BOUND_MS).toBeLessThanOrEqual(CASE_BOUND_MS);
  });

  // AND VITEST'S OWN WAIT, which a case on a faked clock waits with, gets the
  // same bound rather than its one-second default.
  test("Vitest's wait, through eventually, gets the wait's bound", async () => {
    const wait = vi.spyOn(vi, "waitFor");
    onTestFinished(() => wait.mockRestore());
    await expect(eventually(() => "held")).resolves.toBe("held");
    expect(wait).toHaveBeenCalledOnce();
    expect(wait.mock.calls[0]![1]).toEqual({ timeout: WAIT_BOUND_MS });
  });
});
