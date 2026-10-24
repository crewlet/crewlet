/**
 * The folders: the Folders section and a folder's page, their tiles, trail,
 * header and empty states; New folder's two presets; Remove's words; and
 * Move to…, its paths, its current place, its inline create, a folder move
 * that widens, and the selection bar that opens it.
 *
 * The engine's answers are stood in for by `useQuery` (the tree and the
 * listing) and `keyedWrite` (every write, recorded), and every live preview
 * draws its stand-in, as the gallery's suite does.
 */

import type { ReactNode } from "react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { crumbsFor, folderUpKey } from "~/app/crumbs.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import type { ArtifactFolderItem, ArtifactListItem } from "~/protocol/index.ts";
import { ArtifactFolders } from "./Folders.tsx";
import { ArtifactFolderPage } from "./FolderPage.tsx";
import { MoveDialog, makingOf } from "./MoveDialog.tsx";
import { indexFolders } from "~/lib/artifacts/folders.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

vi.mock("~/lib/useQuery.ts", () => ({ useQuery: vi.fn() }));
vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  const write = vi.fn();
  return { ...actual, keyedWrite: write, useKeyedGesture: () => actual.useKeyedGesture(write) };
});
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return {
    ...actual,
    useClient: () => ({ socket: { query: vi.fn() } }),
    useOrg: () => ({ name: "FairSplit", seats: [], units: [] }),
  };
});
vi.mock("~/lib/viewer.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/viewer.ts")>("~/lib/viewer.ts");
  return {
    ...actual,
    useViewer: () => ({ owner: "ada", admin: false, handle: "", kind: "human" }),
  };
});
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return {
    ...actual,
    useArtifactAccess: (need: { role?: string; held?: string }) =>
      authoring && (!need.role || need.held === "owner" || need.held === "editor")
        ? { can: true, as: "ada", acts: [] }
        : { can: false, block: "no_access", reason: "This change needs the Artifacts access." },
  };
});
vi.mock("~/lib/artifacts/pinToggle.ts", () => ({
  usePinToggle: () => ({ label: "Pin to your sidebar", toggle: vi.fn(), pinned: false }),
}));

const MARKETING = "11111111-1111-4111-8111-111111111111";
const Q4 = "22222222-2222-4222-8222-222222222222";
const PRESS = "33333333-3333-4333-8333-333333333333";
const BOARD = "44444444-4444-4444-8444-444444444444";

function folder(
  over: Partial<ArtifactFolderItem> & { id: string; name: string },
): ArtifactFolderItem {
  return {
    root: over.parent ? MARKETING : over.id,
    owner: "ada",
    owner_kind: "human",
    depth: 1,
    max_depth: 4,
    visibility: "company",
    status: "live",
    created_by: { name: "ada", kind: "human" },
    created_at: "2026-10-20T10:00:00Z",
    updated_by: { name: "ada", kind: "human" },
    updated_at: "2026-10-20T10:00:00Z",
    version: 1,
    artifacts: 0,
    folders: 0,
    role: "owner",
    ...over,
  } as ArtifactFolderItem;
}

function row(over: Partial<ArtifactListItem>): ArtifactListItem {
  return {
    id: "a1",
    kind: "doc",
    title: "Pricing FAQ",
    owner: "ada",
    owner_kind: "human",
    head_number: 1,
    role: "owner",
    status: "live",
    capabilities: [],
    share_count: 0,
    created_by: { name: "ada", kind: "human" },
    updated_by: { name: "ada", kind: "human" },
    created_at: "2026-10-18T10:00:00Z",
    updated_at: "2026-10-18T10:00:00Z",
    trashed_at: "0001-01-01T00:00:00Z",
    opened_at: "0001-01-01T00:00:00Z",
    ...over,
  } as ArtifactListItem;
}

let tree: ArtifactFolderItem[];
let contents: ArtifactListItem[];
let asked: Record<string, unknown>[];
let authoring = true;
const sent: KeyedRequest[] = [];
let answer: (req: KeyedRequest) => KeyedAnswer;

beforeEach(() => {
  tree = [
    folder({ id: MARKETING, name: "Marketing", artifacts: 12, folders: 1 }),
    folder({ id: BOARD, name: "Board", visibility: "private", artifacts: 4 }),
    folder({ id: Q4, name: "Q4 review", parent: MARKETING, depth: 2, folders: 1 }),
    folder({ id: PRESS, name: "Press kit", parent: Q4, depth: 3, artifacts: 5 }),
  ];
  contents = [
    row({}),
    row({ id: "a2", title: "Settlement pulse", kind: "dashboard", role: "viewer" }),
  ];
  asked = [];
  authoring = true;
  sent.length = 0;
  answer = () => ({ kind: "done", pending: false, body: {} });
  vi.mocked(keyedWrite).mockImplementation(async (req) => {
    sent.push(req);
    return answer(req);
  });
  vi.mocked(useQuery).mockImplementation(((what: string, params?: Record<string, unknown>) => {
    if (what === "artifact_folders") {
      const data = { folders: tree, top_level: 23, read_level: "", position: {}, complete: true };
      return { data, loading: false, error: null, refusal: null, refetch: vi.fn() };
    }
    if (what === "artifacts") {
      asked.push(params ?? {});
      return {
        data: { contents, total: contents.length, prefix: "", key_count: contents.length },
        loading: false,
        error: null,
        refusal: null,
        refetch: vi.fn(),
      };
    }
    return { data: null, loading: false, error: null, refusal: null, refetch: vi.fn() };
  }) as never);
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

function mount(node: ReactNode, hash: string) {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>{node}</ToastProvider>
      </LayerHost>
    </Router>,
  );
}

describe("the Folders section", () => {
  test("its top-level folders are tiles saying who sees each and what the reader can open, then Not in a folder", () => {
    mount(<ArtifactFolders />, "#/artifacts/folders");
    const tiles = screen.getByRole("list", { name: "Folders" });
    const marketing = within(tiles).getByRole("link", { name: "Marketing" });
    expect(marketing.getAttribute("href")).toBe(`#/artifacts/folders/${MARKETING}`);
    expect(
      within(tiles)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual([expect.stringContaining("Board"), expect.stringContaining("Marketing")]);
    expect(marketing.closest("li")?.textContent).toContain("Everyone");
    expect(marketing.closest("li")?.textContent).toContain("12 artifacts · 1 folder");
    expect(within(tiles).getByRole("link", { name: "Board" }).closest("li")?.textContent).toContain(
      "Only you",
    );
    expect(screen.getByRole("region", { name: "Not in a folder · 23" })).toBeTruthy();
    // THE TOP LEVEL, ONE LEVEL: no folder named, the delimiter set.
    expect(asked.at(-1)).toMatchObject({ delimiter: "/", scope: "all" });
    expect(asked.at(-1)).not.toHaveProperty("folder");
  });

  test("in the list shape the folders are rows, each its contents under its name and a link to its page", () => {
    localStorage.setItem(STORAGE_KEYS.artifactsShape, "list");
    mount(<ArtifactFolders />, "#/artifacts/folders");
    const rows = screen.getByRole("group", { name: "Folders" });
    const marketing = within(rows)
      .getByText("Marketing")
      .closest(".crewlet-data-grid__row") as HTMLElement;
    expect(marketing.textContent).toContain("12 artifacts · 1 folder");
    expect(marketing.textContent).toContain("Everyone");
    expect(screen.queryByRole("list", { name: "Folders" })).toBeNull();
  });

  test("with no folders and nothing at the top level it says so, and offers New folder", () => {
    tree = [];
    contents = [];
    mount(<ArtifactFolders />, "#/artifacts/folders");
    expect(screen.getByText("No folders yet")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "New folder" }).length).toBeGreaterThan(0);
  });

  test("with no folders yet but things to file, it says so above what is in no folder", () => {
    tree = [];
    mount(<ArtifactFolders />, "#/artifacts/folders");
    expect(screen.getByText("No folders yet")).toBeTruthy();
    expect(screen.getByRole("region", { name: /Not in a folder/ })).toBeTruthy();
  });

  test("New folder at the top level asks who sees it, and sends the preset with the name", async () => {
    answer = () => ({
      kind: "done",
      pending: false,
      body: {
        folders: [folder({ id: "55555555-5555-4555-8555-555555555555", name: "Partnerships" })],
      },
    });
    mount(<ArtifactFolders />, "#/artifacts/folders");
    fireEvent.click(screen.getByRole("button", { name: "New folder" }));
    const dialog = screen.getByRole("dialog", { name: "New folder" });
    expect(within(dialog).getByRole("radio", { name: /Everyone at FairSplit/ })).toHaveProperty(
      "checked",
      true,
    );
    expect(within(dialog).getByText(/A folder never shares what is in it/)).toBeTruthy();
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Name" }), {
      target: { value: "Partnerships" },
    });
    fireEvent.click(within(dialog).getByRole("radio", { name: /Only you/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create folder" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({
      method: "POST",
      path: "/artifact-folders",
      body: { name: "Partnerships", visibility: "private" },
    });
  });

  test("a name a sibling the reader sees already carries holds Create with the reason", () => {
    mount(<ArtifactFolders />, "#/artifacts/folders");
    fireEvent.click(screen.getByRole("button", { name: "New folder" }));
    const dialog = screen.getByRole("dialog", { name: "New folder" });
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Name" }), {
      target: { value: "marketing" },
    });
    const create = within(dialog).getByRole("button", { name: "Create folder" });
    expect(create.getAttribute("aria-disabled")).toBe("true");
  });
});

describe("a folder's page", () => {
  test("its header names it and who sees it, its subfolders come first, and it lists one level by id", () => {
    mount(<ArtifactFolderPage id={Q4} />, `#/artifacts/folders/${Q4}`);
    expect(screen.getByRole("heading", { level: 2, name: "Q4 review" })).toBeTruthy();
    expect(screen.getByText("Everyone at FairSplit")).toBeTruthy();
    const tiles = screen.getByRole("list", { name: "Folders in Q4 review" });
    expect(within(tiles).getByRole("link", { name: "Press kit" })).toBeTruthy();
    expect(asked.at(-1)).toMatchObject({ folder: Q4, delimiter: "/" });
    expect(screen.getByRole("searchbox", { name: "Search Q4 review" })).toBeTruthy();
  });

  test("a phrase searches everything beneath it, and each row then names its folder", () => {
    localStorage.setItem(STORAGE_KEYS.artifactsShape, "list");
    contents = [row({ folder_path: "Marketing/Q4 review/Press kit/" })];
    mount(<ArtifactFolderPage id={Q4} />, `#/artifacts/folders/${Q4}?q=press`);
    expect(asked.at(-1)).toMatchObject({ folder: Q4, q: "press" });
    expect(asked.at(-1)).not.toHaveProperty("delimiter");
  });

  test("the trail names every folder above it, each a way back", () => {
    const labels = { [Q4]: "Q4 review", [folderUpKey(Q4, 0)]: MARKETING, [MARKETING]: "Marketing" };
    expect(crumbsFor(["artifacts", "folders", Q4], labels)).toEqual([
      { label: "Artifacts", path: ["artifacts"], icon: "layers" },
      { label: "Folders", path: ["artifacts", "folders"] },
      { label: "Marketing", path: ["artifacts", "folders", MARKETING] },
      { label: "Q4 review" },
    ]);
    // Until the page has said, never the folder's uuid.
    expect(crumbsFor(["artifacts", "folders", Q4]).at(-1)).toEqual({ label: "Folder" });
  });

  test("a folder the reader cannot see is not found, the same as one that does not exist", () => {
    mount(
      <ArtifactFolderPage id="99999999-9999-4999-8999-999999999999" />,
      "#/artifacts/folders/99999999-9999-4999-8999-999999999999",
    );
    expect(screen.getByText("Folder not found")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to folders" }).getAttribute("href")).toBe(
      "#/artifacts/folders",
    );
  });

  test("empty, it says so with ways to fill it to a reader who may, and claims nothing to one who may not", () => {
    contents = [];
    mount(<ArtifactFolderPage id={PRESS} />, `#/artifacts/folders/${PRESS}`);
    expect(screen.getByText("This folder is empty")).toBeTruthy();
    cleanup();
    tree = tree.map((f) => ({ ...f, role: "viewer" as const }));
    mount(<ArtifactFolderPage id={PRESS} />, `#/artifacts/folders/${PRESS}`);
    expect(screen.getByText("Nothing here is shared with you")).toBeTruthy();
    expect(screen.queryByText("This folder is empty")).toBeNull();
  });

  test("its owner's More offers Rename, Move to… and Remove; Remove says everything moves up and nothing is deleted", async () => {
    mount(<ArtifactFolderPage id={Q4} />, `#/artifacts/folders/${Q4}`);
    const header =
      screen.getByRole("heading", { level: 2, name: "Q4 review" }).closest("header") ??
      document.body;
    fireEvent.click(
      within(header as HTMLElement).getByRole("button", {
        name: "More for the folder “Q4 review”",
      }),
    );
    for (const name of ["Copy link", "Rename", "Move to…", "Remove"]) {
      expect(screen.getByRole("menuitem", { name })).toBeTruthy();
    }
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    const dialog = screen.getByRole("dialog", { name: "Remove “Q4 review”?" });
    expect(dialog.textContent).toContain(
      "Everything in it moves up to Marketing. Nothing is deleted, and nobody loses access.",
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove folder" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({ method: "DELETE", path: `/artifact-folders/${Q4}` });
  });

  test("a reader who only sees a folder is offered its link and nothing that changes it", () => {
    tree = tree.map((f) => ({ ...f, role: "contributor" as const }));
    mount(<ArtifactFolderPage id={Q4} />, `#/artifacts/folders/${Q4}`);
    fireEvent.click(screen.getAllByRole("button", { name: "More for the folder “Q4 review”" })[0]!);
    expect(screen.getByRole("menuitem", { name: "Copy link" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Remove" })).toBeNull();
  });
});

describe("Select", () => {
  // EACH PICK IS THE ENGINE'S TO DECIDE: a pick the reader may only view is
  // still sent (a folder's manager may take it to the top level), and one the
  // engine refuses is named in the move's toast while the rest move.
  test("Select draws a box on every card, and the bar's Move to… sends every pick, naming what did not move", async () => {
    contents = contents.map((item) => ({ ...item, folder: Q4 }));
    answer = () => ({
      kind: "done",
      pending: false,
      body: {
        results: [
          { artifact: "a1", outcome: "moved" },
          {
            artifact: "a2",
            outcome: "unauthorized",
            message:
              "moving it needs the editor role on the artifact; its owner can share it with you at that role",
          },
        ],
      },
    });
    mount(<ArtifactFolderPage id={Q4} />, `#/artifacts/folders/${Q4}`);
    fireEvent.click(screen.getByRole("button", { name: "Select" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Pricing FAQ" }));
    const bar = screen.getByRole("toolbar", { name: "Selection" });
    expect(bar.textContent).toContain("1 selected");
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Settlement pulse" }));
    expect(bar.textContent).toContain("2 selected");
    const move = within(bar).getByRole("button", { name: "Move to…" });
    expect(move.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(move);
    const dialog = await screen.findByRole("dialog", { name: /^Move/ });
    fireEvent.click(within(dialog).getByRole("option", { name: "Top level" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Move here" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({
      method: "POST",
      path: "/artifacts/folder-moves",
      body: {
        items: [
          { artifact: "a1", folder: "" },
          { artifact: "a2", folder: "" },
        ],
      },
    });
    expect(await screen.findByText("Moved 1 of 2 to the top level")).toBeTruthy();
    expect(
      screen.getByText(
        "“Settlement pulse” was not moved. Moving it needs the editor role on the artifact; its owner can share it with you at that role.",
      ),
    ).toBeTruthy();
  });

  test("the bar's Move to… is held, with its reason, only for a reader without the artifacts app", () => {
    mount(<ArtifactFolderPage id={Q4} />, `#/artifacts/folders/${Q4}`);
    fireEvent.click(screen.getByRole("button", { name: "Select" }));
    authoring = false;
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Pricing FAQ" }));
    const bar = screen.getByRole("toolbar", { name: "Selection" });
    expect(
      within(bar).getByRole("button", { name: "Move to…" }).getAttribute("aria-disabled"),
    ).toBe("true");
  });
});

describe("Move to…", () => {
  const index = () => indexFolders(tree);

  test("a phrase that names no folder offers to make it where the reader is, or where a path says", () => {
    expect(makingOf("Launch", index(), MARKETING)).toEqual({
      parent: expect.objectContaining({ id: MARKETING }),
      name: "Launch",
    });
    expect(makingOf("Marketing › Q4 review › Drafts", index(), "")).toEqual({
      parent: expect.objectContaining({ id: Q4 }),
      name: "Drafts",
    });
    expect(makingOf("Launch", index(), "")).toEqual({ parent: null, name: "Launch" });
    // A name already there, a parent nobody can find, and a name the engine refuses offer nothing.
    expect(makingOf("q4 review", index(), MARKETING)).toBeNull();
    expect(makingOf("Nowhere/Launch", index(), "")).toBeNull();
    expect(makingOf("a/", index(), "")).toEqual({ parent: null, name: "a" });
    expect(makingOf("..", index(), "")).toBeNull();
  });

  test("it lists the top level first and every path, marks where it is now, and files it with one request", async () => {
    const onMoved = vi.fn();
    mount(
      <MoveDialog
        subject={{ kind: "artifacts", items: [{ id: "a1", title: "Pricing FAQ", folder: Q4 }] }}
        onClose={vi.fn()}
        onMoved={onMoved}
      />,
      "#/artifacts",
    );
    const dialog = screen.getByRole("dialog", { name: "Move “Pricing FAQ”" });
    // ON A PHONE, THE WHOLE WINDOW: the kit's full-height sheet, so the list
    // is more than a few rows a screen.
    expect(dialog.closest(".crewlet-modal-overlay--phone-sheet")).not.toBeNull();
    const options = within(dialog).getAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([
      "Top level",
      "Board",
      "Marketing",
      "Marketing › Q4 reviewCurrent",
      "Marketing › Q4 review › Press kit",
    ]);
    expect(within(dialog).getByText("Moving never changes who can open it.")).toBeTruthy();
    fireEvent.click(options[4]!);
    fireEvent.click(within(dialog).getByRole("button", { name: "Move here" }));
    await waitFor(() => expect(onMoved).toHaveBeenCalled());
    expect(sent[0]).toMatchObject({
      method: "PUT",
      path: "/artifacts/a1/folder",
      body: { folder: PRESS },
    });
  });

  // THE LIST NEVER LEAVES THE DIALOG: it is always on screen, so a press on
  // Cancel while the field holds focus lands on Cancel and closes the
  // dialog, rather than on the veil after a list collapsing on blur shrank
  // the dialog under the pointer. Mutation: draw the list as a popup that
  // closes on blur and the list is gone once the field loses focus.
  test("the list stays on screen when the field loses focus, and Cancel closes at the first press", () => {
    const onClose = vi.fn();
    mount(
      <MoveDialog
        subject={{ kind: "artifacts", items: [{ id: "a1", title: "Pricing FAQ", folder: Q4 }] }}
        onClose={onClose}
        onMoved={vi.fn()}
      />,
      "#/artifacts",
    );
    const dialog = screen.getByRole("dialog", { name: "Move “Pricing FAQ”" });
    const field = within(dialog).getByRole("combobox", { name: "Find a folder" });
    expect(document.activeElement).toBe(field);
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    fireEvent.mouseDown(cancel);
    fireEvent.blur(field);
    expect(within(dialog).getByRole("listbox", { name: "Folders" })).toBeTruthy();
    expect(within(dialog).getAllByRole("option").length).toBeGreaterThan(0);
    fireEvent.mouseUp(cancel);
    fireEvent.click(cancel);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  test("a folder is never offered a place inside itself, and a move that widens asks first", async () => {
    answer = (req) =>
      (req.body as { confirm_widen?: boolean }).confirm_widen
        ? { kind: "done", pending: false, body: {} }
        : ({
            kind: "refused",
            text: "Moving it lets more people see it.",
            stale: false,
            error: { code: "folder_widens" },
          } as never);
    const onMoved = vi.fn();
    mount(
      <MoveDialog
        subject={{ kind: "folder", folder: tree[1]! }}
        onClose={vi.fn()}
        onMoved={onMoved}
      />,
      "#/artifacts",
    );
    const dialog = screen.getByRole("dialog", { name: "Move “Board”" });
    const paths = within(dialog)
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(paths).not.toContain("Board");
    fireEvent.click(within(dialog).getByRole("option", { name: "Marketing" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Move here" }));
    const widen = await screen.findByRole("dialog", { name: "Let more people see “Board”?" });
    // Every step keeps the one shape, so the dialog does not change size between them.
    expect(widen.closest(".crewlet-modal-overlay--phone-sheet")).not.toBeNull();
    expect(widen.textContent).toContain("Everyone at FairSplit");
    fireEvent.click(within(widen).getByRole("button", { name: "Move it" }));
    await waitFor(() => expect(onMoved).toHaveBeenCalled());
    expect(sent.map((r) => r.body)).toEqual([
      { parent: MARKETING },
      { parent: MARKETING, confirm_widen: true },
    ]);
  });
});
