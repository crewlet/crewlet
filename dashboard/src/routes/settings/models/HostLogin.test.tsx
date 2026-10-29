/**
 * The host login panel shows each node's login and the command that makes one
 * where it is missing, and a check updates the node that answered and asks
 * the page again.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ModelHostLogin } from "~/protocol/models.ts";
import { HostLogin } from "./HostLogin.tsx";

const checkHostLogin = vi.hoisted(() => vi.fn());
vi.mock("./api.ts", () => ({ checkHostLogin }));

afterEach(() => {
  cleanup();
  checkHostLogin.mockReset();
});

const LOGINS: ModelHostLogin[] = [
  {
    node: "node-1",
    state: "signed_in",
    method: "claude.ai",
    account: "ops@example.com",
    checked_at: "2026-10-29T09:00:00Z",
    command: "crewlet llm login default",
  },
  {
    node: "node-2",
    state: "signed_out",
    method: null,
    account: null,
    checked_at: "2026-10-29T09:00:00Z",
    command: "crewlet llm login default",
  },
];

test("each node says its login, and only a node without one shows the command", () => {
  render(<HostLogin modelKey="default" logins={LOGINS} onChecked={() => {}} />);
  expect(screen.getByText("Signed in on node-1 as ops@example.com")).toBeDefined();
  expect(screen.getByText("Not signed in on node-2")).toBeDefined();
  expect(screen.getAllByText("crewlet llm login default")).toHaveLength(1);
  expect(screen.getByRole("button", { name: /Copy the login command for node-2/ })).toBeDefined();
});

test("Check again replaces the answering node's line and asks the page again", async () => {
  checkHostLogin.mockResolvedValue({
    kind: "ok",
    status: 200,
    value: {
      ...LOGINS[1]!,
      state: "signed_in",
      account: "ops@example.com",
      detail: null,
      shares_login_with: ["default", "aux"],
    },
  });
  const onChecked = vi.fn();
  render(<HostLogin modelKey="default" logins={LOGINS} onChecked={onChecked} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
  });
  expect(checkHostLogin).toHaveBeenCalledWith("default");
  expect(screen.getByText("Signed in on node-2 as ops@example.com")).toBeDefined();
  expect(screen.queryByText("crewlet llm login default")).toBeNull();
  expect(screen.getByText(/One login also signs in the models aux/)).toBeDefined();
  expect(onChecked).toHaveBeenCalled();
});
