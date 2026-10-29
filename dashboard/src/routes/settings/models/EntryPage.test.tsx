/**
 * A model's own page: who runs on it by category with backups said apart, one
 * empty state, the engine's condition with the action that fixes it, a host
 * sign-in that stays on the page, and a member's reading with no action or
 * credential in it.
 */

import { waitFor, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import type { ModelEntry, ModelUse } from "~/protocol/models.ts";
import { EntryPage, whoRuns } from "./EntryPage.tsx";
import { apiKeyEntry, modelsData, seat } from "~/routes/settings/models/fixtures.ts";

vi.mock("./api.ts", () => ({ checkHostLogin: vi.fn() }));

afterEach(cleanup);

const SEATS = [seat("backend-engineer", "Backend Engineer"), seat("qa", "QA"), seat("pm", "PM")];

const USED_BY: ModelUse[] = [
  { seat: "backend-engineer", job: "executor", position: 0 },
  { seat: "backend-engineer", job: "review", position: 0 },
  { seat: "backend-engineer", job: "subagent", position: 0 },
  { seat: "qa", job: "review", position: 0 },
  { seat: "pm", job: "executor", position: 1 },
  { assistant: "chat", position: 0 },
];

function page(entry: Partial<ModelEntry> & Pick<ModelEntry, "key">, admin = true) {
  const data = modelsData({
    entries: [apiKeyEntry(entry), apiKeyEntry({ key: "other" })],
    admin,
    seats: SEATS,
  });
  const onFlow = vi.fn();
  render(
    <Router>
      <EntryPage model={data.models[0]!} data={data} onFlow={onFlow} />
    </Router>,
  );
  return onFlow;
}

test("who runs on it is grouped by category, a partial category named, a backup apart", () => {
  const who = whoRuns(USED_BY);
  expect(who.categories.map((c) => c.category)).toEqual(["coding", "reviews"]);
  const coding = who.categories[0]!;
  expect(coding.runs.map((s) => s.seat)).toEqual(["backend-engineer"]);
  expect(coding.backups.map((s) => s.seat)).toEqual(["pm"]);
  const reviews = who.categories[1]!;
  expect(reviews.runs).toEqual([
    { seat: "backend-engineer", jobs: ["review", "subagent"], whole: true },
    { seat: "qa", jobs: ["review"], whole: false },
  ]);
  expect(who.assistant).toBe("runs");
  expect(who.seats).toBe(3);
});

test("the card names seats by their names and says the Dashboard Assistant once", () => {
  page({ key: "sonnet", used_by: USED_BY });
  const list = screen.getByRole("list", { name: "Who runs on sonnet" });
  // Once under each category it runs: coding, and reviewing and delegations.
  const links = within(list).getAllByRole("link", { name: /Backend Engineer/ });
  expect(links).toHaveLength(2);
  expect(links[0]!.getAttribute("href")).toContain("backend-engineer");
  expect(list.textContent).toContain("(reviewing only)");
  expect(list.textContent).toContain("Backup for PM.");
  expect(list.textContent).toContain("The Dashboard Assistant runs on it.");
  expect(list.textContent).not.toMatch(/backend-engineer|llm_review/);
});

// QA M12: one empty state, no headed table, the assistant on a line of its own.
test("a model no seat uses says so once, with the assistant beside it", () => {
  page({ key: "assistant", used_by: [{ assistant: "chat", position: 0 }] });
  expect(screen.queryByRole("table")).toBeNull();
  expect(screen.getByText(/No seat uses/).textContent).toBe("No seat uses assistant.");
  expect(screen.getByText("The Dashboard Assistant runs on it.")).toBeDefined();
});

test("a sitting-out model's Retry now opens the retry flow", () => {
  const onFlow = page({
    key: "default",
    health: "sitting_out",
    sitting_out: {
      until: "2026-10-29T14:00:00Z",
      resets_at: null,
      window: "session",
      said: "You've hit your session limit",
      since: "2026-10-29T09:00:00Z",
      nodes: ["node-1"],
    },
  });
  expect(screen.getByText("Claude Code session limit")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Retry now…" }));
  expect(onFlow).toHaveBeenCalledWith("retry", "default", undefined);
});

// QA M4: a failure older than the credential stored after it is not the news.
test("a refusal from before the last stored credential is not shown as the cause", () => {
  page({
    key: "sonnet",
    health: "rejected",
    last_failure: {
      kind: "auth",
      at: "2026-09-01T00:00:00Z",
      said: "401 invalid x-api-key",
      node: "node-1",
    },
  });
  expect(screen.getAllByText("The API key was refused")).not.toHaveLength(0);
  expect(screen.queryByText(/401 invalid x-api-key/)).toBeNull();
});

// QA Minor 12: nothing is edited, so a host sign-in stays on the page.
test("a host sign-in moves to the sign-in card rather than opening the wizard", () => {
  const onFlow = page({
    key: "default",
    health: "no_sign_in",
    sign_in: {
      ...apiKeyEntry({ key: "default" }).sign_in,
      route: "credentials",
      mode: "subscription",
      variable: null,
      host_logins: [
        {
          node: "node-1",
          state: "signed_out",
          method: null,
          account: null,
          checked_at: "2026-10-29T09:00:00Z",
          command: "crewlet llm login default",
        },
      ],
    },
  });
  const card = document.getElementById("models-sign-in")!;
  expect(within(card).getByText("crewlet llm login default")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Sign in on the host" }));
  expect(onFlow).not.toHaveBeenCalled();
  expect(document.activeElement).toBe(card);
});

// QA Minor 12, from elsewhere: the list's or an alarm's host sign-in arrives
// as `?focus=host-login` and lands on the sign-in card, once.
test("a host sign-in asked for elsewhere arrives on the sign-in card", async () => {
  location.hash = "#/settings/models/default?focus=host-login";
  page({
    key: "default",
    health: "no_sign_in",
    sign_in: { ...apiKeyEntry({ key: "default" }).sign_in, route: "credentials", variable: null },
  });
  const card = document.getElementById("models-sign-in")!;
  await waitFor(() => expect(document.activeElement).toBe(card));
  await waitFor(() => expect(location.hash).not.toContain("focus="));
  location.hash = "#/";
});

test("a member reads the condition with no action, no credential's name and no vendor sentence", () => {
  page(
    {
      key: "sonnet",
      health: "rejected",
      used_by: USED_BY,
      last_failure: {
        kind: "auth",
        at: "2026-10-29T09:00:00Z",
        said: "401 invalid x-api-key",
        node: "node-1",
      },
    },
    false,
  );
  expect(screen.queryAllByRole("button")).toHaveLength(0);
  expect(document.body.textContent).not.toMatch(/ANTHROPIC_API_KEY|401 invalid|Vendor said/);
  expect(screen.getByRole("list", { name: "Who runs on sonnet" })).toBeDefined();
});
