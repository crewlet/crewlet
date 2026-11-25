/**
 * A consent product's OAuth client on the connect form: the engine's redirect
 * URI to copy, the state of what step 1 connects first, and each optional
 * field marked once.
 */

import type { ReactElement } from "react";
import { cleanup, fireEvent, render as rtlRender, screen } from "@testing-library/react";
import { LayerHost } from "@crewlethq/ui";
import { afterEach, expect, test, vi } from "vitest";
import { SetupForm } from "./SetupForm.tsx";
import type { SetupRequirement, SetupStep, SetupToolState } from "~/protocol/index.ts";

const CALLBACK = "https://crewlet.example.com/webhooks/mcp-oauth";

function req(over: Partial<SetupRequirement>): SetupRequirement {
  return {
    field: "f",
    label: "F",
    kind: "text",
    config_path: "",
    required: false,
    present: false,
    resolved: null,
    ...over,
  };
}

/** Google Analytics as the engine answers it, the client optional. */
function analytics(
  over: { url?: string; connected?: boolean; required?: boolean } = {},
): SetupToolState {
  const required = over.required ?? false;
  const prepare: SetupStep[] = [
    {
      text: [{ text: "Connect Google Cloud first; no project needs preparing." }],
      needs: "google_cloud",
      connected: over.connected ?? false,
    },
    { text: [{ text: "Add the company reader below to each property." }] },
  ];
  return {
    key: "google_analytics",
    configured: false,
    enabled: false,
    satisfied: true,
    form_complete: true,
    prepare,
    requirements: [
      req({
        field: "redirect_uri",
        label: "Authorized redirect URI",
        kind: "redirect",
        required,
        redirect: { url: over.url ?? CALLBACK, key: "api.public.external_url" },
        hint: [
          { text: "Make a " },
          { ui: ["Web application"] },
          { text: " client with this redirect URI." },
        ],
        more: [{ text: "Leave " }, { ui: ["Authorized JavaScript origins"] }, { text: " empty." }],
      }),
      req({
        field: "client_id",
        label: "OAuth client ID",
        kind: "id",
        required,
        config_path: "integrations.google_analytics.oauth.client_id",
      }),
      req({
        field: "client_secret",
        label: "OAuth client secret",
        kind: "secret",
        required,
        config_path: "integrations.google_analytics.oauth.client_secret",
      }),
      req({
        field: "google_sign_in",
        label: required ? "Sign in with Google" : "Admin sign-in",
        kind: "consent",
        required,
      }),
    ],
  };
}

function render(ui: ReactElement): ReturnType<typeof rtlRender> {
  return rtlRender(ui, { wrapper: LayerHost });
}

function form(tool: SetupToolState) {
  return render(
    <SetupForm
      sections={[{ name: "Google Analytics", tool }]}
      title="Google Analytics"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
}

/** The steps, opened from the narrow pane's sheet where they are not beside the form. */
function openSteps(): void {
  const sheet = screen.queryByRole("button", { name: "Setup steps" });
  if (sheet) fireEvent.click(sheet);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("the redirect URI is the engine's, copied whole, and says what it is built on", () => {
  form(analytics());
  expect(screen.getByText(CALLBACK)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Copy Authorized redirect URI" })).toBeTruthy();
  expect(screen.getByText("api.public.external_url")).toBeTruthy();
  expect(screen.getByText(/update it in the client before the next sign-in/)).toBeTruthy();
  // THE WHOLE STEP SHOWN, never behind a disclosure: origins are part of making the client.
  expect(screen.getByText("Authorized JavaScript origins")).toBeTruthy();
});

test("an engine with no public address says so instead of an address", () => {
  form(analytics({ url: "" }));
  expect(screen.queryByRole("button", { name: "Copy Authorized redirect URI" })).toBeNull();
  expect(screen.getByText(/This engine has no public address/)).toBeTruthy();
  expect(screen.getByText("api.public.external_url")).toBeTruthy();
});

test("an optional field is marked optional once, a required one not at all", () => {
  const { unmount } = form(analytics());
  for (const label of ["OAuth client ID", "OAuth client secret", "Admin sign-in"]) {
    const marks = document.body.textContent?.split(`${label} (optional)`).length ?? 0;
    expect(marks, label).toBe(2);
  }
  expect(document.body.textContent).not.toContain("(optional) (optional)");
  expect(document.body.textContent).not.toContain("Authorized redirect URI (optional)");
  unmount();
  form(analytics({ required: true }));
  expect(document.body.textContent).not.toContain("(optional)");
});

test("a step's Google Cloud not connected is said above the fields and beside the step", () => {
  form(analytics({ connected: false }));
  expect(screen.getByText("Connect Google Cloud first")).toBeTruthy();
  openSteps();
  expect(screen.getByText("Google Cloud not connected")).toBeTruthy();
  const links = screen.getAllByRole("link", { name: "Open Google Cloud" });
  expect(links.length).toBe(2);
  for (const link of links)
    expect(link.getAttribute("href")).toBe("#/company/integrations/google_cloud");
});

test("a connected Google Cloud is said beside the step and nowhere above the fields", () => {
  form(analytics({ connected: true }));
  expect(screen.queryByText("Connect Google Cloud first")).toBeNull();
  openSteps();
  expect(screen.getByText("Google Cloud connected")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open Google Cloud" })).toBeNull();
});
