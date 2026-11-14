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
  limits: { deadline_minutes: 15, decisions_per_hour: 30, server_allows_per_hour: 10 },
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
