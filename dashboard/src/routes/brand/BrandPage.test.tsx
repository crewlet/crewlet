/**
 * Company › Brand as each reader meets it: a reader reads the adopted brand
 * and is handed no control; an administrator edits one card at a time, a
 * colour that would be refused holds Save with the engine's reason, and a
 * newer version is offered for review; with no brand adopted, an
 * administrator is offered the setup and everyone else is told what that
 * means.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { BrandPage } from "./BrandPage.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type BrandAnswer } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 404 })),
  );
  location.hash = "#/company/brand";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const ADOPTED: NonNullable<BrandAnswer["brand"]> = {
  kit: "01KIT",
  kit_version: 7,
  document: {
    v: 1,
    name: "FairSplit",
    tagline: "Split the trip, keep the friends",
    logos: { primary: "logos/primary.svg" },
    colors: { primary: "#0f7b5f", text: "#111418", light_ground: "#ffffff" },
    type: { heading: { kit: "geist" }, body: { stack: "system-sans" } },
    voice: {
      summary: "Plain, warm and exact.",
      write_like: ["Say what happened, then what to do."],
      avoid: ["Exclamation marks in errors."],
    },
  },
  css_sha: "c".repeat(64),
  files: [
    { slot: "css", content_type: "text/css", object: { hash: "c".repeat(64), size: 10 } },
    {
      slot: "logo.primary",
      content_type: "image/svg+xml",
      object: { hash: "a".repeat(64), size: 10 },
    },
  ],
  adopted_by: { name: "maya", kind: "human" },
  adopted_at: "2026-10-24T09:00:00Z",
};

/** Mount the page for a reader holding `grants`, over the company's `brand` answer. */
function mount(grants: string[], brand: BrandAnswer["brand"]) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(withDerived({ name: "FairSplit", roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve({
          login: "ana",
          owner: "ana",
          handle: "ana",
          kind: "human",
          grants,
          acts: [],
        })
      : what === "brand"
        ? Promise.resolve({ brand })
        : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <BrandPage />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

test("a reader reads the adopted brand card by card and is handed no control", async () => {
  mount([], ADOPTED);
  expect(await screen.findByRole("heading", { name: "Logos" })).toBeTruthy();
  for (const card of ["Colours", "Typography", "Voice and tone"]) {
    expect(screen.getByRole("heading", { name: card })).toBeTruthy();
  }
  const logosCard = screen.getByRole("region", { name: "Logos" });
  expect(within(logosCard).getAllByText("FairSplit").length).toBeGreaterThan(0);
  expect(screen.getByText("#0f7b5f")).toBeTruthy();
  expect(screen.getByText("Text on light ground")).toBeTruthy();
  expect(screen.getByRole("list", { name: "Write like this" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Edit / })).toBeNull();
  expect(screen.queryByRole("button", { name: /More brand actions/ })).toBeNull();
  // WHO ADOPTED IT AND WHICH VERSION, linked to the kit.
  expect(screen.getByText(/by maya/)).toBeTruthy();
});

test("a reader with no brand adopted is told what that means, and offered nothing", async () => {
  mount([], null);
  expect(await screen.findByText("No brand yet")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Set up your brand" })).toBeNull();
});

test("an administrator with no brand adopted is offered the setup, held until a logo and a colour are chosen", async () => {
  mount(["admin"], null);
  fireEvent.click(await screen.findByRole("button", { name: "Set up your brand" }));
  const dialog = await screen.findByRole("dialog");
  const adopt = within(dialog).getByRole("button", { name: "Adopt as the company brand" });
  expect(adopt.getAttribute("aria-disabled") ?? adopt.getAttribute("disabled")).not.toBeNull();
  expect(within(dialog).getByDisplayValue("FairSplit")).toBeTruthy();
  expect(within(dialog).getByText(/visible to everyone in the company/)).toBeTruthy();
});

test("an administrator edits one card at a time, and a colour that would be refused holds Save", async () => {
  mount(["admin"], ADOPTED);
  fireEvent.click(await screen.findByRole("button", { name: "Edit Colours" }));
  // ONE CARD AT A TIME: the others say why they wait.
  const logos = screen.getByRole("button", { name: "Edit Logos" });
  expect(logos.getAttribute("aria-disabled") ?? logos.getAttribute("disabled")).not.toBeNull();
  const text = screen.getByRole("textbox", { name: /^Text/ });
  fireEvent.change(text, { target: { value: "#9aa3ad" } });
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/must reach 4\.5:1/));
  const save = screen.getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled") ?? save.getAttribute("disabled")).not.toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
});

test("a newer version is offered to an administrator for review, and named to a reader who sees it", async () => {
  const newer = { ...ADOPTED, newer_version: 9, newer_by: { name: "brand-agent", kind: "agent" } };
  mount(["admin"], newer);
  expect(
    await screen.findByText(/Version 9 by brand-agent is newer than the adopted version 7\./),
  ).toBeTruthy();
  expect(screen.getByRole("button", { name: "Review and adopt" })).toBeTruthy();
  cleanup();
  mount([], newer);
  expect(await screen.findByText(/An administrator can review and adopt it\./)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Review and adopt" })).toBeNull();
});
