/**
 * The Files panel: an artifact's asset store listed, a file added through an
 * asset upload and its add, a name already there left out, a refusal said in
 * the engine's words, adding held where the page cannot show a file, and a
 * file removed once the reader confirms.
 *
 * The store's list is stood in for by `useQuery`; every write goes through
 * the real REST client to a stubbed `fetch`, so the requests are the ones the
 * engine reads.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import type { ArtifactAsset, ArtifactDetail } from "~/protocol/index.ts";
import { ENGINE_HEADER, ENGINE_HEADER_VALUE } from "~/contract/engine.ts";
import { AssetsPanel, typeWords } from "./AssetsPanel.tsx";

vi.mock("~/lib/useQuery.ts", () => ({ useQuery: vi.fn() }));
const role = vi.hoisted(() => ({ can: true }));
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return {
    ...actual,
    useArtifactAccess: () =>
      role.can
        ? { can: true, as: "ada", acts: [] }
        : { can: false, reason: "This needs the “Editor” role on the artifact." },
  };
});

const ID = "a-1";
const LOGO = "11111111-1111-4111-8111-111111111111";

function asset(over: Partial<ArtifactAsset>): ArtifactAsset {
  return {
    artifact: ID,
    id: LOGO,
    name: "logo.png",
    content_type: "image/png",
    object: { hash: "h", size: 2048 },
    added_by: { name: "ada", kind: "human" },
    added_at: "2026-11-10T10:00:00Z",
    version: 1,
    ...over,
  } as ArtifactAsset;
}

const DETAIL = {
  id: ID,
  kind: "html",
  title: "Venue poll",
  head_number: 2,
  capabilities: ["assets", "db"],
  role: "owner",
  owner: "ada",
  status: "live",
  created_by: { name: "ada", kind: "human" },
  updated_by: { name: "ada", kind: "human" },
  created_at: "2026-11-10T10:00:00Z",
  updated_at: "2026-11-10T10:00:00Z",
} as unknown as ArtifactDetail;

let stored: ArtifactAsset[];
let sent: { method: string; url: string; body: unknown }[];
/** What the engine answers an add with, by the file's name: a refusal where one is set. */
let refuseAdd: Record<string, { status: number; body: Record<string, unknown> }>;
const refetch = vi.fn();

/** An answer as the engine writes it: JSON, and its mark, so a refusal is read as its own. */
function json(body: unknown, status: number) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", [ENGINE_HEADER]: ENGINE_HEADER_VALUE },
  });
}

beforeEach(() => {
  role.can = true;
  stored = [asset({})];
  sent = [];
  refuseAdd = {};
  refetch.mockReset();
  vi.mocked(useQuery).mockImplementation((() => ({
    data: { assets: stored },
    loading: false,
    error: null,
    refusal: null,
    refetch,
  })) as never);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const method = init.method ?? "GET";
      const path = new URL(url, "http://engine").pathname;
      const body =
        typeof init.body === "string" && init.body.startsWith("{") ? JSON.parse(init.body) : null;
      sent.push({ method, url: path, body });
      if (method === "POST" && path === `/artifacts/${ID}/uploads`) {
        const name = (body as { entries: { path: string }[] }).entries[0]!.path;
        return json(
          {
            upload: `u-${name}`,
            expires_at: "",
            transfers: [{ path: name, method: "PUT", url: "", direct: false }],
          },
          201,
        );
      }
      if (method === "POST" && path === `/artifacts/${ID}/assets`) {
        const name = (body as { name: string }).name;
        const refusal = refuseAdd[name];
        if (refusal) return json(refusal.body, refusal.status);
        return json({ asset: asset({ id: "new", name }), op_id: "k" }, 201);
      }
      return new Response(null, { status: method === "PUT" ? 204 : 200 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(detail: ArtifactDetail = DETAIL) {
  return render(
    <TestClient>
      <Router>
        <LayerHost>
          <ToastProvider>
            <AssetsPanel detail={detail} />
          </ToastProvider>
        </LayerHost>
      </Router>
    </TestClient>,
  );
}

/** Pick files with the panel's Add files, as the system picker hands them over. */
function pick(...files: File[]) {
  const input = document.querySelector<HTMLInputElement>('input[type="file"]');
  if (!input) throw new Error("no file input");
  fireEvent.change(input, { target: { files } });
}

test("the store is listed by name, what each file is and how large", () => {
  mount();
  const list = screen.getByRole("list", { name: "Files its page shows" });
  expect(within(list).getByText("logo.png")).toBeTruthy();
  expect(list.textContent).toContain("PNG image");
  expect(list.textContent).toContain("2.0 KiB");
  expect(screen.getByText("1 file, 2.0 KiB")).toBeTruthy();
});

test("a file's type is named in words, or by its extension where the type has none", () => {
  expect(typeWords("text/csv; charset=utf-8", "venues.csv")).toBe("CSV file");
  expect(typeWords("application/octet-stream", "map.geojson")).toBe("GEOJSON file");
  expect(typeWords("", "README")).toBe("File");
});

// Mutation: send the upload without `asset: true` and the engine decides it
// as a publish, which an asset never is; drop the add and nothing lands.
test("Add files sends each new file as an asset upload, then its add, and leaves a name already there out", async () => {
  mount();
  pick(
    new File(["a,b\n"], "venues.csv", { type: "text/csv" }),
    new File([new Uint8Array([1])], "logo.png", { type: "image/png" }),
  );
  await waitFor(() => expect(refetch).toHaveBeenCalled());
  const intent = sent.find((r) => r.url === `/artifacts/${ID}/uploads`)!;
  expect(intent.body).toMatchObject({ asset: true, entries: [{ path: "venues.csv", size: 4 }] });
  expect(sent.some((r) => r.method === "PUT" && r.url.endsWith("/files/venues.csv"))).toBe(true);
  expect(sent.find((r) => r.url === `/artifacts/${ID}/assets`)!.body).toEqual({
    upload: "u-venues.csv",
    name: "venues.csv",
  });
  // logo.png was already in the store: not sent, and said so.
  expect(sent.filter((r) => r.url === `/artifacts/${ID}/uploads`)).toHaveLength(1);
  expect(screen.getByText(/logo\.png is already here/)).toBeTruthy();
  expect(await screen.findByText("Added venues.csv")).toBeTruthy();
});

test("a refused add is named with the engine's own sentence, and the rest land", async () => {
  refuseAdd["huge.png"] = {
    status: 422,
    body: { error: "invalid", detail: "asset huge.png is too large for the asset store" },
  };
  mount();
  pick(
    new File(["x"], "huge.png", { type: "image/png" }),
    new File(["{}"], "data.json", { type: "application/json" }),
  );
  await screen.findByText(/A file was not added/);
  expect(document.body.textContent).toMatch(
    /huge\.png: Asset huge\.png is too large for the asset store/,
  );
  expect(await screen.findByText("Added data.json")).toBeTruthy();
});

// Mutation: drop the capability check in `addHold` and a file is offered to
// a page that can never show it.
test("adding is held, with why, while the latest version does not ask for added files", () => {
  mount({ ...DETAIL, capabilities: ["db"] } as ArtifactDetail);
  const add = screen.getByRole("button", { name: "Add files" });
  expect(add.getAttribute("aria-disabled")).toBe("true");
  expect(screen.getByText("Its page does not show added files yet")).toBeTruthy();
  // What the store holds stays listed, and removable.
  expect(
    screen.getByRole("button", { name: "Remove logo.png" }).getAttribute("aria-disabled"),
  ).not.toBe("true");
});

test("a reader below editor sees the store and may neither add nor remove", () => {
  role.can = false;
  mount();
  expect(screen.getByRole("button", { name: "Add files" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
  expect(
    screen.getByRole("button", { name: "Remove logo.png" }).getAttribute("aria-disabled"),
  ).toBe("true");
  expect(screen.queryByText("Its page does not show added files yet")).toBeNull();
});

test("Remove asks first, then removes the one asset by its id", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Remove logo.png" }));
  const ask = screen.getByRole("alertdialog", { name: "Remove logo.png?" });
  expect(ask.textContent).toContain("Its page stops showing it at once.");
  fireEvent.click(within(ask).getByRole("button", { name: "Remove" }));
  await waitFor(() =>
    expect(sent).toContainEqual(
      expect.objectContaining({ method: "DELETE", url: `/artifacts/${ID}/assets/${LOGO}` }),
    ),
  );
  expect(await screen.findByText("Removed logo.png")).toBeTruthy();
  expect(refetch).toHaveBeenCalled();
});

test("an empty store says what it is for", () => {
  stored = [];
  mount();
  expect(screen.getByText("No files yet")).toBeTruthy();
  expect(screen.queryByRole("list", { name: "Files its page shows" })).toBeNull();
});
