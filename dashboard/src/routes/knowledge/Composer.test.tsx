/**
 * The composer: a new page, written as the person, filed where it was asked
 * for.
 *
 * What these protect:
 *  - IT IS FILED WHERE IT WAS ASKED FOR, and sent as `write_page` with the
 *    space, the parent, the labels and the status the writer chose.
 *  - A DRAFT IS A STATUS, not a second tool.
 *  - A TITLE ALREADY TAKEN IN THE SPACE IS SAID UNDER THE FIELD and holds both
 *    presses, beside a way to open the page that has it.
 *  - A TEMPLATE NEVER REPLACES WRITING.
 *  - SIMILAR PAGES ARE THE KNOWLEDGE SEARCH ON THE TITLE, asked once it rests.
 *  - NOTHING TYPED IS LOST TO A PRESS: Discard asks first.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider, parseMarkdown } from "@crewlethq/ui";

import { Composer, TEMPLATES } from "./Composer.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { fill, loadRichEditor } from "~/test/richEditor.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const PARENT = "11111111-1111-4111-8111-111111111111";
const TAKEN = "22222222-2222-4222-8222-222222222222";
const WRITTEN = "44444444-4444-4444-8444-444444444444";

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "knowledge"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["save_page", "comment_on_page", "write_page"],
};

const ORG = {
  name: "Acme",
  timezone: "Asia/Tokyo",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const summary = (over: Record<string, unknown>) => ({
  container: "ENG",
  status: "published",
  version: 1,
  revision: 1,
  updated_at: "2026-09-24T22:00:00Z",
  ...over,
});

let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = `#/knowledge/new?space=ENG&parent=${PARENT}`;
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: "CREWLET_PAGES_LOG@1:99",
          receipt: { id: WRITTEN },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

function mount() {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    const p = params ?? {};
    asked.push({ kind: what, params: p });
    switch (what) {
      case "viewer":
        return Promise.resolve(JANE);
      case "inbox":
        return Promise.resolve({
          handle: "jane",
          notices: [],
          primary_reasons: [],
          unread: 0,
          primary: 0,
        });
      case "decisions":
        return Promise.resolve({ handle: "jane", items: [], total: 0, capped: false });
      case "containers":
        return Promise.resolve({ containers: [{ key: "ENG", name: "Engineering", pages: 2 }] });
      case "pages":
        // THE TITLE CHECK asks with words from the title; the parent picker
        // asks for the space's pages.
        if (p.title === "Deploy runbook") {
          return Promise.resolve({
            pages: [summary({ id: TAKEN, title: "Deploy runbook" })],
            limit: 10,
            total: 1,
          });
        }
        if (p.title) return Promise.resolve({ pages: [], limit: 10, total: 0 });
        return Promise.resolve({
          pages: [summary({ id: PARENT, title: "Runbooks", folder: true })],
          limit: 500,
          total: 1,
        });
      case "knowledge":
        return Promise.resolve({
          backend: "native",
          query: p.q,
          mode: p.mode,
          served_mode: p.q ? "keyword" : "",
          modes: ["keyword"],
          degraded: "",
          available: true,
          reason: "",
          note: "",
          hits: p.q
            ? [
                {
                  id: TAKEN,
                  title: "Deploy runbook",
                  url: "",
                  container: "ENG",
                  snippet: "",
                  updated_at: "",
                },
              ]
            : [],
        });
      default:
        return Promise.resolve({});
    }
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <AppAnnouncer />
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <Composer />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { asked };
}

async function settle() {
  await afterMicrotasks();
}

const title = () => screen.getByRole("textbox", { name: "Title" });
const body = () => screen.getByRole("textbox", { name: "The page" });

beforeAll(() => loadRichEditor());

/** The first heading a template opens on, which the editor draws. */
function firstHeading(markdown: string): string {
  const first = parseMarkdown(markdown).find((b) => b.kind === "heading");
  return first && first.kind === "heading" ? first.text : "";
}

test("Publish writes the page as the person, filed where it was asked for", async () => {
  mount();
  await settle();
  fireEvent.change(title(), { target: { value: "Postmortem template" } });
  await fill(body(), [{ text: "What happened", heading: 2 }, { text: "One line." }]);
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Publish" }));
  await settle();
  expect(posted).toEqual([
    {
      tool: "write_page",
      args: {
        title: "Postmortem template",
        body: "## What happened\n\nOne line.",
        container: "ENG",
        parent: PARENT,
        status: "published",
      },
    },
  ]);
  // THE NEW PAGE, by the id the engine answered with.
  await waitFor(() => expect(location.hash).toBe(`#/knowledge/pages/${WRITTEN}`));
});

test("Save as draft is the same write with the draft status, and carries the labels", async () => {
  mount();
  await settle();
  fireEvent.change(title(), { target: { value: "Postmortem template" } });
  await fill(body(), [{ text: "Draft text" }]);
  // NO CHOICES TO PICK FROM, so the field is a plain text box.
  const labels = screen.getByRole("textbox", { name: "Labels" });
  fireEvent.change(labels, { target: { value: "ops" } });
  fireEvent.keyDown(labels, { key: "Enter" });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Save as draft" }));
  await settle();
  expect(posted[0]?.args).toMatchObject({ status: "draft", labels: ["ops"] });
});

test("a title already taken in the space is said, and holds both presses", async () => {
  mount();
  await settle();
  fireEvent.change(title(), { target: { value: "Deploy runbook" } });
  await fill(body(), [{ text: "Text" }]);
  // SAID UNDER THE FIELD, and again as the reason both presses are held.
  await waitFor(() =>
    expect(
      screen.getAllByText(/“Deploy runbook” already exists in Engineering/).length,
    ).toBeGreaterThan(0),
  );
  expect(screen.getByRole("link", { name: "Open it" }).getAttribute("href")).toBe(
    `#/knowledge/pages/${TAKEN}`,
  );
  for (const name of ["Publish", "Save as draft"]) {
    expect(screen.getByRole("button", { name }).getAttribute("aria-disabled"), name).toBe("true");
  }
});

test("a template starts an empty page, and never replaces what was written", async () => {
  mount();
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: "Runbook" }));
  const runbook = TEMPLATES.find((t) => t.value === "runbook")!.body;
  expect(within(body()).getByText(firstHeading(runbook))).toBeTruthy();
  // STILL THE TEMPLATE'S OWN TEXT: another template may take its place.
  fireEvent.click(screen.getByRole("radio", { name: "Decision record" }));
  const decision = TEMPLATES.find((t) => t.value === "decision")!.body;
  expect(within(body()).getByText(firstHeading(decision))).toBeTruthy();
  await fill(body(), [{ text: "Something I wrote." }]);
  fireEvent.click(screen.getByRole("radio", { name: "Runbook" }));
  expect(body().textContent).toBe("Something I wrote.");
  expect(screen.getByText("Clear the page first to start from a template.")).toBeTruthy();
});

test("similar pages are the knowledge search on the title, once it rests", async () => {
  const { asked } = mount();
  await settle();
  fireEvent.change(title(), { target: { value: "Deploy" } });
  const side = screen.getByRole("complementary", { name: "About the new page" });
  await waitFor(() =>
    expect(within(side).getByRole("link", { name: /Deploy runbook/ })).toBeTruthy(),
  );
  expect(asked.filter((a) => a.kind === "knowledge" && a.params.q === "Deploy")).toHaveLength(1);
});

test("Discard asks first when something is written, and leaves for where it came from", async () => {
  mount();
  await settle();
  fireEvent.change(title(), { target: { value: "Half a thought" } });
  fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  const ask = await screen.findByRole("alertdialog", { name: "Discard this page?" });
  // The same prompt every page a person makes something on asks.
  expect(ask.textContent).toContain("Its title and what you wrote are not saved anywhere yet.");
  fireEvent.click(within(ask).getByRole("button", { name: "Keep editing" }));
  await settle();
  expect((title() as HTMLInputElement).value).toBe("Half a thought");

  fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  fireEvent.click(
    within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard" }),
  );
  await waitFor(() => expect(location.hash).toBe(`#/knowledge/pages/${PARENT}`));
  expect(posted).toEqual([]);
});
