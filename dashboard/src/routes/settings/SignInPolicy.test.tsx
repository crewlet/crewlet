/**
 * Settings › Sign-in policy: durations typed as one, a value past the node's
 * limit refused before sending, a weakening change confirmed, held values said.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { SignInPolicyAnswer } from "~/contract/signin.ts";
import { typeSetting } from "~/test/settingField.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  READER_VIEWER,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { heldWords, SignInPolicy } from "./SignInPolicy.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const DAY = 86_400;
type Row = SignInPolicyAnswer["settings"][number];

function row(key: string, value: number, def: number, min: number, max: number, op = false): Row {
  return {
    key,
    group: "sign_in",
    unit: key.endsWith("_seconds") ? "seconds" : "count",
    value,
    default: def,
    off: false,
    default_off: false,
    settable: true,
    range: { min, max, ...(op ? { operator_max: true } : {}) },
  };
}

const ANSWER: SignInPolicyAnswer = {
  node: "node-a",
  policy: {
    second_factor: "required",
    min_password_length: 12,
    session_absolute_seconds: 7 * DAY,
    session_idle_seconds: 12 * 3600,
    step_up_seconds: 3600,
  },
  limits: {
    second_factor: "required",
    min_password_length: 12,
    max_session_lifetime_seconds: 14 * DAY,
    max_idle_timeout_seconds: 30 * DAY,
    max_step_up_seconds: DAY,
  },
  settings: [
    row("sign_in.min_password_length", 12, 12, 12, 256),
    row("sign_in.session_lifetime_seconds", 7 * DAY, 7 * DAY, 3600, 14 * DAY, true),
    row("sign_in.idle_timeout_seconds", 12 * 3600, 12 * 3600, 900, 30 * DAY),
    row("sign_in.step_up_seconds", 3600, 3600, 300, DAY),
  ],
  second_factor_setting: {
    key: "sign_in.require_second_factor",
    group: "sign_in",
    kind: "switch",
    value: true,
    default: true,
    set: false,
    off: false,
    has_off: false,
  },
};

async function show(answer = ANSWER, viewer?: Record<string, unknown>) {
  mountSettings(
    <SignInPolicy />,
    { sign_in_policy: answer },
    {
      hash: "#/settings/sign-in",
      ...(viewer ? { viewer } : {}),
    },
  );
  await act(afterMicrotasks);
}

async function change(name: RegExp, value: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
  const field = await typeSetting(value);
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
}

const DOC = { name: "Acme" };

test("each setting is drawn with its value, its key and the node's limits", async () => {
  await show();
  const sessions = screen.getByRole("table", { name: "Sessions" });
  const lifetime = within(sessions).getByRole("row", { name: /Longest a session lives/ });
  expect(within(lifetime).getByText("7 days")).toBeTruthy();
  expect(within(lifetime).getByText("Default")).toBeTruthy();
  const signing = screen.getByRole("table", { name: "Signing in" });
  expect(within(signing).getByText("12 characters")).toBeTruthy();
  expect(within(signing).getByText("Required at every sign-in")).toBeTruthy();
  const limits = screen.getByRole("table", { name: "This node’s limits" });
  expect(within(limits).getByText("14 days")).toBeTruthy();
  expect(within(limits).getByText("Always required")).toBeTruthy();
});

// A DURATION IS TYPED AS ONE: 3d is three days in seconds, in the one key's
// patch, and the revision says it in words.
test("a duration typed in days is saved as seconds in its one key", async () => {
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show();
  await change(/Change Longest a session lives/, "3d");
  const save = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(save?.body).toEqual({
    sign_in: { session_lifetime_seconds: 3 * DAY },
    _summary:
      "Sign-in policy: Longest a session lives set to 3 days (sign_in.session_lifetime_seconds)",
  });
});

// PAST THE OPERATOR'S LIMIT the field says so and nothing is sent.
test("a value past this node's limit is refused before anything is sent", async () => {
  const calls = stubEngine(() => engineAnswer(DOC, 200, { ETag: '"r1"' }));
  await show();
  await change(/Change Longest a session lives/, "30d");
  expect(calls).toEqual([]);
  expect(
    screen.getAllByText("From 1 hour to 14 days. This node’s operator allows no more.").length,
  ).toBeGreaterThan(0);
});

// A WEAKENING CHANGE IS CONFIRMED: the engine's warning stops the save, and
// only the second press stores it.
test("a weakening change asks for a confirmation saying what it allows", async () => {
  const allows =
    "This lets a session last 10 days instead of 7 days, so a stolen session works for longer.";
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) {
      const empty = Object.keys(call.body as object).every((k) => k.startsWith("_"));
      return validAnswer(
        empty
          ? []
          : [{ kind: "advisory", path: "sign_in.session_lifetime_seconds", message: allows }],
      );
    }
    return savedAnswer();
  });
  await show();
  await change(/Change Longest a session lives/, "10d");
  expect(screen.getByText(allows)).toBeTruthy();
  expect(calls.filter((c) => c.method === "PATCH" && !c.path.includes("dry_run"))).toHaveLength(0);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
  });
  await act(afterMicrotasks);
  expect(calls.filter((c) => c.method === "PATCH" && !c.path.includes("dry_run"))).toHaveLength(1);
});

test("a value the node holds lower is said beside the policy", async () => {
  const held: SignInPolicyAnswer = {
    ...ANSWER,
    settings: ANSWER.settings.map((r) =>
      r.key === "sign_in.session_lifetime_seconds" ? { ...r, value: 30 * DAY } : r,
    ),
    second_factor_setting: { ...ANSWER.second_factor_setting, value: false, set: true },
    policy: { ...ANSWER.policy, session_absolute_seconds: 14 * DAY },
  };
  expect(heldWords(held)).toEqual(["two-step verification to required", "sessions to 14 days"]);
  expect(heldWords(ANSWER)).toEqual([]);
  await show(held);
  expect(
    screen.getByText(/holds two-step verification to required, sessions to 14 days/).textContent,
  ).toContain("the limit its operator set");
});

test("a reader who may not change the configuration sees the pencils held, with why", async () => {
  await show(ANSWER, READER_VIEWER);
  const pencil = screen.getByRole("button", { name: "Change Longest a session lives" });
  expect(pencil.getAttribute("aria-disabled") ?? pencil.getAttribute("disabled")).not.toBeNull();
});
