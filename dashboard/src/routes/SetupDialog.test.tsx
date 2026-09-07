/**
 * The setup dialog renders a form from data, and never renders a value.
 *
 * The invariant worth breaking a build over is the second one: no route the
 * engine serves returns a credential, so any value on this page would have to
 * have come from somewhere it should not have.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { SetupDialog, fieldsFor } from "./SetupDialog.tsx";
import type { SetupRequirement, SetupToolState } from "~/protocol/index.ts";

function req(over: Partial<SetupRequirement>): SetupRequirement {
  return {
    field: "f",
    label: "F",
    kind: "text",
    config_path: "integrations.datadog.f",
    required: true,
    present: false,
    resolved: false,
    ...over,
  };
}

const tool: SetupToolState = {
  key: "datadog",
  configured: false,
  enabled: false,
  satisfied: false,
  inbound_path: "/webhooks/datadog",
  public_url: "https://engine.example.com/webhooks/datadog",
  requirements: [
    req({
      field: "enabled",
      label: "Accept Datadog deliveries",
      kind: "toggle",
      config_path: "integrations.datadog.enabled",
    }),
    req({
      field: "webhook_token",
      label: "Shared token",
      kind: "secret",
      mintable: true,
      secret_name: "DATADOG_WEBHOOK_TOKEN",
      config_path: "integrations.datadog.webhook_token",
      blocks: "credential_missing",
    }),
    req({
      field: "route_to",
      label: "Fallback seat",
      kind: "handle",
      config_path: "integrations.datadog.route_to",
      blocks: "credential_missing",
      choices: [{ value: "sre-lead", label: "sre-lead" }],
    }),
    req({
      field: "handle_tag",
      label: "Owner tag key",
      required: false,
      config_path: "integrations.datadog.handle_tag",
    }),
  ],
};

function stubFetch(handler: (path: string, init?: RequestInit) => Response) {
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://engine.test").pathname;
    return Promise.resolve(handler(path, init));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return spy;
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.restoreAllMocks();
});

// EVERY WORD COMES FROM THE ENGINE. Nothing about any third-party app is in the
// component, which is what makes adding one a Go change and no screen work.
test("the form is rendered from the requirement list", () => {
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText("Fallback seat")).toBeDefined();
  expect(screen.getByText("Owner tag key")).toBeDefined();
  expect(screen.getByText(/Accept Datadog deliveries/)).toBeDefined();
});

// A MINTABLE SECRET HAS NO INPUT. Asking a person to invent a shared token is
// asking them to invent a password.
test("a mintable secret offers no input", () => {
  const { container } = render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText(/Crewlet will generate this/)).toBeDefined();
  // And no password box anywhere: the one secret on this form is minted.
  expect(container.querySelectorAll('input[type="password"]').length).toBe(0);
});

// NO VALUE IS EVER RENDERED. A stored secret shows the ${VAR} it points at,
// which is safe, and nothing else.
test("a stored secret shows its pointer, never a value", () => {
  const stored: SetupToolState = {
    ...tool,
    configured: true,
    requirements: tool.requirements.map((r) =>
      r.field === "webhook_token" ? { ...r, present: true, resolved: true } : r,
    ),
  };
  const { container } = render(
    <SetupDialog
      sections={[{ name: "Datadog", tool: stored }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText(/Stored as \$\{DATADOG_WEBHOOK_TOKEN\}/)).toBeDefined();
  expect(container.querySelectorAll('input[type="password"]').length).toBe(0);
});

// A FIX NARROWS TO THE FIELDS THAT CLEAR THE FINDING, which is what the
// blocks field on a requirement is for.
test("a finding narrows the form to what clears it", () => {
  const shown = fieldsFor(tool.requirements, "credential_missing");
  expect(shown.map((r) => r.field)).toEqual(["webhook_token", "route_to"]);
  // And a finding nothing clears falls back to the whole list rather than an
  // empty dialog.
  expect(fieldsFor(tool.requirements, "grant_short").length).toBe(tool.requirements.length);
});

// THE SUBMISSION SENDS A MINT REQUEST, NOT A VALUE, and sends nothing for a
// field the operator did not touch.
test("submitting asks for the mint and sends only what was filled in", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.click(screen.getByText("Save"));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  const init = spy.mock.calls[0]?.[1];
  const body = JSON.parse(String(init?.body)) as {
    values: Record<string, string>;
    generate: string[];
  };
  expect(body.generate).toEqual(["webhook_token"]);
  // The toggle defaults on, because connecting something and leaving it off
  // is not what the button says.
  expect(body.values.enabled).toBe("true");
  // Nothing was typed into these, so nothing is sent: a field sent back
  // unchanged is a field rewritten for no reason.
  expect(body.values.route_to).toBeUndefined();
  expect(body.values.handle_tag).toBeUndefined();
});

// A LITERAL REFUSAL LANDS ON THE FIELD, not in a banner nobody connects to an
// input.
test("a literal_in_config refusal is shown against its field", async () => {
  stubFetch(
    () =>
      new Response(
        JSON.stringify({
          error: "literal_in_config",
          path: "integrations.datadog.webhook_token",
        }),
        { status: 409 },
      ),
  );
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.click(screen.getByText("Save"));
  expect(await screen.findByRole("alert")).toBeDefined();
  expect(screen.getByText(/holds a value here rather than a/)).toBeDefined();
});
