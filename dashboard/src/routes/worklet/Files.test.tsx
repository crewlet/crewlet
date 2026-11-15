/**
 * A project's files: what is kept, a page at a time, and a download fetched
 * over the session rather than a bare link, so its refusal is the engine's.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { fileKind, fileName, ProjectFiles, tooLarge, uploadRequest } from "./Files.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { rest, RestError } from "~/protocol/rest.ts";
import type { WorkFilesAnswer } from "~/protocol/index.ts";

const access = vi.hoisted(() => ({ can: true }));
vi.mock("~/lib/useWriteAccess.ts", () => ({
  useWriteAccess: () =>
    access.can ? { can: true } : { can: false, reason: "This needs Worklet access." },
}));

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  access.can = true;
});

function serving(pages: Record<string, WorkFilesAnswer>) {
  const query = vi.fn(async (_what: string, params: Record<string, unknown>) => {
    return pages[String(params.after ?? "")] ?? { project: "ENG", files: [], complete: true };
  });
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  return query;
}

const file = (path: string) => ({
  project: "ENG",
  path,
  content_type: "text/markdown",
  size: 2048,
  version: 7,
  created_at: "2026-09-01T10:00:00Z",
  updated_at: "2026-09-01T10:00:00Z",
  updated_by: "ada",
});

const mount = () =>
  render(
    <Router>
      <ProjectFiles project="ENG" />
    </Router>,
  );

test("an empty project says so rather than drawing an empty table", async () => {
  serving({});
  mount();
  expect(await screen.findByText("No files are kept in this project yet")).toBeTruthy();
});

test("a page of files is listed, and the next page is the engine's own cursor", async () => {
  const query = serving({
    "": {
      project: "ENG",
      files: [file("a.md"), file("reports/b.md")],
      next: "reports/b.md",
      complete: true,
    },
    "reports/b.md": { project: "ENG", files: [file("zeta.txt")], complete: true },
  });
  mount();
  expect(await screen.findByText("reports/b.md")).toBeTruthy();
  fireEvent.click(screen.getByText("Next page"));
  expect(await screen.findByText("zeta.txt")).toBeTruthy();
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>][];
  expect(calls.at(-1)?.[1]).toEqual({ project: "ENG", after: "reports/b.md" });
  fireEvent.click(screen.getByText("First page"));
  expect(await screen.findByText("a.md")).toBeTruthy();
});

test("a download fetches the bytes over the session, and a refusal is said on the row", async () => {
  serving({ "": { project: "ENG", files: [file("reports/q3 plan.md")], complete: true } });
  const blob = vi.spyOn(rest, "blob").mockRejectedValueOnce(
    new RestError(503, {
      error: "content_unavailable",
      detail: "the object store did not answer",
    }),
  );
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /^Download / }));
  await waitFor(() =>
    expect(screen.getByText(/Not downloaded: The object store did not answer\./)).toBeTruthy(),
  );
  expect(blob).toHaveBeenCalledWith("/worklet/files/ENG/reports/q3%20plan.md");
});

// A REFUSAL ON AUTHORITY NAMES THE GRANT the engine said would admit the
// reader, rather than its code: "unauthorized" on the row tells a person
// nothing they can act on.
test("a download refused on authority says which grant would admit the reader", async () => {
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  vi.spyOn(rest, "blob").mockRejectedValueOnce(
    new RestError(403, { error: "unauthorized", reason: "no_grant", grants: ["worklet"] }),
  );
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /^Download / }));
  await waitFor(() =>
    expect(
      screen.getByText(
        /Not downloaded: Downloading a project's file needs Worklet access\. An admin can give it to you\./,
      ),
    ).toBeTruthy(),
  );
});

// A REFUSAL NO ACCESS CLEARS IS THE ENGINE'S OWN WORDS, whatever it names: a
// proof gone stale names the access that admitted the reader. Mutation: decide
// by the grants alone and this names Admin.
test("a download refused for a stale proof says the engine's words, never a grant", async () => {
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  vi.spyOn(rest, "blob").mockRejectedValueOnce(
    new RestError(403, {
      error: "step_up_required",
      message: "Confirm it's you to do this, then try again.",
      reason: "step_up",
      grants: ["admin"],
    }),
  );
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /^Download / }));
  await waitFor(() =>
    expect(
      screen.getByText(/Not downloaded: Confirm it's you to do this, then try again\./),
    ).toBeTruthy(),
  );
  expect(screen.queryByText(/needs Admin access/)).toBeNull();
});

test("a file is saved under its own name", () => {
  expect(fileName("reports/2026/q3.md")).toBe("q3.md");
  expect(fileName("notes.txt")).toBe("notes.txt");
});

// A RAW TYPE IS THE RECORD, NOT A DESCRIPTION. `text/plain; charset=utf-8` ran
// after the path, off the card on a phone: the kind is a word in a column of its
// own that gives way, and the stored type stays on its title.
test("a file's kind is a word a reader uses, whatever its parameters", () => {
  expect(fileKind("text/markdown; charset=utf-8")).toBe("Markdown");
  expect(fileKind("application/json")).toBe("JSON");
  expect(fileKind("text/x-yaml")).toBe("Text");
  expect(fileKind("image/png")).toBe("Image");
  expect(fileKind("application/x-tar")).toBe("TAR");
  expect(fileKind("")).toBe("");
  expect(fileKind(undefined)).toBe("");
});

test("the listing is the kit's grid: named heads, figures on the right, the raw type on hover", async () => {
  serving({
    "": {
      project: "ENG",
      files: [{ ...file("docs/a.md"), content_type: "text/markdown; charset=utf-8" }],
      complete: true,
    },
  });
  const { container } = mount();
  await screen.findByText("docs/a.md");
  const heads = [...container.querySelectorAll('[role="columnheader"]')].map((h) => h.textContent);
  expect(heads).toEqual(["Path", "Kind", "Size", "By", "Changed", "Download", "Replace", "Remove"]);
  const kind = screen.getByText("Markdown").closest('[role="cell"]');
  expect(kind?.getAttribute("title")).toBe("text/markdown; charset=utf-8");
  // A FIGURE AND A TIME END ON ONE EDGE.
  for (const name of ["Size", "Changed"]) {
    const at = heads.indexOf(name);
    const cell = container.querySelectorAll('.crewlet-data-grid__row [role="cell"]')[at];
    expect(cell?.className).toContain("crewlet-data-grid__cell--right");
  }
});

// ---------------------------------------------------------------------------
// Upload, replace, remove
// ---------------------------------------------------------------------------

/** The byte route answering a write, as the engine does: landed, at a position. */
function writes() {
  return vi.spyOn(rest, "request").mockResolvedValue({
    status: 200,
    body: { outcome: "applied", position: "CREWLET_TRACKER_LOG@1:4", version: 8 },
  } as never);
}

/** The file input a FilePickButton labelled `name` opens. */
function pickerOf(name: string): HTMLInputElement {
  const button = screen.getByRole("button", { name });
  const input = button.parentElement?.querySelector('input[type="file"]');
  if (!input) throw new Error(`no file input beside ${name}`);
  return input as HTMLInputElement;
}

// AN UPLOAD IS THE FILE'S BYTES, under its own type, keyed so a retry is the
// same write, and naming no version: a name the project already keeps is
// refused rather than replaced. Mutation: send the file as JSON and the body is
// no longer the file.
test("an upload puts each picked file's bytes at its name, keyed, with no version", async () => {
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  const request = writes();
  mount();
  await screen.findByText("a.md");
  const picked = new File(['{"q": 3}'], "numbers.json", { type: "application/json" });
  fireEvent.change(pickerOf("Upload"), { target: { files: [picked] } });
  await waitFor(() => expect(request).toHaveBeenCalledTimes(1));
  const [method, path, options] = request.mock.calls[0]! as [string, string, Record<string, any>];
  expect(method).toBe("PUT");
  expect(path).toBe("/worklet/files/ENG/numbers.json");
  expect(options.body).toBe(picked);
  expect(options.contentType).toBe("application/json");
  expect(options.headers["Idempotency-Key"]).toMatch(/\S/);
  expect(options.headers["If-Match"]).toBeUndefined();
});

// A REFUSED UPLOAD IS NAMED IN THE ENGINE'S WORDS, beside the files that landed:
// here a name the project already keeps, which an upload never replaces.
test("an upload the engine refuses is said, naming the file", async () => {
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  // THE ENGINE'S OWN REFUSAL, marked as its answer, of a name already kept.
  vi.spyOn(rest, "request").mockRejectedValueOnce(
    new RestError(
      412,
      {
        error: "version_moved",
        detail: "file a.md in ENG is at version 7 and this write was conditioned on 0",
      },
      null,
      true,
    ),
  );
  mount();
  await screen.findByText("a.md");
  fireEvent.change(pickerOf("Upload"), {
    target: { files: [new File(["x"], "a.md", { type: "text/markdown" })] },
  });
  await waitFor(() =>
    expect(screen.getByRole("alert").textContent).toMatch(/^Not uploadeda\.md: .*version 7/i),
  );
});

// REPLACE AND REMOVE NAME THE VERSION ON SCREEN, so a file somebody rewrote
// since the listing was read is refused rather than overwritten unseen.
test("a row's Replace and Remove name the version on screen", async () => {
  serving({ "": { project: "ENG", files: [file("reports/q3.md")], complete: true } });
  const request = writes();
  mount();
  await screen.findByText("reports/q3.md");
  fireEvent.change(pickerOf("Replace reports/q3.md"), {
    target: { files: [new File(["new"], "q3.md", { type: "text/markdown" })] },
  });
  await waitFor(() => expect(request).toHaveBeenCalledTimes(1));
  let [method, path, options] = request.mock.calls[0]! as [string, string, Record<string, any>];
  expect([method, path, options.headers["If-Match"]]).toEqual([
    "PUT",
    "/worklet/files/ENG/reports/q3.md",
    '"7"',
  ]);

  fireEvent.click(screen.getByRole("button", { name: "Remove reports/q3.md" }));
  const confirm = await screen.findByRole("alertdialog");
  fireEvent.click(within(confirm).getByRole("button", { name: "Remove file" }));
  await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
  [method, path, options] = request.mock.calls[1]! as [string, string, Record<string, any>];
  expect([method, path, options.headers["If-Match"]]).toEqual([
    "DELETE",
    "/worklet/files/ENG/reports/q3.md",
    '"7"',
  ]);
});

test("a reader without Worklet access is told why Upload and Remove are held", async () => {
  access.can = false;
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  mount();
  await screen.findByText("a.md");
  expect(screen.getByRole("button", { name: "Upload" }).getAttribute("aria-disabled")).toBe("true");
  expect(screen.getByRole("button", { name: "Remove a.md" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
});

test("a file past the engine's cap is refused before its bytes are sent", () => {
  expect(tooLarge({ name: "dump.bin", size: 2 * 1024 ** 3 })).toMatch(/dump\.bin is .*at most/);
  expect(tooLarge({ name: "a.md", size: 10 })).toBeNull();
  const blob = new Blob(["x"]);
  expect(uploadRequest("ENG", "a b.md", blob, 3)).toMatchObject({
    method: "PUT",
    path: "/worklet/files/ENG/a%20b.md",
    contentType: "application/octet-stream",
    headers: { "If-Match": '"3"' },
  });
});
