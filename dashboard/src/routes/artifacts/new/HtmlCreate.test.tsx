import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { RestError } from "~/protocol/index.ts";
import { keyedWrite } from "~/lib/keyedWrite.ts";
import { checkPage, uploadFiles } from "~/lib/artifacts/api.ts";
import { HtmlCreate } from "./HtmlCreate.tsx";

vi.mock("~/lib/artifacts/access.ts", () => ({ useArtifactAccess: () => ({ can: true }) }));
vi.mock("~/lib/keyedWrite.ts", () => ({ keyedWrite: vi.fn() }));
vi.mock("~/lib/artifacts/api.ts", async (original) => ({
  ...(await original<typeof import("~/lib/artifacts/api.ts")>()),
  uploadFiles: vi.fn(),
  checkPage: vi.fn(),
}));
const fileArtifactTask = vi.fn();
vi.mock("./WhoBuildsIt.tsx", () => ({
  useWhoBuildsIt: () => ({
    builders: [{ seat: { handle: "pixel", name: "Pixel" }, suggested: true }],
    seat: "pixel",
    setSeat: () => {},
    project: "CRW",
    setProject: () => {},
    projects: [{ key: "CRW", name: "Crewlet" }],
    projectsLoading: false,
  }),
  WhoBuildsIt: () => null,
}));
vi.mock("./fileTask.ts", async (original) => ({
  ...(await original<typeof import("./fileTask.ts")>()),
  useFileArtifactTask: () => ({ fileArtifactTask, busy: false, refusal: null, held: null }),
}));
vi.mock("./html/DraftPreview.tsx", () => ({
  DraftPreview: ({ id, version }: { id: string; version: number }) => (
    <p>{`The draft ${id} at version ${version}`}</p>
  ),
}));

afterEach(cleanup);
beforeEach(() => {
  vi.mocked(keyedWrite).mockReset();
  fileArtifactTask.mockReset().mockResolvedValue({ item: "CRW-9", key: "CRW-9", linked: null });
  vi.mocked(uploadFiles)
    .mockReset()
    .mockResolvedValue({
      upload: "u-1",
      files: [{ path: "index.html", upload: true }],
    });
  vi.mocked(checkPage).mockReset().mockResolvedValue({ refusals: [], warnings: [] });
});

function mount(hash: string) {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <HtmlCreate />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
}

const page = (body: string) =>
  `<!doctype html>\n<html>\n<head>\n<title>Venue poll</title>\n${body}\n</head>\n</html>`;

// A REFUSED PREVIEW SAVES NOTHING and lists every finding with its line, each
// taking the editor there; the page's title names it.
test("a refused preview lists the linter's findings and takes the editor to one", async () => {
  vi.mocked(keyedWrite).mockResolvedValue({
    kind: "refused",
    text: "Refused",
    stale: false,
    error: new RestError(422, {
      error: "policy_refused",
      findings: [
        {
          path: "index.html",
          line: 5,
          code: "remote_script",
          message: "the script loads another origin",
        },
      ],
    }),
  });
  mount("#/artifacts/new/html?mode=source");
  const source = screen.getByRole("textbox", {
    name: "Source of index.html",
  }) as HTMLTextAreaElement;
  fireEvent.change(source, {
    target: { value: page('<script src="https://x.example/a.js"></script>') },
  });
  expect((screen.getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe(
    "Venue poll",
  );
  await act(async () => {
    fireEvent.click(screen.getAllByRole("button", { name: "Preview privately" })[0]!);
  });
  await screen.findByText(
    "The preview was refused, so nothing was saved. Fix these and preview again.",
  );
  const finding = screen.getByRole("button", { name: /index\.html, line 5/ });
  expect(finding.textContent).toContain("The script loads another origin.");
  fireEvent.click(finding);
  expect(document.activeElement).toBe(source);
  expect(source.value.slice(0, source.selectionStart).split("\n")).toHaveLength(5);
  expect(vi.mocked(uploadFiles).mock.calls[0]![0]).toBeNull();
});

// THE FIRST PREVIEW MAKES THE DRAFT, private, as an HTML page with what it
// may use; the bar then offers Update preview and Open page. Command+Enter
// previews from the editor only, so the editor says so and the bar's Update
// preview claims no key.
test("a landed preview makes a private draft and frames it", async () => {
  vi.mocked(keyedWrite).mockResolvedValue({
    kind: "done",
    pending: false,
    body: { artifact: "a-1", number: 1, warnings: [] },
  });
  mount("#/artifacts/new/html?mode=source");
  const source = screen.getByRole("textbox", { name: "Source of index.html" });
  fireEvent.change(source, {
    target: { value: page("<script>crewlet.data.collection('v')</script>") },
  });
  await act(async () => {
    fireEvent.keyDown(source, { key: "Enter", metaKey: true });
  });
  await screen.findByText("The draft a-1 at version 1");
  const sent = vi.mocked(keyedWrite).mock.calls[0]![0];
  expect(sent).toMatchObject({
    method: "POST",
    path: "/artifacts",
    body: { kind: "html", title: "Venue poll", entry: "index.html", capabilities: ["db"] },
  });
  expect(screen.getByText("Private draft, v1")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Open page" }).length).toBeGreaterThan(0);
  expect(document.querySelector(".html-create-editor-keys")?.textContent).toMatch(/to preview$/);
  for (const update of screen.getAllByRole("button", { name: "Update preview" })) {
    expect(update.getAttribute("aria-keyshortcuts")).toBeNull();
  }
});

// DESCRIBE IT shows the task exactly as it will be filed, and a starting
// point fills an empty brief without overwriting words.
test("a starting point fills an empty brief and the task is shown as it is sent", () => {
  mount("#/artifacts/new/html");
  fireEvent.click(screen.getByRole("button", { name: /Poll/ }));
  const brief = screen.getByRole("textbox", { name: "Describe the page" }) as HTMLTextAreaElement;
  expect(brief.value).toContain("A poll asking");
  expect(screen.getByText(/the `db` capability/)).toBeTruthy();
  fireEvent.change(brief, { target: { value: "My own words" } });
  fireEvent.click(screen.getByRole("button", { name: /Tracker/ }));
  expect(brief.value).toBe("My own words");
  expect(screen.getByText(/the `presence` capability/)).toBeTruthy();
});

// A PREVIEW THAT FAILS FOR ANY OTHER REASON IS SAID IN PREVIEW, where its
// callout is: flipped to Checks, the reader was told the page had no problems.
test("a preview that failed, not refused, stays on Preview and says why", async () => {
  vi.mocked(keyedWrite).mockResolvedValue({
    kind: "refused",
    text: "The engine could not save the draft.",
    stale: false,
    error: new RestError(503, { error: "unavailable" }),
  });
  mount("#/artifacts/new/html?mode=source");
  await act(async () => {
    fireEvent.click(screen.getAllByRole("button", { name: "Preview privately" })[0]!);
  });
  await screen.findByText("The engine could not save the draft.");
  expect(screen.getByRole("tab", { name: "Preview" }).getAttribute("aria-selected")).toBe("true");
  expect(screen.getByText("The preview was not saved")).toBeTruthy();
});

// LEAVING ASKS AS EVERY CREATE PAGE ASKS: unsaved words are discarded or kept
// in the same prompt, worded alike.
test("leaving with unsaved words asks to discard them, as every create page does", () => {
  mount("#/artifacts/new/html?mode=source");
  fireEvent.change(screen.getByRole("textbox", { name: "Source of index.html" }), {
    target: { value: page("") },
  });
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  const dialog = screen.getByRole("alertdialog", { name: "Discard this page?" });
  expect(dialog.textContent).toContain("What you wrote is not saved anywhere.");
  fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
  expect(screen.queryByRole("alertdialog")).toBeNull();
});

test("the checks speak while the page is written", async () => {
  vi.useFakeTimers();
  try {
    vi.mocked(checkPage).mockResolvedValue({
      refusals: [
        { path: "index.html", line: 2, code: "javascript_url", message: "a javascript: URL" },
      ],
      warnings: [],
    });
    mount("#/artifacts/new/html?mode=source");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    expect(checkPage).toHaveBeenCalled();
    fireEvent.click(screen.getByRole("tab", { name: /Checks/ }));
    expect(screen.getByRole("button", { name: /index\.html, line 2/ })).toBeTruthy();
    const preview = screen.getAllByRole("button", { name: "Preview privately" })[0]!;
    expect(preview.getAttribute("aria-disabled")).toBe("true");
  } finally {
    vi.useRealTimers();
  }
});

// ASKING FILES ONE REAL TASK to the chosen agent, carrying the brief exactly
// as the side panel showed it, and lands on that task.
test("asking files the task to the chosen agent and opens it", async () => {
  mount("#/artifacts/new/html");
  const brief = screen.getByRole("textbox", { name: "Describe the page" });
  fireEvent.change(brief, { target: { value: "A poll for the offsite venue." } });
  fireEvent.click(screen.getByRole("checkbox", { name: /Saves what people enter/ }));
  const shown = document.querySelector(".html-create-sent-body")!.textContent;
  expect(screen.getByText("Filed to Pixel in Crewlet.")).toBeTruthy();
  await act(async () => {
    fireEvent.click(screen.getAllByRole("button", { name: "Ask Pixel" })[0]!);
  });
  const request = fileArtifactTask.mock.calls[0]![0];
  expect(request).toMatchObject({
    kind: "html",
    seat: "pixel",
    project: "CRW",
    title: "Build an HTML page: A poll for the offsite venue.",
  });
  expect(shown).toContain(request.brief);
  expect(shown).toContain("(kind `html`)");
  expect(location.hash).toBe("#/work/CRW-9");
});
