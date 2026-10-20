/**
 * New doc: the page opens on the title, templates fill the body without
 * gating it, and Create sends the doc as written in one keyed write.
 */

import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { DOC_TEMPLATES } from "~/lib/artifacts/docTemplates.ts";
import type { KeyedAnswer, KeyedRequest } from "~/lib/keyedWrite.ts";
import { DocCreate } from "./DocCreate.tsx";

const write = vi.fn<(req: KeyedRequest) => Promise<KeyedAnswer>>();

vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return {
    ...actual,
    useKeyedGesture: () => actual.useKeyedGesture((req) => write(req)),
  };
});
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});

beforeEach(() => {
  write.mockReset();
  write.mockResolvedValue({ kind: "done", pending: false, body: { artifact: "doc-1" } });
});
afterEach(cleanup);

function mount(hash = "#/artifacts/new/doc") {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <DocCreate />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
}

const title = () => screen.getByRole("textbox", { name: "Title" });
const create = () => screen.getByRole("button", { name: "Create doc" });
const sent = () => write.mock.calls[0]![0].body as Record<string, unknown>;

// THE PAGE OPENS ON THE TITLE, with the templates beside the empty body and
// Create held until there is a title, saying why.
test("opens on the title, offers the templates and holds Create for a title", () => {
  mount();
  expect(document.activeElement).toBe(title());
  const starts = screen.getByRole("radiogroup", { name: "Start from a template" });
  expect([...starts.querySelectorAll('[role="radio"]')].map((r) => r.textContent)).toEqual(
    DOC_TEMPLATES.map((t) => t.label),
  );
  expect(create().getAttribute("aria-disabled")).toBe("true");
  expect(screen.getByText("Private to you until you share it")).toBeTruthy();
});

// A BLANK BODY BEGINS FROM THE STARTER: no doc plan is sent, and the page
// opens the new doc.
test("Create with a blank body begins the doc from its starter and opens it", async () => {
  mount();
  fireEvent.change(title(), { target: { value: "  On-call handbook " } });
  fireEvent.click(create());
  await waitFor(() => expect(location.hash).toBe("#/artifacts/doc-1"));
  expect(write).toHaveBeenCalledOnce();
  expect(sent()).toEqual({ kind: "doc", title: "On-call handbook", starter: true });
  expect(write.mock.calls[0]![0].key).toBeTruthy();
});

// A TEMPLATE FILLS THE BODY, and Create sends the body as written under the
// title, for the task the page was opened for.
test("a template fills the body, and Create sends it as the doc's text", async () => {
  mount("#/artifacts/new/doc?item=CRW-7");
  expect(screen.getByText(/listed on CRW-7/)).toBeTruthy();
  fireEvent.click(screen.getByRole("radio", { name: "Runbook" }));
  expect(screen.getByRole("radio", { name: "Runbook" }).getAttribute("aria-checked")).toBe("true");
  fireEvent.change(title(), { target: { value: "Database failover" } });
  fireEvent.keyDown(title(), { key: "Enter", metaKey: true });
  await waitFor(() => expect(write).toHaveBeenCalledOnce());
  const runbook = DOC_TEMPLATES.find((t) => t.key === "runbook")!;
  expect(sent()).toEqual({
    kind: "doc",
    title: "Database failover",
    starter: true,
    work_item: "CRW-7",
    doc: { body: runbook.body },
  });
});

// THE ADDRESS MAY NAME A TEMPLATE, as the gallery's starting points do; a
// second press of the chosen one clears the page again.
test("template= opens on that template, and pressing it again clears it", () => {
  mount("#/artifacts/new/doc?template=decision");
  const chip = screen.getByRole("radio", { name: "Decision record" });
  expect(chip.getAttribute("aria-checked")).toBe("true");
  expect(title().getAttribute("placeholder")).toBe("Move session storage to Postgres");
  fireEvent.click(chip);
  expect(chip.getAttribute("aria-checked")).toBe("false");
  expect(title().getAttribute("placeholder")).toBe("Untitled doc");
});

// THE ENGINE'S REFUSAL is said at the top of the page, and the page keeps
// everything typed.
test("a refusal is said above the title and nothing is lost", async () => {
  write.mockResolvedValue({ kind: "refused", text: "The title is taken.", stale: false });
  mount();
  fireEvent.change(title(), { target: { value: "Plan" } });
  fireEvent.click(create());
  const alert = (await screen.findByText("The doc was not created")).closest('[role="alert"]');
  expect(alert?.textContent).toContain("The title is taken.");
  expect(location.hash).toBe("#/artifacts/new/doc");
  expect((title() as HTMLInputElement).value).toBe("Plan");
});

// A TITLE PAST THE ENGINE'S BOUND is said on the field, and holds Create.
test("a title past its bound is marked and holds Create", () => {
  mount();
  fireEvent.change(title(), { target: { value: "x".repeat(400) } });
  expect(title().getAttribute("aria-invalid")).toBe("true");
  expect(create().getAttribute("aria-disabled")).toBe("true");
});

// LEAVING WITH ANYTHING WRITTEN ASKS FIRST; leaving an untouched page does not.
test("Cancel asks before discarding a title, and leaves at once when nothing is written", async () => {
  mount();
  fireEvent.change(title(), { target: { value: "Draft" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  const dialog = await screen.findByRole("alertdialog", { name: "Discard this doc?" });
  expect(location.hash).toBe("#/artifacts/new/doc");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  });
  expect(dialog.isConnected).toBe(false);
  expect(location.hash).toBe("#/artifacts");
  cleanup();
  mount();
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(location.hash).toBe("#/artifacts");
});
