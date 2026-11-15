/**
 * What Settings › Delegated decisions WRITES: the switch, a rule changed in
 * its sheet, a rule removed after asking. Each is a merge patch of the
 * company document the engine checks first, carrying the revision it was
 * read from and a summary for the history.
 *
 * Its own file because `DelegatedDecisions.test.tsx` mocks the query hook,
 * and this suite answers the socket instead, as the frame does.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { DelegatedDecisionsAnswer } from "~/contract/delegated.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  READER_VIEWER,
  savedAnswer,
  stubEngine,
  validAnswer,
  type EngineCall,
} from "~/test/settingsScreen.tsx";
import { DelegatedDecisions } from "./DelegatedDecisions.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const SSH = { kinds: ["ssh_operate"], delegates: ["security-engineer"], escalate_to: ["ops-lead"] };
const RELEASE = {
  kinds: ["pr_merge", "deploy"],
  delegates: ["release-reviewer"],
  escalate_to: ["ops-lead"],
  sample_one_in: 5,
  always_escalate_paths: ["billing/**"],
  required_checks: [{ repo: "acme/api", branch: "main", checks: ["test"] }],
};

/** The document as `GET /config` answers it. */
const DOC = { name: "Acme", delegated_decisions: { enabled: true, rules: [SSH, RELEASE] } };

const ANSWER: DelegatedDecisionsAnswer = {
  enabled: true,
  rules: [
    { rule: 1, ...SSH, sample_one_in: 10, always_escalate_paths: [], required_checks: [] },
    { rule: 2, ...RELEASE },
  ] as DelegatedDecisionsAnswer["rules"],
  limits: [
    {
      key: "delegated_decisions.deadline_seconds",
      group: "delegated",
      unit: "seconds",
      value: 900,
      default: 900,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 300, max: 1800 },
    },
    {
      key: "delegated_decisions.allows_per_hour",
      group: "delegated",
      unit: "count",
      value: 30,
      default: 30,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 1, max: 60 },
    },
    {
      key: "delegated_decisions.server_allows_per_hour",
      group: "delegated",
      unit: "count",
      value: 10,
      default: 10,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 1, max: 60 },
    },
  ],
  day: "2026-11-10",
  since: "2026-11-10T00:00:00Z",
  until: "2026-11-11T00:00:00Z",
  complete: false,
  decided: [],
  decided_more: false,
  spot_checks: [],
  spot_checks_more: false,
  flagged: [],
  flagged_more: false,
};

const ORG = {
  timezone: "UTC",
  roles: [
    { name: "Ops Lead", handle: "ops-lead", kind: "human" },
    { name: "Security Engineer", handle: "security-engineer", kind: "agent" },
    { name: "Release Reviewer", handle: "release-reviewer", kind: "agent" },
  ],
};

async function show(viewer?: Record<string, unknown>) {
  const mounted = mountSettings(
    <DelegatedDecisions />,
    { delegated_decisions: ANSWER },
    { hash: "#/settings/delegated-decisions", viewer, org: ORG },
  );
  await act(afterMicrotasks);
  return mounted;
}

/** The engine: the document on a read, valid on a check, stored on a save. */
const engine = (call: EngineCall): Response => {
  if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
  if (call.path.includes("dry_run=true")) return validAnswer();
  return savedAnswer();
};

const writes = (calls: EngineCall[]) =>
  calls.filter((c) => c.method === "PATCH" && !c.path.includes("dry_run"));

// OFF IS THE DEFAULT, so turning delegation off removes the key.
test("the switch turns delegation off as a patch that removes the key", async () => {
  const calls = stubEngine(engine);
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("switch", { name: "On" }));
  });
  await act(afterMicrotasks);
  const [save] = writes(calls);
  expect(save!.headers.get("If-Match")).toBe('"r1"');
  expect(save!.body).toEqual({
    delegated_decisions: { enabled: null },
    _summary: "Delegated decisions: turned off",
  });
  expect(screen.getByText("Saved, applying…")).toBeTruthy();
});

// FILLED FROM THE DOCUMENT, and the WHOLE list written back: a merge patch
// replaces a list, so the rule beside the one changed is sent as it was.
test("a rule's sheet is filled from the document and its save writes the whole list", async () => {
  const calls = stubEngine(engine);
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Change rule 2" }));
  });
  await act(afterMicrotasks);
  const sheet = screen.getByRole("dialog", { name: "Change rule 2" });
  const rate = within(sheet).getByLabelText(/Spot-check one in/) as HTMLInputElement;
  expect(rate.value).toBe("5");
  expect(within(sheet).getByText("billing/**")).toBeTruthy();
  fireEvent.change(rate, { target: { value: "3" } });
  await act(async () => {
    fireEvent.submit(rate.closest("form")!);
  });
  await act(afterMicrotasks);
  const [save] = writes(calls);
  expect(save!.body).toEqual({
    delegated_decisions: { rules: [SSH, { ...RELEASE, sample_one_in: 3 }] },
    _summary: "Delegated decisions: change rule 2 (Merges and Deploys)",
  });
  // A SAVE CLOSES THE SHEET; the card says the change is applying.
  expect(screen.queryByRole("dialog", { name: "Change rule 2" })).toBeNull();
  expect(screen.getByText("Saved, applying…")).toBeTruthy();
});

test("the engine's refusal of a rule is said in the sheet, and nothing is saved", async () => {
  const refusal =
    "delegated_decisions.rules[1].escalate_to[0]: ops-lead is not a person; an escalation goes to people";
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    return engineAnswer(
      {
        error: "invalid_patch",
        detail: refusal,
        problems: [
          {
            path: "delegated_decisions.rules[1].escalate_to[0]",
            segments: null,
            kind: "invalid",
            message: refusal,
          },
        ],
      },
      400,
    );
  });
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Change rule 2" }));
  });
  await act(afterMicrotasks);
  const sheet = screen.getByRole("dialog", { name: "Change rule 2" });
  const rate = within(sheet).getByLabelText(/Spot-check one in/);
  fireEvent.change(rate, { target: { value: "3" } });
  await act(async () => {
    fireEvent.submit(rate.closest("form")!);
  });
  await act(afterMicrotasks);
  expect(within(sheet).getByText(refusal)).toBeTruthy();
  expect(writes(calls)).toEqual([]);
});

// ASKED FIRST, saying what moves: an escalation names a rule by its number.
test("a rule is removed after asking, and the later rules move up", async () => {
  const calls = stubEngine(engine);
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Remove rule 1" }));
  });
  const ask = screen.getByRole("alertdialog", { name: "Remove rule 1?" });
  expect(within(ask).getByText(/Rules after it move up one: rule 2 becomes rule 1\./)).toBeTruthy();
  expect(calls).toEqual([]);
  await act(async () => {
    fireEvent.click(within(ask).getByRole("button", { name: "Remove rule" }));
  });
  await act(afterMicrotasks);
  const [save] = writes(calls);
  expect(save!.body).toEqual({
    delegated_decisions: { rules: [RELEASE] },
    _summary: "Delegated decisions: remove rule 1 (Commands on servers)",
  });
});

// NEVER HIDDEN: every control is drawn for a reader who cannot write the
// document, disabled with the reason.
test("a reader without the grant sees every control disabled with the reason", async () => {
  await show(READER_VIEWER);
  const reason = CONFIG_WRITE_REASONS.no_grant;
  for (const control of [
    screen.getByRole("button", { name: /Add a rule/ }),
    screen.getByRole("button", { name: "Change rule 1" }),
    screen.getByRole("button", { name: "Remove rule 2" }),
  ]) {
    expect(control.getAttribute("title")).toBe(reason);
  }
  expect(screen.getByRole("switch", { name: "On" }).getAttribute("title")).toBe(reason);
});

/** Opens one limit's field from its pencil, types `value` and submits it. */
async function changeLimit(name: RegExp, value: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
}

// THE LIMITS ARE THE DOCUMENT'S, changed where they are read by the field
// Settings › Limits changes a cap with: checked by the engine first, then
// saved as a patch of the one key, on the revision the read named.
test("a limit the engine enforces is changed in place as a patch of its one key", async () => {
  const calls = stubEngine(engine);
  await show();
  const limits = screen.getByRole("table", { name: "What Crewlet enforces" });
  const deadline = within(limits).getByRole("row", { name: /goes to people after/ });
  expect(within(deadline).getByText("15 minutes")).toBeTruthy();
  expect(within(deadline).getByText("Default")).toBeTruthy();
  await changeLimit(/Change A request no delegate answered goes to people after/, "1200");
  const [save] = writes(calls);
  expect(save!.headers.get("If-Match")).toBe('"r1"');
  expect(save!.body).toEqual({
    delegated_decisions: { deadline_seconds: 1200 },
    _summary:
      "Delegated decisions: A request no delegate answered goes to people after set to 1200 " +
      "(delegated_decisions.deadline_seconds)",
  });
  // THE SAVED VALUE, MARKED AS APPLYING, in the row it was changed in.
  const row = within(screen.getByRole("table", { name: "What Crewlet enforces" })).getByRole(
    "row",
    {
      name: /goes to people after/,
    },
  );
  expect(within(row).getByText("20 minutes")).toBeTruthy();
  expect(within(row).getByText("applying…")).toBeTruthy();
});

// THE ENGINE'S RANGE, said beside the field before anything is sent: no cap
// can be switched off or stretched past what it bounds.
test("a limit outside its range is said beside the field, and nothing is sent", async () => {
  const calls = stubEngine(engine);
  await show();
  await changeLimit(/Change Allows per kind and delegate/, "61");
  expect(calls).toEqual([]);
  expect(screen.getByRole("textbox").getAttribute("aria-invalid")).toBe("true");
  expect(screen.getAllByText("From 1 to 60.").length).toBeGreaterThan(0);
});

// NEVER HIDDEN: a person a rule escalates to reads the limits, and sees each
// pencil disabled with the reason.
test("a reader who may not change the document sees each limit's pencil held with the reason", async () => {
  await show(READER_VIEWER);
  const pencil = screen.getByRole("button", { name: /Change Commands allowed per server/ });
  expect(
    pencil.getAttribute("aria-disabled") ?? String((pencil as HTMLButtonElement).disabled),
  ).toBe("true");
  const described = (pencil.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toContain(CONFIG_WRITE_REASONS.no_grant);
});
