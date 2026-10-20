/**
 * The new slide deck page: a title, a theme and the slides it opens with, sent
 * as one plan the engine composes; and its loading, failing and leaving.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store, rest, type WorkProjectRow } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { readSlidesStarters } from "~/lib/artifacts/api.ts";
import type { SlidesStartersAnswer } from "~/protocol/index.ts";
import { SlidesCreate } from "./SlidesCreate.tsx";

const starters = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/api/artifactsapi/testdata/slides-starters.golden.json"),
    "utf8",
  ),
) as SlidesStartersAnswer;

vi.mock("~/lib/artifacts/api.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/artifacts/api.ts")>("~/lib/artifacts/api.ts");
  return { ...actual, readSlidesStarters: vi.fn() };
});

// THE ENGINE'S ANSWER TO A CREATE, at the transport every keyed write goes through.
const request = vi.spyOn(rest, "request");

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "artifacts"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["create_work_item"],
  project: "OPS",
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    {
      name: "Agent Marketer",
      handle: "marketer",
      kind: "agent",
      goal: "Pitch decks and product marketing",
    },
  ],
  units: [],
};

const project = (key: string): WorkProjectRow =>
  ({ key, name: key, unit: { resolved: true }, lead: { handle: "jane" } }) as WorkProjectRow;

/** The tracker acts the page sent (`create_work_item`), as the operator route receives them. */
let acts: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.mocked(readSlidesStarters).mockResolvedValue(starters);
  acts = [];
  request.mockReset();
  request.mockImplementation((async (
    _method: string,
    path: string,
    options?: { body?: unknown },
  ) => {
    if (path.startsWith("/operator/act/")) {
      const tool = decodeURIComponent(path.slice("/operator/act/".length));
      acts.push({ tool, args: (options?.body as { args: Record<string, unknown> }).args });
      return {
        status: 200,
        body: {
          tool,
          outcome: "applied",
          position: "CREWLET_TRACKER_LOG@1:9",
          receipt: { id: "0f0f", item: "OPS-7", key: "OPS-7" },
        },
      };
    }
    if (path.endsWith("/work-items")) return { status: 200, body: {} };
    return { status: 201, body: { artifact: "a-1" } };
  }) as never);
});
afterEach(() => {
  cleanup();
  localStorage.clear();
  reloadForTest();
});

function mount() {
  location.hash = "#/artifacts/new/slides";
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => {
    const answers: Record<string, unknown> = {
      viewer: JANE,
      work_projects: { projects: [project("OPS")] },
      work_inbox: { handle: "jane", notices: [], unread: 0, primary: 0 },
    };
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
  return render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <SlidesCreate />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

const title = () => screen.findByRole("textbox", { name: "Title" });

test("it opens on the title, in Nightshade, with the engine's four starting slides", async () => {
  mount();
  const field = await title();
  await waitFor(() => expect(document.activeElement).toBe(field));
  expect(screen.getByRole("radio", { name: /^Nightshade,/ }).getAttribute("aria-checked")).toBe(
    "true",
  );
  expect(document.querySelectorAll(".crewlet-slide-filmstrip__thumb")).toHaveLength(4);
  expect(screen.getByRole("button", { name: "Create deck" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
});

test("Create deck sends the title, the theme and every slide as one plan, then opens the deck", async () => {
  mount();
  fireEvent.change(await title(), { target: { value: "Board review" } });
  fireEvent.click(screen.getByRole("radio", { name: /^Grove,/ }));
  fireEvent.click(screen.getByRole("button", { name: "Slide 2, Agenda", exact: true } as never));
  fireEvent.click(screen.getByRole("button", { name: "New slide" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Big number/ }));
  fireEvent.change(screen.getByRole("textbox", { name: "Slide title" }), {
    target: { value: "Uptime" },
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create deck" }));
  });
  await waitFor(() => expect(location.hash).toBe("#/artifacts/a-1"));
  const [method, path, sent] = request.mock.calls[0]!;
  expect([method, path]).toEqual(["POST", "/artifacts"]);
  expect((sent as { body: unknown }).body).toEqual({
    kind: "slides",
    title: "Board review",
    starter: true,
    deck: {
      theme: "grove",
      slides: [
        { layout: "cover" },
        { layout: "agenda" },
        { layout: "number", title: "Uptime" },
        { layout: "columns" },
        { layout: "closing" },
      ],
    },
  });
});

test("leaving with a title typed asks first, and keeping on stays", async () => {
  mount();
  fireEvent.change(await title(), { target: { value: "Board review" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(await screen.findByText("Leave without creating the deck?")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
  expect(location.hash).toBe("#/artifacts/new/slides");
});

test("themes that did not load say so and offer to ask again", async () => {
  vi.mocked(readSlidesStarters).mockRejectedValueOnce(new Error("down"));
  mount();
  const retry = await screen.findByRole("button", { name: "Try again" });
  fireEvent.click(retry);
  expect(await title()).toBeTruthy();
});

test("asking an agent makes the deck shared with it, files its task linked to the deck, and opens the task", async () => {
  mount();
  fireEvent.change(await title(), { target: { value: "Board review" } });
  fireEvent.click(screen.getByRole("radio", { name: "An agent" }));
  const ask = await screen.findByRole("button", { name: "Ask Agent Marketer" });
  expect(ask.getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(screen.getByRole("textbox", { name: "Brief" }), {
    target: { value: "A ten-minute update for the board." },
  });
  await waitFor(() => expect(ask.getAttribute("aria-disabled")).toBeNull());
  await act(async () => {
    fireEvent.click(ask);
  });
  await waitFor(() => expect(location.hash).toBe("#/work/OPS-7"));
  const [, path, made] = request.mock.calls[0]!;
  expect(path).toBe("/artifacts");
  expect((made as { body: Record<string, unknown> }).body).toMatchObject({
    kind: "slides",
    title: "Board review",
    starter: true,
    deck: { theme: "nightshade" },
    shares: [{ audience: { kind: "seat", id: "marketer" }, role: "editor" }],
  });
  const filed = acts.find((a) => a.tool === "create_work_item")!;
  expect(filed.args).toMatchObject({ title: "Board review", assignee: "marketer", project: "OPS" });
  expect(String(filed.args.body)).toContain("A ten-minute update for the board.");
  expect(String(filed.args.body)).toContain("1. Cover: Board review");
  expect(String(filed.args.body)).toContain("artifact a-1");
  const [, linkPath, link] = request.mock.calls.find(([, p]) => String(p).endsWith("/work-items"))!;
  expect(linkPath).toBe("/artifacts/a-1/work-items");
  expect((link as { body: unknown }).body).toEqual({
    work_item: "OPS-7",
    after: "CREWLET_TRACKER_LOG@1:9",
  });
});
