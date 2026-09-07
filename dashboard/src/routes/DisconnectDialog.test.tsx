/**
 * The one destructive dialog in the product, and what it must not infer.
 *
 * Removing what the engine registered for itself is never in question.
 * Removing the ACCOUNTS it created is a separate decision, because each is a
 * colleague at that third-party app with history attached — so the checkbox is off
 * until somebody ticks it, and the request says so explicitly rather than
 * omitting the field and letting a default on the far side decide.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { DisconnectDialog } from "./DisconnectDialog.tsx";

type Sent = { method: string; path: string; body: unknown };

function stubFetch(sent: Sent[], status = 202) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      return new Response(JSON.stringify({ key: "github", disconnecting: true }), { status });
    }),
  );
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

test("the accounts are kept unless the box is ticked", async () => {
  const sent: Sent[] = [];
  stubFetch(sent);
  render(<DisconnectDialog name="GitHub" kind="github" onClose={() => {}} onDone={() => {}} />);

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  await waitFor(() => expect(sent.length).toBe(1));

  expect(sent[0]!.method).toBe("DELETE");
  expect(sent[0]!.path).toBe("/setup/integrations/github");
  expect(sent[0]!.body).toEqual({ remove_seats: false, force: false });
});

test("ticking the box asks for the accounts too", async () => {
  const sent: Sent[] = [];
  stubFetch(sent);
  render(<DisconnectDialog name="GitHub" kind="github" onClose={() => {}} onDone={() => {}} />);

  fireEvent.click(screen.getByRole("checkbox"));
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  await waitFor(() => expect(sent.length).toBe(1));
  expect(sent[0]!.body).toEqual({ remove_seats: true, force: false });
});

// FORCE IS NOT OFFERED UP FRONT. It leaves the third-party app holding things nobody
// will remove, so it appears only once a teardown has actually failed —
// otherwise it is just the easier button beside the correct one.
test("forcing is offered only after a failure", async () => {
  const sent: Sent[] = [];
  stubFetch(sent, 503);
  render(<DisconnectDialog name="GitHub" kind="github" onClose={() => {}} onDone={() => {}} />);

  expect(screen.queryByRole("button", { name: /anyway/i })).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  const force = await screen.findByRole("button", { name: /anyway/i });

  fireEvent.click(force);
  await waitFor(() => expect(sent.length).toBe(2));
  expect(sent[1]!.body).toEqual({ remove_seats: false, force: true });
});
