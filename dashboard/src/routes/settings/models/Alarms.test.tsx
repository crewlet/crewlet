/**
 * The model alarms are the engine's, grouped as a person fixes them, said in
 * plain words, and each carries the one gesture that fixes it.
 *
 * The invariants: models reading one secret, or sharing one plan, are ONE
 * alarm; the title names the cause and the vendor's sentence never leads (QA
 * M11); seats and jobs are said by their names, never a handle (QA M8); the
 * worst alarm comes first; every fix opens its flow through `onFlow` at the
 * right step with nothing chosen for the reader (the deep-link walk); and a
 * limit holding seats offers to move them, with the seats added (QA M10).
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import type { ModelAlarm } from "~/protocol/models.ts";
import {
  EVERY_JOB,
  MODELS_NOW,
  at,
  modelEntry,
  modelsData,
  runs,
  signIn,
} from "~/routes/settings/models/fixtures.ts";
import { Alarms } from "./Alarms.tsx";
import type { ModelsData } from "./data.ts";
import { NEW_CREDENTIAL, WIZARD_STEPS } from "./parts.tsx";

const VENDOR = "401 invalid x-api-key: the key was revoked";

/** Two models on one API key, refused; a third and fourth on one plan, sitting out with a seat waiting; a token about to expire; a Codex model nothing signs in. */
function company(): ModelsData {
  const key = signIn({ route: "api-key", mode: "api-key", variable: "ANTHROPIC_API_KEY" });
  const sessionOut = {
    until: at(120),
    resets_at: null,
    window: "session" as const,
    said: "You've hit your session limit",
    since: at(-30),
    nodes: ["node-1"],
  };
  const failure = { kind: "auth", at: at(-10), said: VENDOR, node: "node-1" };
  return modelsData({
    entries: [
      modelEntry({
        key: "sonnet",
        health: "rejected",
        plan: "key-1",
        sign_in: key,
        last_failure: failure,
        used_by: [...runs("pm", "executor"), ...runs("qa", "review", "subagent")],
      }),
      modelEntry({
        key: "sonnet-batch",
        health: "rejected",
        plan: "key-1",
        sign_in: key,
        last_failure: failure,
      }),
      modelEntry({
        key: "default",
        health: "sitting_out",
        plan: "plan-a",
        sitting_out: sessionOut,
        waiting: { seats: ["qa"], since: at(-20) },
        used_by: runs("backend-engineer", ...EVERY_JOB),
      }),
      modelEntry({ key: "aux", health: "sitting_out", plan: "plan-a", sitting_out: sessionOut }),
      modelEntry({
        key: "opus",
        health: "expiring",
        plan: "plan-b",
        sign_in: signIn({ variable: "OPUS_TOKEN", expires_at: at(60 * 24 * 5) }),
        used_by: runs("qa", "executor"),
      }),
      modelEntry({
        key: "codex-review",
        agent: "codex",
        model: "gpt-6.1",
        health: "no_sign_in",
        sign_in: signIn({ route: "none", variable: null, source: "none", resolves: false }),
        used_by: runs("pm", "auxiliary", "judge"),
      }),
    ],
    alarms: ALARMS,
  });
}

const ALARMS: ModelAlarm[] = [
  {
    kind: "llm_credential_expires_soon",
    detail: "opus's OPUS_TOKEN expires on 3 Nov 2026 (5 days)",
    remedy: "Rotate the credential now.",
    entries: [{ key: "opus", detail: "opus's OPUS_TOKEN expires on 3 Nov 2026 (5 days)" }],
  },
  {
    kind: "llm_sign_in_refused",
    detail: `codex-review: nothing signs it in`,
    remedy: "Give the model a credential that signs in.",
    entries: [
      { key: "codex-review", detail: "nothing signs codex-review in on this node" },
      { key: "sonnet", detail: `sonnet's sign-in was refused at ${at(-10)}: ${VENDOR}` },
      {
        key: "sonnet-batch",
        detail: `sonnet-batch's sign-in was refused at ${at(-10)}: ${VENDOR}`,
      },
    ],
  },
  {
    kind: "llm_limit_waiting",
    detail: "a seat has waited 20 minutes parked on default's limit",
    remedy: "Seats are not working.",
    entries: [
      { key: "aux", detail: "aux sits out its session limit" },
      { key: "default", detail: "a seat has waited 20 minutes parked on default's limit" },
    ],
  },
];

function mount(data: ModelsData = company()) {
  const onFlow = vi.fn();
  render(<Alarms data={data} onFlow={onFlow} />, { wrapper: LayerHost });
  return onFlow;
}

/** Every alarm on the page: its title, its whole text, and its fixing button. */
function callouts() {
  return [...document.querySelectorAll<HTMLElement>(".crewlet-callout")].map((el) => ({
    el,
    title: el.querySelector(".crewlet-callout__title")?.textContent ?? "",
    lead: [...el.querySelectorAll(".models-alarm > span")].map((s) => s.textContent).join(" "),
    button: el.querySelector<HTMLButtonElement>(".crewlet-callout__action button"),
  }));
}

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("models that read one secret are one alarm, titled by its cause, the vendor's words never leading", () => {
  mount();
  const refused = callouts().find((c) => c.title.includes("API key"));
  expect(refused?.title).toBe("The API key for the models sonnet and sonnet-batch was refused");
  expect(refused?.lead).toContain("They share ANTHROPIC_API_KEY.");
  // WHO IT STOPS, by the seats' names and the jobs' names (QA M8).
  expect(refused?.lead).toMatch(/coding for Product Manager/);
  expect(refused?.lead).toMatch(/for Quality Lead/);
  expect(refused?.lead).not.toMatch(/\bpm\b|\bqa\b|executor/);
  // THE VENDOR'S SENTENCE is kept whole, behind the disclosure, never in the lead (QA M11).
  expect(refused?.title).not.toContain("401");
  expect(refused?.lead).not.toContain("401");
  fireEvent.click(screen.getAllByRole("button", { name: "Details" })[0]!);
  expect(screen.getAllByText(new RegExp(VENDOR)).length).toBeGreaterThan(0);
});

test("models sharing one plan pause together as one alarm, naming who waits", () => {
  mount();
  const limit = callouts().filter((c) => c.title.includes("limit"));
  expect(limit).toHaveLength(1);
  expect(limit[0]!.title).toBe(
    "Seats are waiting for the Claude Code plan's session limit to reset",
  );
  expect(limit[0]!.lead).toMatch(/Quality Lead is waiting, with no fallback model/);
  expect(limit[0]!.lead).toContain("The models aux and default sit out until");
  expect(limit[0]!.lead).toContain("They share one plan, so they pause together.");
});

test("models on different plans are separate alarms", () => {
  const data = company();
  const aux = data.models.find((m) => m.key === "aux")!;
  aux.admin!.plan = "plan-other";
  mount(data);
  expect(callouts().filter((c) => c.title.includes("session limit"))).toHaveLength(2);
});

test("the alarms that stop work come before the ones that warn", () => {
  mount();
  const tones = callouts().map((c) =>
    c.el.classList.contains("crewlet-callout--danger") ? "danger" : "warning",
  );
  expect(tones.indexOf("warning")).toBeGreaterThan(tones.lastIndexOf("danger"));
  expect(tones.filter((t) => t === "danger")).toHaveLength(2);
});

test("a credential's expiry says when, and its fix is the wizard's Method step with the secret open", () => {
  const onFlow = mount();
  const expiry = callouts().find((c) => c.title.startsWith("The subscription token"))!;
  expect(expiry.title).toBe("The subscription token for the model opus expires in 5 days");
  expect(expiry.lead).toMatch(/Renew it before then/);
  expect(expiry.button?.textContent).toBe("Renew token");
  fireEvent.click(expiry.button!);
  expect(onFlow).toHaveBeenCalledWith("edit", "opus", NEW_CREDENTIAL);
});

/**
 * THE DEEP-LINK WALK: every alarm's one button opens its flow on its model,
 * at the step it is about, and the address carries only where to open (the
 * step, the field, the seat, the job), never a value chosen for the reader
 * (QA M5's other half).
 */
test("every alarm's fix opens its flow at the right step, and arrives neutral", () => {
  const onFlow = mount();
  const expected: Record<string, unknown[]> = {
    "The API key for the models sonnet and sonnet-batch was refused": [
      "edit",
      "sonnet",
      NEW_CREDENTIAL,
    ],
    "The model codex-review is not signed in": ["device", "codex-review", undefined],
    "Seats are waiting for the Claude Code plan's session limit to reset": [
      "retry",
      "aux",
      undefined,
    ],
    "The subscription token for the model opus expires in 5 days": ["edit", "opus", NEW_CREDENTIAL],
  };
  const all = callouts();
  expect(all.map((c) => c.title).sort()).toEqual(Object.keys(expected).sort());
  for (const callout of all) {
    onFlow.mockClear();
    // ONE FIX per alarm; a limit holding seats adds the menu that moves them.
    expect(callout.el.querySelectorAll(".crewlet-callout__action button")).toHaveLength(
      callout.title.includes("limit") ? 2 : 1,
    );
    fireEvent.click(callout.button!);
    expect(onFlow).toHaveBeenCalledTimes(1);
    expect(onFlow.mock.calls[0]).toEqual(expected[callout.title]);
    const at = onFlow.mock.calls[0]![2] as Record<string, unknown> | undefined;
    if (at) {
      expect(Object.keys(at).every((k) => ["step", "focus", "add"].includes(k))).toBe(true);
      expect(WIZARD_STEPS).toContain(at.step);
    }
  }
});

/**
 * QA M10: a limit's seats are not left waiting for a reset. The second
 * gesture moves them, every seat's category on the limited models, to a model
 * that answers now (never one on the same plan, never one that cannot do the
 * coding it would take), or to a model being added.
 */
test("a limit offers to move the seats it holds to a model that answers, with them added", () => {
  const data = company();
  // A READY MODEL whose CLI runs no coding cannot take coding, which these seats need.
  data.models.push(
    ...modelsData({
      entries: [
        modelEntry({ key: "codex-ready", agent: "codex", model: "gpt-6.1", health: "ready" }),
        modelEntry({ key: "haiku", model: "claude-haiku-4-5", health: "ready" }),
        // ON THE LIMITED PLAN: ready by its own last answer, held by the plan's limit all the same.
        modelEntry({ key: "plan-mate", plan: "plan-a", health: "ready" }),
      ],
    }).models,
  );
  // A LIMITED MODEL that last answered is still the one the seats are moving off.
  data.models.find((m) => m.key === "aux")!.entry.health = "ready";
  const onFlow = mount(data);
  const limit = callouts().find((c) => c.title.includes("limit"))!;
  const trigger = within(limit.el).getByRole("button", {
    name: "Use a different model for these seats",
  });
  // The fix comes first: Retry now is still the alarm's primary gesture.
  expect(limit.button?.textContent).toBe("Retry now");
  fireEvent.click(trigger);
  const offered = screen.getAllByRole("menuitem").map((i) => i.textContent ?? "");
  // opus is expiring but still answers; the refused, the unsigned, the limited, its plan's and the Codex model are not offered.
  expect(offered.map((t) => t.split("claude")[0])).toEqual(["opus", "haiku", "Add a model"]);
  const moved = [
    { job: "coding", seat: "backend-engineer" },
    { job: "reviews", seat: "backend-engineer" },
    { job: "routines", seat: "backend-engineer" },
  ];
  fireEvent.click(screen.getAllByRole("menuitem").find((i) => i.textContent?.startsWith("haiku"))!);
  expect(onFlow).toHaveBeenCalledWith("edit", "haiku", { step: "assign", add: moved });
  onFlow.mockClear();
  fireEvent.click(trigger);
  fireEvent.click(screen.getByRole("menuitem", { name: "Add a model" }));
  expect(onFlow).toHaveBeenCalledWith("add", undefined, { step: "cli", add: moved });
});

test("a model's own limit holds that model alone, so its plan's other models are offered", () => {
  const data = company();
  for (const key of ["default", "aux"]) {
    const m = data.models.find((x) => x.key === key)!;
    m.entry.sitting_out = { ...m.entry.sitting_out!, window: "model" };
  }
  // A LIMITED MODEL is never offered back, whatever its last reading says.
  data.models.find((m) => m.key === "aux")!.entry.health = "ready";
  data.models.push(
    ...modelsData({ entries: [modelEntry({ key: "plan-mate", plan: "plan-a", health: "ready" })] })
      .models,
  );
  mount(data);
  const limit = callouts().find((c) => c.title.includes("limit"))!;
  fireEvent.click(
    within(limit.el).getByRole("button", { name: "Use a different model for these seats" }),
  );
  expect(screen.getAllByRole("menuitem").map((i) => i.textContent?.split("claude")[0])).toEqual([
    "opus",
    "plan-mate",
    "Add a model",
  ]);
});

test("a limit that holds no seat offers no move", () => {
  const data = company();
  data.models.find((m) => m.key === "default")!.uses = data.models.find(
    (m) => m.key === "aux",
  )!.uses;
  mount(data);
  const limit = callouts().find((c) => c.title.includes("limit"))!;
  expect(
    within(limit.el).queryByRole("button", { name: "Use a different model for these seats" }),
  ).toBeNull();
});

test("an alarm whose engine names no model is drawn in the engine's words, with no gesture", () => {
  mount(
    modelsData({
      entries: [],
      alarms: [{ kind: "llm_limit", detail: "a model sits out its plan limit", remedy: "Wait." }],
    }),
  );
  const [only] = callouts();
  expect(only?.title).toBe("a model sits out its plan limit");
  expect(only?.button).toBeNull();
});

test("a member's reading carries no alarm, and the page draws none", () => {
  const data = company();
  mount({ ...data, admin: false, alarms: [] });
  expect(callouts()).toHaveLength(0);
  expect(screen.queryByRole("region", { name: "Model alarms" })).toBeNull();
});
