/**
 * An artifact's page frame: one slim bar and the artifact, the panels at its
 * side opened from the bar and by their keys, each in the address so a link
 * opens on it and Back closes it, the addresses the old tabs had still
 * opening what they became, and a kind's own controls drawn in the bar.
 *
 * WHAT IS HELD HERE IS THE FRAME: the kind's view, the room, the datastore and
 * each panel's own body are stood in for, since each has suites of its own.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { ArtifactPage } from "./ArtifactPage.tsx";
import { useKindControls } from "~/lib/artifacts/kindControls.ts";
import { Router } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useDataStore } from "~/lib/artifacts/useDataStore.ts";
import type { ArtifactDetail, ArtifactVersionSummary } from "~/protocol/index.ts";

const ID = "0192f2c4-3333-7000-8000-000000000003";

let detail: ArtifactDetail;
const versions = [3, 2, 1].map(
  (n) =>
    ({
      artifact: ID,
      number: n,
      author: { name: "ada", kind: "human" },
      created_at: "2026-10-18T10:00:00Z",
    }) as unknown as ArtifactVersionSummary,
);
const refetch = vi.fn();

vi.mock("~/lib/useQuery.ts", () => ({ useQuery: vi.fn() }));
vi.mock("~/lib/artifacts/useDataStore.ts", () => ({ useDataStore: vi.fn(() => null) }));
const roomOthers = vi.hoisted(() => [] as unknown[]);
vi.mock("~/lib/artifacts/room.ts", () => ({
  useArtifactRoom: () => ({
    state: { kind: "joined" },
    others: roomOthers,
    roster: { members: [] },
  }),
}));
vi.mock("./presence.ts", () => ({ useArtifactPresence: () => ({ at: () => {} }) }));
vi.mock("~/lib/artifacts/people.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/people.ts")>(
    "~/lib/artifacts/people.ts",
  );
  const people = {
    index: { seats: [], units: [] },
    company: "Acme",
    seatName: (s: string) => s,
    unitName: (s: string) => s,
    seatPrincipal: (s: string) => ({ id: s, name: s, kind: "person" }),
    unitPrincipal: (s: string) => ({ id: s, name: s, kind: "team" }),
    sheetPeople: [],
    pagePerson: () => ({}),
    pagePeople: [],
  };
  return { ...actual, useArtifactPeople: () => people };
});
const reader = vi.hoisted(() => ({ handle: "", login: "" }));
vi.mock("~/lib/viewer.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/viewer.ts")>("~/lib/viewer.ts");
  return { ...actual, useViewer: () => reader };
});
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useConnection: () => ({ connected: true, accessRefused: null }) };
});
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});
vi.mock("~/lib/artifacts/pins.ts", () => ({
  usePins: () => ({ data: { items: [] } }),
  usePinToggle: () => ({ label: "Pin", toggle: vi.fn(), pinned: false, busy: false }),
}));
// THE PANELS' OWN BODIES, stood in for by their names.
vi.mock("./HistoryTab.tsx", () => ({ HistoryTab: () => <p>The versions</p> }));
vi.mock("./DataTab.tsx", () => ({ DataTab: () => <p>The rows</p> }));
vi.mock("./SettingsTab.tsx", () => ({
  SettingsTab: () => <p>The settings</p>,
  TrashDialog: () => null,
  PurgeControl: () => null,
}));
vi.mock("./Share.tsx", () => ({
  ArtifactShare: ({ open }: { open: boolean }) => (open ? <p>The share dialog</p> : null),
}));
// THE KIND'S VIEW: a deck's, publishing its views to the bar as SlidesView does.
const onMode = vi.fn();
vi.mock("./ViewTab.tsx", () => ({
  ViewTab: ({ detail: d }: { detail: ArtifactDetail }) => <KindStandIn kind={d.kind} />,
}));
function KindStandIn({ kind }: { kind: string }) {
  useKindControls(
    kind === "slides"
      ? {
          modes: {
            label: "View",
            value: "",
            options: [
              { value: "", label: "Edit" },
              { value: "grid", label: "Grid" },
            ],
            onValueChange: onMode,
          },
          menu: [{ key: "print", label: "Print or save as PDF", onSelect: () => {} }],
        }
      : null,
  );
  return <p>The artifact itself</p>;
}

function mount(hash: string) {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <ArtifactPage id={ID} />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
}

/** The bar's control by name: the wide one (the narrow bar folds it into More). */
function tool(name: string): HTMLElement {
  const bar = screen.getByRole("banner");
  return within(bar.querySelector(".crewlet-artifact-bar__end") as HTMLElement).getAllByRole(
    "button",
    { name },
  )[0]!;
}

function press(key: string) {
  act(() => {
    window.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
  });
}

beforeEach(() => {
  detail = {
    id: ID,
    kind: "slides",
    title: "Q4 roadmap",
    description: "The quarter's plan.",
    owner: "ada",
    head_number: 3,
    role: "owner",
    status: "live",
    capabilities: [],
    share_count: 2,
    created_by: { name: "ada", kind: "human" },
    updated_by: { name: "ada", kind: "human" },
    created_at: "2026-10-17T10:00:00Z",
    updated_at: "2026-10-18T10:00:00Z",
  } as unknown as ArtifactDetail;
  vi.mocked(useQuery).mockImplementation(((what: string) => {
    const data = what === "artifact" ? detail : what === "artifact_versions" ? { versions } : null;
    return { data, loading: false, error: null, refusal: null, refetch };
  }) as never);
  vi.mocked(useDataStore).mockReturnValue(null);
  onMode.mockClear();
  reader.handle = "";
  reader.login = "";
  roomOthers.length = 0;
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

test("draws one bar over the artifact: the title as the heading, and no tab strip", () => {
  mount(`#/artifacts/${ID}`);
  const bar = screen.getByRole("banner");
  expect(within(bar).getByRole("heading", { level: 1, name: "Q4 roadmap" })).toBeTruthy();
  expect(within(bar).getByRole("link", { name: "Back to artifacts" }).getAttribute("href")).toBe(
    "#/artifacts",
  );
  expect(within(bar).getByRole("button", { name: "Versions: v3" })).toBeTruthy();
  expect(screen.queryByRole("tablist")).toBeNull();
  expect(screen.getByText("The artifact itself")).toBeTruthy();
  expect(screen.queryByRole("complementary")).toBeNull();
});

test("a control opens its panel beside the artifact, in the address, and pressing it again closes it", () => {
  mount(`#/artifacts/${ID}`);
  const details = tool("Details");
  details.focus();
  fireEvent.click(details);
  const panel = screen.getByRole("complementary", { name: "Details" });
  expect(location.hash).toBe(`#/artifacts/${ID}?panel=details`);
  expect(tool("Details").getAttribute("aria-pressed")).toBe("true");
  // THE FACTS THE HEADER USED TO STACK, now in the panel.
  expect(within(panel).getByText("The quarter's plan.")).toBeTruthy();
  expect(within(panel).getByText("3, the latest")).toBeTruthy();
  // Focus is on the panel's heading, so a screen reader says what opened.
  expect(document.activeElement).toBe(within(panel).getByRole("heading", { name: "Details" }));
  fireEvent.click(tool("Details"));
  expect(screen.queryByRole("complementary")).toBeNull();
  expect(location.hash).toBe(`#/artifacts/${ID}`);
});

test("a link opens on its panel, and leaves focus where the page put it", () => {
  mount(`#/artifacts/${ID}?panel=history`);
  const panel = screen.getByRole("complementary", { name: "Versions and history" });
  expect(within(panel).getByText("The versions")).toBeTruthy();
  expect(panel.contains(document.activeElement)).toBe(false);
});

test("the address an old tab or the people rail had opens the panel it became", async () => {
  mount(`#/artifacts/${ID}?tab=settings`);
  expect(screen.getByRole("complementary", { name: "Settings" })).toBeTruthy();
  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${ID}?panel=settings`));
  cleanup();
  mount(`#/artifacts/${ID}?rail=people`);
  expect(screen.getByRole("complementary", { name: "People here" })).toBeTruthy();
  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${ID}?panel=people`));
});

test("each panel has its key, and the key closes what it opened", () => {
  mount(`#/artifacts/${ID}`);
  press("i");
  expect(screen.getByRole("complementary", { name: "Details" })).toBeTruthy();
  press("h");
  expect(screen.getByRole("complementary", { name: "Versions and history" })).toBeTruthy();
  press("p");
  expect(screen.getByRole("complementary", { name: "People here" })).toBeTruthy();
  press("p");
  expect(screen.queryByRole("complementary")).toBeNull();
  press("s");
  expect(screen.getByText("The share dialog")).toBeTruthy();
});

// AN OLDER VERSION WAS ONLY A TINTED CHIP: nothing said why a deck's Edit
// had gone, or offered the way back.
test("an older version says so, with the way back to the latest", () => {
  mount(`#/artifacts/${ID}?version=1`);
  expect(screen.getByText(/You are looking at version 1, an older version/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Show the latest" }));
  expect(location.hash).toBe(`#/artifacts/${ID}`);
  expect(screen.queryByText(/an older version/)).toBeNull();
});

// A READER SIGNED IN WITH NO SEAT saw a People panel of everybody but
// themselves: "You" was drawn only from a seat.
test("the People panel starts with the reader, seat or none", () => {
  reader.login = "token:review";
  mount(`#/artifacts/${ID}?panel=people`);
  const panel = screen.getByRole("complementary", { name: "People here" });
  expect(within(panel).getByText("You")).toBeTruthy();
});

// THE ROOM NAMES A SEATLESS READER BY THEIR LOGIN, so their own second tab is
// theirs: compared against the seat alone, it was drawn as somebody else
// they could follow.
test("a seatless reader's own other tab is counted beside You", () => {
  reader.login = "token:review";
  roomOthers.push({
    session: "s-2",
    seat: "token:review",
    kind: "operator",
    activity: "viewing",
    focus: {},
    at: "2026-10-18T10:00:00Z",
  });
  mount(`#/artifacts/${ID}?panel=people`);
  const panel = screen.getByRole("complementary", { name: "People here" });
  expect(within(panel).getByText("You, 2 tabs")).toBeTruthy();
  expect(within(panel).queryByText("token:review")).toBeNull();
});

test("Data is a control and a key only where the artifact keeps data", () => {
  mount(`#/artifacts/${ID}`);
  expect(within(screen.getByRole("banner")).queryByRole("button", { name: "Data" })).toBeNull();
  press("d");
  expect(screen.queryByRole("complementary")).toBeNull();
  cleanup();
  detail = { ...detail, kind: "sheet" } as ArtifactDetail;
  vi.mocked(useDataStore).mockReturnValue({ schema: null, docsOf: () => [] } as never);
  mount(`#/artifacts/${ID}`);
  press("d");
  const panel = screen.getByRole("complementary", { name: "Data" });
  expect(within(panel).getByText("The rows")).toBeTruthy();
  // A table the side cannot hold is expanded to the whole workspace, in the address.
  fireEvent.click(within(panel).getByRole("button", { name: "Expand to the full view" }));
  expect(location.hash).toBe(`#/artifacts/${ID}?panel=data&expand=1`);
});

test("a sheet's version names nothing a reader sees change, so it draws no chip", () => {
  detail = { ...detail, kind: "sheet" } as ArtifactDetail;
  mount(`#/artifacts/${ID}`);
  expect(screen.queryByRole("button", { name: /^Versions:/ })).toBeNull();
  // AND ITS PANEL PROMISES NO VERSIONS: it is the sheet's History.
  fireEvent.click(tool("History"));
  expect(screen.getByRole("complementary", { name: "History" })).toBeTruthy();
});

test("Escape from inside a panel closes it and hands focus back to its control", () => {
  mount(`#/artifacts/${ID}`);
  const details = tool("Details");
  details.focus();
  fireEvent.click(details);
  const heading = screen.getByRole("heading", { name: "Details" });
  fireEvent.keyDown(heading, { key: "Escape" });
  expect(screen.queryByRole("complementary")).toBeNull();
  expect(document.activeElement).toBe(tool("Details"));
});

test("a kind's views are drawn in the bar and its rows in More", () => {
  mount(`#/artifacts/${ID}`);
  const bar = screen.getByRole("banner");
  const views = within(bar.querySelector(".crewlet-artifact-bar__modes") as HTMLElement);
  fireEvent.click(views.getByRole("radio", { name: "Grid" }));
  expect(onMode).toHaveBeenCalledWith("grid");
  const wideMore = [...bar.querySelectorAll(".crewlet-artifact-bar__wide")]
    .map((el) => within(el as HTMLElement).queryByRole("button", { name: /^More for/ }))
    .find(Boolean)!;
  fireEvent.click(wideMore);
  const menu = screen.getByRole("menu");
  expect(within(menu).getByRole("menuitem", { name: "Print or save as PDF" })).toBeTruthy();
  expect(within(menu).getByRole("menuitem", { name: "Rename" })).toBeTruthy();
  expect(within(menu).getByRole("menuitem", { name: "Move to the trash" })).toBeTruthy();
});

test("the title is renamed in place, through the same route Settings renames by", async () => {
  const sent: { method: string; path: string; body: unknown }[] = [];
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : null,
      });
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    }),
  });
  mount(`#/artifacts/${ID}`);
  const heading = screen.getByRole("heading", { level: 1 });
  fireEvent.click(within(heading).getByRole("button", { name: "Q4 roadmap" }));
  const field = screen.getByRole("textbox", { name: "Title" });
  fireEvent.change(field, { target: { value: "Q4 plan" } });
  await act(async () => {
    fireEvent.keyDown(field, { key: "Enter" });
  });
  expect(sent).toContainEqual({
    method: "PATCH",
    path: `/artifacts/${ID}`,
    body: { title: "Q4 plan" },
  });
  expect(refetch).toHaveBeenCalled();
});
