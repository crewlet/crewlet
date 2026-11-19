/**
 * A finding's page over its routes, stubbed at `fetch` and the socket: whose
 * words are the model's, where a snoozed or dismissed finding stands and what
 * its presses say, and the fix's presses beside its words.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { AssistantControlContext, type AssistantControl } from "~/app/assistant/control.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FindingPage } from "./FindingPage.tsx";
import type { AdvisorFindingDetail } from "~/protocol/advisorFindings.ts";

const NOW = Date.now();
const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();
const ID = "00000000-0000-4000-8000-000000000042";

function detail(patch: Partial<AdvisorFindingDetail> = {}): AdvisorFindingDetail {
  return {
    id: ID,
    key: "sandbox.direct_box:seat:agent-swe",
    class: "sandbox.direct_box",
    source: "rule",
    actor: "owner",
    area: "sandbox",
    severity: "note",
    subject: {
      kind: "seat",
      id: "agent-swe",
      name: "Agent SWE",
      seat: "agent-swe",
      unit: "",
      schedule: "",
    },
    title: "Agent runs share the engine's server.",
    why: "Runs share the server, so a runaway run slows every seat.",
    item: "",
    names: [],
    path: "",
    entries: null,
    explanation: null,
    state: "open",
    close_reason: null,
    ineffective: null,
    snoozed_until: null,
    opened_at: ago(60 * 24),
    last_confirmed_at: ago(4),
    closed_at: null,
    effect_due_at: null,
    confirmed: "fresh",
    revision: 1,
    seen_before: 0,
    impact: null,
    evidence: { window: { from: ago(60 * 24 * 7), to: ago(0) }, facts: [], records: [] },
    fix: { kind: "none", sentence: "Nothing to send: open the records to look.", options: [] },
    history: [{ at: ago(60 * 24), event: "opened", by: { kind: "engine", handle: "" }, moved: [] }],
    episodes: [],
    ...patch,
  };
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

/** The engine: the finding over the socket, each decision answered with `answered`, every request recorded. */
function mount(shown: AdvisorFindingDetail, answered: AdvisorFindingDetail = shown) {
  const sent: { path: string; body: unknown }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init: RequestInit = {}) => {
      const url = new URL(input);
      sent.push({ path: url.pathname, body: init.body ? JSON.parse(String(init.body)) : null });
      return url.pathname.endsWith("/decision")
        ? json(answered)
        : json({ error: "not_found" }, 404);
    }),
  );
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (name: string) => Promise<unknown> }).query = (name) =>
    Promise.resolve(name === "advisor_finding" ? shown : {});
  const control: AssistantControl = {
    open: false,
    unavailable: null,
    toggle: () => {},
    ask: () => {},
    draft: () => {},
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <AssistantControlContext.Provider value={control}>
          <ToastProvider>
            <LayerHost>
              <FindingPage id={ID} />
            </LayerHost>
          </ToastProvider>
        </AssistantControlContext.Provider>
      </Router>
    </ClientContext.Provider>,
  );
  return sent;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// THE MODEL'S WORDS ARE SAID TO BE ITS OWN: drawn as a second paragraph under
// the engine's why, they read as the engine's.
test("the model's explanation is headed as the model's, and a model finding is marked", async () => {
  mount(
    detail({
      source: "model",
      explanation: "One failed disk would take every log with it.",
    }),
  );
  const said = await screen.findByRole("region", { name: "The Advisor's model explains" });
  expect(within(said).getByText("One failed disk would take every log with it.")).toBeDefined();
  expect(screen.getByText("Found by the Advisor's model")).toBeDefined();
  expect(
    within(screen.getByRole("region", { name: "Why" })).queryByText(/One failed disk/),
  ).toBeNull();
});

// A FIX WITH NO PRESS OF ITS OWN keeps its menu on its sentence's line.
test("a fix with nothing to send keeps its menu beside its words", async () => {
  mount(detail());
  const menu = await screen.findByRole("button", { name: /More for/ });
  expect(menu.closest(".advisor-fix-line")?.textContent).toContain(
    "Nothing to send: open the records to look.",
  );
});

// A SNOOZE IS SEEN ON THE PAGE, and can be ended there.
test("a snoozed finding says until when, and End the snooze reopens it", async () => {
  const until = new Date(NOW + 6 * 86_400_000).toISOString();
  const sent = mount(detail({ snoozed_until: until }));
  expect(await screen.findByText(/^Snoozed until /)).toBeDefined();
  expect(screen.queryByText("Open")).toBeNull();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "End the snooze" }));
  });
  await waitFor(() =>
    expect(sent.find((r) => r.path.endsWith("/decision"))?.body).toEqual({ decision: "reopen" }),
  );
});

// A REOPENED DISMISSAL IS SAID AS IT IS: the engine answers the dismissed
// episode as it was, and the condition comes back as a new finding if it
// still holds, so the page never says "Reopened" over a finding still
// reading Dismissed.
test("reopening a dismissed finding says its dismissal was lifted, not that it reopened", async () => {
  const dismissed = detail({
    state: "dismissed",
    close_reason: "dismissed_wrong",
    closed_at: ago(30),
  });
  mount(dismissed, dismissed);
  await screen.findByText(/Dismissed as wrong/);
  await act(async () => {
    fireEvent.click(await screen.findByRole("button", { name: "Reopen" }));
  });
  expect(await screen.findByText("Dismissal lifted")).toBeDefined();
  expect(screen.queryByText("Reopened")).toBeNull();
});

test("a dismissal already lifted is not offered again", async () => {
  mount(
    detail({
      state: "dismissed",
      close_reason: "dismissed_known",
      closed_at: ago(30),
      history: [
        { at: ago(90), event: "opened", by: { kind: "engine", handle: "" }, moved: [] },
        { at: ago(30), event: "closed", by: { kind: "person", handle: "jane" }, moved: [] },
        { at: ago(10), event: "released", by: { kind: "person", handle: "jane" }, moved: [] },
      ],
    }),
  );
  expect(await screen.findByText(/Its dismissal was lifted/)).toBeDefined();
  expect(screen.queryByRole("button", { name: "Reopen" })).toBeNull();
  expect(screen.getByText(/^Dismissal lifted/)).toBeDefined();
});
