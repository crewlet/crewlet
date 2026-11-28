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
import { typeSetting } from "~/test/settingField.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { toastText } from "~/testing.tsx";
import { APPLYING_SENTENCE } from "~/lib/savedToast.ts";
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
  sample: { default: 10, min: 1, max: 1000 },
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

/** The company's GitHub as the rule form reads it: two repositories, a branch list and checks. */
const GITHUB = {
  delegated_repositories: {
    connected: true,
    repos: [
      { repo: "acme/api", default_branch: "main", archived: false },
      { repo: "acme/web", default_branch: "trunk", archived: false },
      { repo: "acme/old", default_branch: "main", archived: true },
    ],
  },
  delegated_branches: (params: { repo: string }) => ({
    repo: params.repo,
    branches: ["main", "release", "trunk"],
    more: false,
  }),
  delegated_checks: (params: { repo: string; branch: string }) => ({
    repo: params.repo,
    branch: params.branch,
    checks: ["build", "e2e"],
  }),
};

async function show(viewer?: Record<string, unknown>) {
  const mounted = mountSettings(
    <DelegatedDecisions />,
    { delegated_decisions: ANSWER, ...GITHUB },
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
  // CONFIRMED BY ITS TOAST, never a line beside the switch.
  expect(toastText()).toContain("Delegated decisions: turned off");
  expect(toastText()).toContain(APPLYING_SENTENCE);
  expect(screen.queryByText(/Saved, applying/)).toBeNull();
});

// FILLED FROM THE DOCUMENT, and the WHOLE list written back: a merge patch
// replaces a list, so the rule beside the one changed is sent as it was.
test("a rule's sheet is filled from the document and its save writes the whole list", async () => {
  const calls = stubEngine(engine);
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 2" }));
  });
  await act(afterMicrotasks);
  const sheet = screen.getByRole("dialog", { name: "Edit rule 2" });
  expect(within(sheet).getByRole("radio", { name: "1 in 5" }).getAttribute("aria-checked")).toBe(
    "true",
  );
  expect(within(sheet).getByText(/review about 1 in 5 of the agent's decisions/)).toBeTruthy();
  expect(within(sheet).getByText("billing/**")).toBeTruthy();
  fireEvent.click(within(sheet).getByRole("radio", { name: "Custom" }));
  const rate = within(sheet).getByLabelText("Spot-check one decision in") as HTMLInputElement;
  expect(rate.value).toBe("5");
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
  // A SAVE CLOSES THE SHEET; its toast says the change is applying.
  expect(screen.queryByRole("dialog", { name: "Edit rule 2" })).toBeNull();
  expect(toastText()).toContain("Delegated decisions: change rule 2 (Merges and Deploys)");
  expect(toastText()).toContain(APPLYING_SENTENCE);
  expect(screen.queryByText(/Saved, applying/)).toBeNull();
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
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 2" }));
  });
  await act(afterMicrotasks);
  const sheet = screen.getByRole("dialog", { name: "Edit rule 2" });
  fireEvent.click(within(sheet).getByRole("radio", { name: "Every decision" }));
  await act(async () => {
    fireEvent.submit(sheet.querySelector("form")!);
  });
  await act(afterMicrotasks);
  // UNDER THE FIELD IT NAMES, without the document path; the callout points there.
  const people = within(sheet).getByRole("combobox", { name: "Escalates to" });
  expect(people.getAttribute("aria-invalid")).toBe("true");
  expect(
    within(sheet).getByText("ops-lead is not a person; an escalation goes to people"),
  ).toBeTruthy();
  expect(within(sheet).getByText("Fix the fields marked above.")).toBeTruthy();
  expect(within(sheet).queryByText(refusal)).toBeNull();
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
    _summary: "Delegated decisions: remove rule 1 (Server commands)",
  });
});

// NEVER HIDDEN: every control is drawn for a reader who cannot write the
// document, disabled with the reason.
test("a reader without the grant sees every control disabled with the reason", async () => {
  await show(READER_VIEWER);
  const reason = CONFIG_WRITE_REASONS.no_grant;
  const described = (control: HTMLElement) =>
    document.getElementById(control.getAttribute("aria-describedby")?.split(" ").pop() ?? "")
      ?.textContent;
  for (const control of [
    screen.getByRole("button", { name: /Add rule/ }),
    screen.getByRole("button", { name: "Edit rule 1" }),
    screen.getByRole("button", { name: "Remove rule 2" }),
    screen.getByRole("switch", { name: "On" }),
  ]) {
    expect(described(control)).toBe(reason);
  }
});

/** Opens one limit's field from its pencil, types `value` and submits it. */
async function changeLimit(name: RegExp, value: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
  const field = await typeSetting(value);
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
  const deadline = within(limits).getByRole("row", { name: /must answer within/ });
  expect(within(deadline).getByText("15 minutes")).toBeTruthy();
  expect(within(deadline).getByText("Default")).toBeTruthy();
  await changeLimit(/Change An agent must answer within/, "20m");
  const [save] = writes(calls);
  expect(save!.headers.get("If-Match")).toBe('"r1"');
  expect(save!.body).toEqual({
    delegated_decisions: { deadline_seconds: 1200 },
    _summary:
      "Delegated decisions: An agent must answer within set to 20 minutes " +
      "(delegated_decisions.deadline_seconds)",
  });
  // THE SAVED VALUE, in the row it was changed in; its toast says it is applying.
  const row = within(screen.getByRole("table", { name: "What Crewlet enforces" })).getByRole(
    "row",
    {
      name: /must answer within/,
    },
  );
  expect(within(row).getByText("20 minutes")).toBeTruthy();
  expect(within(row).queryByText(/applying/)).toBeNull();
  expect(toastText()).toContain(APPLYING_SENTENCE);
});

// THE ENGINE'S RANGE, said beside the field before anything is sent: no cap
// can be switched off or stretched past what it bounds.
test("a limit outside its range is said beside the field, and nothing is sent", async () => {
  const calls = stubEngine(engine);
  await show();
  await changeLimit(/Change Most allows per agent and kind/, "61");
  expect(calls).toEqual([]);
  expect(screen.getByRole("spinbutton").getAttribute("aria-invalid")).toBe("true");
  expect(screen.getAllByText("From 1 to 60.").length).toBeGreaterThan(0);
});

// NEVER HIDDEN: a person a rule escalates to reads the limits, and sees each
// pencil disabled with the reason.
test("a reader who may not change the document sees each limit's pencil held with the reason", async () => {
  await show(READER_VIEWER);
  const pencil = screen.getByRole("button", { name: /Change Most commands allowed per server/ });
  expect(
    pencil.getAttribute("aria-disabled") ?? String((pencil as HTMLButtonElement).disabled),
  ).toBe("true");
  const described = (pencil.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toContain(CONFIG_WRITE_REASONS.no_grant);
});

/** Merges in rule 2 alone, so deploys are free for a new rule. */
const MERGE = { ...RELEASE, kinds: ["pr_merge"] };
const MERGE_DOC = { name: "Acme", delegated_decisions: { enabled: true, rules: [SSH, MERGE] } };

/** Shows the page over MERGE_DOC, whose saves `respond` answers. */
async function showMerge(respond: (call: EngineCall) => Response = () => savedAnswer()) {
  const calls = stubEngine((call) =>
    call.method === "GET" ? engineAnswer(MERGE_DOC, 200, { ETag: '"r1"' }) : respond(call),
  );
  const answer = {
    ...ANSWER,
    rules: [ANSWER.rules[0]!, { ...ANSWER.rules[1]!, kinds: ["pr_merge"] }],
  } as DelegatedDecisionsAnswer;
  mountSettings(
    <DelegatedDecisions />,
    { delegated_decisions: answer, ...GITHUB },
    {
      hash: "#/settings/delegated-decisions",
      org: ORG,
    },
  );
  await act(afterMicrotasks);
  return calls;
}

/** Opens the sheet for Add a rule, filled from the document. */
async function openAdd() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Add rule/ }));
  });
  await act(afterMicrotasks);
  return screen.getByRole("dialog", { name: "Add rule" });
}

// A KIND IN ONE RULE ONLY: one another rule decides is offered disabled with
// that rule's number, never a choice the engine would refuse.
test("a kind another rule decides is held with that rule's number", async () => {
  await showMerge();
  const sheet = await openAdd();
  const merges = within(sheet).getByRole("checkbox", { name: /Merges/ }) as HTMLInputElement;
  expect(merges.disabled).toBe(true);
  expect(within(sheet).getByText("Rule 2 already decides these.")).toBeTruthy();
  expect(within(sheet).getByText("Rule 1 already decides these.")).toBeTruthy();
  const deploys = within(sheet).getByRole("checkbox", { name: /Deploys/ }) as HTMLInputElement;
  expect(deploys.disabled).toBe(false);
  expect(
    within(sheet).getByText("An agent deploys merged changes to an environment."),
  ).toBeTruthy();
});

// WHAT EVERY RULE NEEDS is asked for under its field, and nothing is sent.
test("a rule without an agent or a person is held under those fields", async () => {
  const calls = await showMerge();
  const sheet = await openAdd();
  fireEvent.click(within(sheet).getByRole("checkbox", { name: /Deploys/ }));
  await act(async () => {
    fireEvent.submit(sheet.querySelector("form")!);
  });
  expect(within(sheet).getByText("Pick the agent that decides.")).toBeTruthy();
  expect(within(sheet).getByText("Pick at least one person.")).toBeTruthy();
  expect(calls.filter((c) => c.method !== "GET")).toEqual([]);
});

// EVERY SEAT WITH ITS FACE: each option and each chosen chip draws the seat's
// avatar, its name and its handle.
test("the seat pickers draw each seat's face, name and handle", async () => {
  stubEngine(engine);
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 2" }));
  });
  await act(afterMicrotasks);
  const sheet = screen.getByRole("dialog", { name: "Edit rule 2" });
  const chosen = within(sheet).getByRole("list", { name: "Chosen: Agent" });
  expect(chosen.textContent).toContain("Release Reviewer @release-reviewer");
  expect(chosen.querySelector(".crewlet-avatar")).not.toBeNull();
  const field = within(sheet).getByRole("combobox", { name: "Agent" });
  fireEvent.keyDown(field, { key: "ArrowDown" });
  const option = within(sheet).getByRole("option", { name: /Security Engineer/ });
  expect(option.textContent).toContain("@security-engineer");
  expect(option.querySelector(".crewlet-avatar")).not.toBeNull();
  // ONLY AGENTS DECIDE, and only people are escalated to.
  expect(within(sheet).queryByRole("option", { name: /Ops Lead/ })).toBeNull();
});

// PICKED, NOT TYPED: the company's GitHub repositories, then the branch (the
// repository's default to start), then the checks seen on that branch.
test("a required-checks entry picks the repository, its branch and its checks", async () => {
  const calls = stubEngine(engine);
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 2" }));
  });
  await act(afterMicrotasks);
  const sheet = screen.getByRole("dialog", { name: "Edit rule 2" });
  fireEvent.click(within(sheet).getByRole("button", { name: "Add a repository" }));
  const repo = within(sheet).getByRole("combobox", { name: "Repository 2" });
  fireEvent.focus(repo);
  fireEvent.change(repo, { target: { value: "web" } });
  await act(afterMicrotasks);
  // ARCHIVED REPOSITORIES are not offered; the typed text narrows the rest.
  expect(within(sheet).queryByRole("option", { name: "acme/old" })).toBeNull();
  expect(within(sheet).queryByRole("option", { name: "acme/api" })).toBeNull();
  fireEvent.mouseDown(within(sheet).getByRole("option", { name: "acme/web" }));
  await act(afterMicrotasks);
  expect(
    (within(sheet).getByRole("combobox", { name: "Branch 2" }) as HTMLInputElement).value,
  ).toBe("trunk");
  const checks = within(sheet).getByRole("combobox", { name: "Checks 2" });
  fireEvent.keyDown(checks, { key: "ArrowDown" });
  fireEvent.mouseDown(within(sheet).getByRole("option", { name: "e2e" }));
  await act(async () => {
    fireEvent.submit(sheet.querySelector("form")!);
  });
  await act(afterMicrotasks);
  const [save] = writes(calls);
  expect(save!.body).toEqual({
    delegated_decisions: {
      rules: [
        SSH,
        {
          ...RELEASE,
          required_checks: [
            ...RELEASE.required_checks,
            { repo: "acme/web", branch: "trunk", checks: ["e2e"] },
          ],
        },
      ],
    },
    _summary: "Delegated decisions: change rule 2 (Merges and Deploys)",
  });
});

// A kind is in one rule only: with every kind taken, Add a rule says so rather than open.
test("with every kind in a rule, Add a rule is held with the reason", async () => {
  stubEngine(engine);
  const deploy = { ...ANSWER.rules[0]!, rule: 3, kinds: ["deploy"] };
  const full = {
    ...ANSWER,
    rules: [ANSWER.rules[0]!, { ...ANSWER.rules[1]!, kinds: ["pr_merge"] }, deploy],
  } as DelegatedDecisionsAnswer;
  mountSettings(
    <DelegatedDecisions />,
    { delegated_decisions: full, ...GITHUB },
    {
      hash: "#/settings/delegated-decisions",
      org: ORG,
    },
  );
  await act(afterMicrotasks);
  expect(screen.getByRole("button", { name: /Add rule/ }).getAttribute("title")).toBe(
    "Every kind of request has a rule. Edit one instead.",
  );
});
