/**
 * The History panel's two dialogs: naming a version opens on that version's own
 * label (and Save cannot clear it unchanged), and Restore publishes nothing
 * until the reader confirms it.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { HistoryTab } from "./HistoryTab.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import {
  LiveSocket,
  Store,
  type ArtifactDetail,
  type ArtifactVersionSummary,
} from "~/protocol/index.ts";

// THE READER MAY MAKE EVERY CHANGE: what these cases hold is what the dialogs
// do, not who may open them (access.test.ts holds that).
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});

const ID = "0192f2c4-1111-7000-8000-000000000002";

const detail = {
  id: ID,
  kind: "html",
  title: "Launch review",
  head_number: 3,
  role: "owner",
  capabilities: [],
} as unknown as ArtifactDetail;

const version = (n: number, label = ""): ArtifactVersionSummary =>
  ({
    artifact: ID,
    number: n,
    version_id: `v${n}`,
    base: n - 1,
    files: 1,
    bytes: 64,
    author: { name: "ada", kind: "human" },
    created_at: "2026-10-18T10:00:00Z",
    position: n,
    capabilities: [],
    ...(label ? { label } : {}),
  }) as unknown as ArtifactVersionSummary;

const versions = [version(3), version(2, "Board review"), version(1)];

let writes: { method: string; path: string }[] = [];

beforeEach(() => {
  location.hash = `#/artifacts/${ID}?panel=history`;
  writes = [];
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      if (init?.method && init.method !== "GET") writes.push({ method: init.method, path });
      return new Response(JSON.stringify({ number: 4 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "";
});

const mount = () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) =>
    what === "artifact_version"
      ? // A WHOLE VERSION, as the engine answers it: its summary's fields
        // (capabilities among them) beside its manifest.
        { ...versions[0], manifest: { entry: "index.html", files: [] } }
      : { entries: [] };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <HistoryTab
            detail={detail}
            versions={versions}
            versionsError={null}
            versionsRefusal={null}
            showing={3}
            onShow={() => {}}
            room={{} as never}
            presence={{} as never}
            bridgeStores={new Set()}
          />
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
};

const labelButton = (n: number) =>
  screen.getByRole("button", { name: new RegExp(`^(Label|Name) version ${n}$`) });

test("the label dialog opens on that version's own label, never the last one typed", async () => {
  mount();
  fireEvent.click(labelButton(2));
  const field = (await screen.findByRole("textbox", { name: "Label" })) as HTMLInputElement;
  expect(field.value).toBe("Board review");
  // UNCHANGED, Save does not clear the label it opened on.
  expect(screen.getByRole("button", { name: "Save" }).getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(field, { target: { value: "Typed for version 2" } });
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("textbox", { name: "Label" })).toBeNull());

  fireEvent.click(labelButton(1));
  const next = (await screen.findByRole("textbox", { name: "Label" })) as HTMLInputElement;
  expect(next.value).toBe("");
  expect(next.placeholder).toBe("For example, Board review");
});

test("Restore asks before it publishes, and names the version it becomes", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Restore version 1" }));
  expect(
    await screen.findByRole("dialog", { name: "Publish version 1 again as version 4?" }),
  ).toBeTruthy();
  expect(writes).toEqual([]);
  fireEvent.click(screen.getByRole("button", { name: "Restore" }));
  await waitFor(() =>
    expect(writes).toEqual([{ method: "POST", path: `/artifacts/${ID}/versions/1/restore` }]),
  );
});
