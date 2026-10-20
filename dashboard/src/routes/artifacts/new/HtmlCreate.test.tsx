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
vi.mock("./html/DraftPreview.tsx", () => ({
  DraftPreview: ({ id, version }: { id: string; version: number }) => (
    <p>{`The draft ${id} at version ${version}`}</p>
  ),
}));

afterEach(cleanup);
beforeEach(() => {
  vi.mocked(keyedWrite).mockReset();
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
// may use; the bar then offers Update preview and Open page.
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
  expect(screen.getByText("Draft, version 1. Only you can see it")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Open page" }).length).toBeGreaterThan(0);
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
