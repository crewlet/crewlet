/**
 * New doc: the page opens on the title, templates fill the body without
 * gating it, and Create sends the doc as written in one keyed write.
 */

import { TestClient } from "~/test/client.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { DOC_TEMPLATES } from "~/lib/artifacts/docTemplates.ts";
import type { KeyedAnswer, KeyedRequest } from "~/lib/keyedWrite.ts";
import { DocCreate, docBrief } from "./DocCreate.tsx";
import type { ArtifactTaskRequest, FiledArtifactTask } from "./fileTask.ts";
import { context, draftField, openFrame, setDraft } from "../draftTesting.ts";

const write = vi.fn<(req: KeyedRequest) => Promise<KeyedAnswer>>();
const fileTask = vi.fn<(req: ArtifactTaskRequest) => Promise<FiledArtifactTask | null>>();

vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return {
    ...actual,
    useKeyedGesture: () => actual.useKeyedGesture((req) => write(req)),
  };
});
vi.mock("./fileTask.ts", async () => {
  const actual = await vi.importActual<typeof import("./fileTask.ts")>("./fileTask.ts");
  return {
    ...actual,
    useFileArtifactTask: () => ({
      fileArtifactTask: fileTask,
      busy: false,
      refusal: null,
      held: null,
    }),
  };
});
vi.mock("./WhoBuildsIt.tsx", () => ({
  useWhoBuildsIt: () => ({
    builders: [{ seat: { handle: "sre-lead", name: "SRE Lead" }, suggested: true }],
    seat: "sre-lead",
    setSeat: () => {},
    project: "ENG",
    setProject: () => {},
    projects: [],
    projectsLoading: false,
  }),
  WhoBuildsIt: () => <p>Who builds it</p>,
}));
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});

beforeEach(() => {
  fileTask.mockReset();
  fileTask.mockResolvedValue({ item: "ENG-2", key: "ENG-2", linked: null });
  write.mockReset();
  write.mockResolvedValue({ kind: "done", pending: false, body: { artifact: "doc-1" } });
});
afterEach(cleanup);

function mount(hash = "#/artifacts/new/doc") {
  location.hash = hash;
  return render(
    <TestClient>
      <Router>
        <LayerHost>
          <ToastProvider>
            <DocCreate />
          </ToastProvider>
        </LayerHost>
      </Router>
    </TestClient>,
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
  // The way back reads as every create page's does.
  expect(within(dialog).getByRole("button", { name: "Keep editing" })).toBeTruthy();
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

// ASK AN AGENT opens the hand-off panel beside the page, which stays; the
// send files one task to the agent with the brief and lands on it, and
// nothing is created here.
test("Ask an agent opens the hand-off beside the page and files the brief as a task", async () => {
  mount();
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  expect(location.hash).toContain("panel=ask");
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  // THE PAGE STAYS: the title and the templates are still there beside it.
  expect(title()).toBeTruthy();
  expect(screen.getByRole("radio", { name: "Runbook" })).toBeTruthy();
  expect(within(panel).getByText("What happens next")).toBeTruthy();
  const ask = () => within(panel).getByRole("button", { name: "Ask SRE Lead" });
  fireEvent.change(title(), { target: { value: "Incident review guide" } });
  expect(ask().getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(within(panel).getByRole("textbox", { name: "What should the doc cover?" }), {
    target: { value: "  How we run incident reviews. " },
  });
  fireEvent.click(ask());
  await waitFor(() => expect(location.hash).toBe("#/worklet/ENG-2"));
  expect(fileTask).toHaveBeenCalledWith({
    kind: "doc",
    title: "Incident review guide",
    brief: "How we run incident reviews.",
    seat: "sre-lead",
    project: "ENG",
  });
  expect(write).not.toHaveBeenCalled();
});

// A LINK OPENS THE PANEL: the address carries it, as an open artifact's panels.
test("an address naming the panel opens the page with it open, and Close closes it", () => {
  mount("#/artifacts/new/doc?panel=ask");
  fireEvent.click(screen.getByRole("button", { name: "Close ask an agent" }));
  expect(screen.queryByRole("complementary", { name: "Ask an agent" })).toBeNull();
  expect(location.hash).not.toContain("panel=");
});

// WHAT IS ON THE PAGE GOES WITH THE BRIEF: a template chosen on the page
// reaches the agent as the outline to follow, and the panel lists it.
test("a template on the page goes to the agent as the outline to follow", async () => {
  mount();
  fireEvent.change(title(), { target: { value: "Database failover" } });
  fireEvent.click(screen.getByRole("radio", { name: "Runbook" }));
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  expect(within(panel).getByText("The runbook outline")).toBeTruthy();
  expect(within(panel).getByText("“Database failover”")).toBeTruthy();
  fireEvent.change(within(panel).getByRole("textbox", { name: "What should the doc cover?" }), {
    target: { value: "For the on-call rota." },
  });
  fireEvent.click(within(panel).getByRole("button", { name: "Ask SRE Lead" }));
  await waitFor(() => expect(fileTask).toHaveBeenCalledOnce());
  const runbook = DOC_TEMPLATES.find((t) => t.key === "runbook")!;
  expect(fileTask.mock.calls[0]![0].brief).toBe(
    `For the on-call rota.\n\nFollow the runbook outline:\n\n${runbook.body.trim()}`,
  );
});

// COMMAND+ENTER IN THE PANEL SENDS, and never creates the doc behind it.
test("Command Enter in the panel asks the agent rather than creating the doc", async () => {
  mount("#/artifacts/new/doc?panel=ask");
  fireEvent.change(title(), { target: { value: "Guide" } });
  const brief = screen.getByRole("textbox", { name: "What should the doc cover?" });
  fireEvent.change(brief, { target: { value: "Cover it." } });
  fireEvent.keyDown(brief, { key: "Enter", metaKey: true });
  await waitFor(() => expect(fileTask).toHaveBeenCalledOnce());
  expect(write).not.toHaveBeenCalled();
});

test("a brief carries a draft to build on, or nothing beyond the reader's words", () => {
  const meeting = DOC_TEMPLATES[0]!;
  expect(docBrief(" Cover it. ", "", undefined)).toBe("Cover it.");
  expect(docBrief("Cover it.", "## Mine\n", meeting)).toBe(
    "Cover it.\n\nBuild on this first draft:\n\n## Mine",
  );
  expect(docBrief("Cover it.", meeting.body, meeting)).toContain(
    "Follow the meeting notes outline:",
  );
});

// WHAT IS OPEN IS THE ASSISTANT'S TO READ, and a change it proposes fills the
// page through its own setters: the title, the body in the editor, and the
// hand-off beside them. Create stays the writer's press.
test("the new doc is the assistant's to read, and a draft.set fills the page", async () => {
  openFrame("artifacts-new");
  mount();
  expect(context().draft).toMatchObject({ kind: "client", step: "write" });
  expect(context().accepts).toContain("draft.set");
  expect(draftField("title")).toMatchObject({ value: "", editable: true });
  expect(draftField("ask.agent")).toMatchObject({ value: "sre-lead", editable: true });
  expect(
    setDraft({ title: "Incident reviews", body: "# Reviews\n\nHow we run them." }),
  ).toMatchObject({ outcome: "applied" });
  expect(title()).toHaveProperty("value", "Incident reviews");
  await waitFor(() => expect(draftField("body")?.value).toBe("# Reviews\n\nHow we run them."));
  expect(setDraft({ "ask.agent": "nobody" })).toMatchObject({
    outcome: "invalid",
    reason: "Choose one of the agents offered: sre-lead.",
  });
  fireEvent.click(create());
  await waitFor(() => expect(write).toHaveBeenCalledOnce());
  expect(sent()).toMatchObject({
    title: "Incident reviews",
    doc: { body: expect.stringContaining("How we run them.") },
  });
});
