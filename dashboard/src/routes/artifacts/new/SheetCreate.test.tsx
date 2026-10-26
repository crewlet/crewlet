/**
 * A new sheet, made the way a person makes one: a title, a way in, columns
 * shaped in the grid, and Create, which writes the sheet, its schema and its
 * rows and opens it.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { SheetCreate } from "./SheetCreate.tsx";
import { draftField, openFrame, setDraft } from "../draftTesting.ts";

vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  const write = vi.fn();
  return {
    ...actual,
    keyedWrite: write,
    useKeyedGesture: () => actual.useKeyedGesture(write),
  };
});
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useOrg: () => null };
});
const fileArtifactTask = vi.hoisted(() => vi.fn());
vi.mock("./fileTask.ts", async (original) => ({
  ...(await original<typeof import("./fileTask.ts")>()),
  useFileArtifactTask: () => ({ fileArtifactTask, busy: false, refusal: null, held: null }),
}));
vi.mock("./WhoBuildsIt.tsx", () => ({
  useWhoBuildsIt: () => ({
    builders: [{ seat: { handle: "analyst", name: "Analyst" }, suggested: true }],
    seat: "analyst",
    setSeat: () => {},
    project: "ENG",
    setProject: () => {},
    projects: [],
    projectsLoading: false,
  }),
  WhoBuildsIt: () => <p>Agent: Analyst</p>,
}));
// THE BRAND NOTE under Ask an agent reads the company's brand over the
// socket, which these tests, mounted with no client, do not serve.
vi.mock("~/components/brand.tsx", () => ({ BrandAgentNote: () => null }));
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});

const ID = "0192f2c4-3333-7000-8000-00000000000a";
const sent: KeyedRequest[] = [];

beforeEach(() => {
  sent.length = 0;
  vi.mocked(keyedWrite).mockImplementation(async (req: KeyedRequest): Promise<KeyedAnswer> => {
    sent.push(req);
    if (req.method === "POST" && req.path === "/artifacts") {
      return { kind: "done", pending: false, body: { artifact: ID } };
    }
    return { kind: "done", pending: false, body: { verdict: "applied", seq: 1 } };
  });
});
afterEach(cleanup);

function mount(hash = "#/artifacts/new/sheet") {
  location.hash = hash;
  // A CLIENT TO STAND IN: the page reads the folder tree for its Save in.
  return render(
    <TestClient>
      <Router>
        <LayerHost>
          <ToastProvider>
            <SheetCreate />
          </ToastProvider>
        </LayerHost>
      </Router>
    </TestClient>,
  );
}

const grid = () => screen.getByRole("grid");
const headers = () =>
  within(grid())
    .getAllByRole("columnheader")
    .slice(1)
    .map((h) => h.textContent?.split(",")[0]);
const create = () => screen.getByRole("button", { name: /Create sheet/ });

test("it opens on a blank sheet, the title focused, and Create waits for a title", () => {
  mount();
  expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Sheet title" }));
  expect(headers()).toEqual(["Name"]);
  expect(screen.getByRole("radio", { name: /Blank/ })).toHaveProperty("checked", true);
  fireEvent.click(create());
  expect(sent).toEqual([]);
  expect(screen.getByText("Give it a title.")).toBeTruthy();
});

test("a template swaps the grid to its columns and examples, which the reader may drop", () => {
  mount();
  fireEvent.click(screen.getByRole("radio", { name: /Tracker/ }));
  expect(headers()).toEqual(["Task", "Owner", "Status", "Priority", "Due", "Notes"]);
  expect(within(grid()).getAllByText("Draft the launch announcement").length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole("checkbox", { name: "Start with the example rows" }));
  expect(within(grid()).queryAllByText("Draft the launch announcement")).toEqual([]);
  // Back to Blank and to the Tracker again: each keeps its own draft.
  fireEvent.click(screen.getByRole("radio", { name: /Blank/ }));
  expect(headers()).toEqual(["Name"]);
  fireEvent.click(screen.getByRole("radio", { name: /Tracker/ }));
  expect(within(grid()).queryAllByText("Draft the launch announcement")).toEqual([]);
});

test("Create writes the sheet, its schema and its rows in the order drawn, and opens it", async () => {
  mount("#/artifacts/new/sheet?item=ENG-12");
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Launch plan" },
  });
  fireEvent.click(screen.getByRole("radio", { name: /Tracker/ }));
  expect(screen.getByText("It will be listed on ENG-12.")).toBeTruthy();
  fireEvent.keyDown(screen.getByRole("textbox", { name: "Sheet title" }), {
    key: "Enter",
    metaKey: true,
  });
  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${ID}`));
  expect(sent.map((r) => `${r.method} ${r.path}`)).toEqual([
    "POST /artifacts",
    `PUT /artifacts/${ID}/data/schema`,
    `POST /artifacts/${ID}/data/writes`,
  ]);
  expect(sent[0]!.body).toEqual({ kind: "sheet", title: "Launch plan", work_item: "ENG-12" });
  const schema = sent[1]!.body as { collections: { name: string; columns: { key: string }[] }[] };
  expect(schema.collections[0]!.name).toBe("tasks");
  const ops = (sent[2]!.body as { ops: { doc: string; fields: { task: string } }[] }).ops;
  expect(ops.map((o) => o.fields.task)).toEqual([
    "Draft the launch announcement",
    "Review the pricing page",
    "Book the customer interviews",
  ]);
  expect(ops.map((o) => o.doc).sort()).toEqual(ops.map((o) => o.doc));
});

// STARTED IN A FOLDER, FILED THERE: New on a folder's page opens the kind's
// page with `?folder=`, the bar says where it will be saved, and the create
// carries the folder (the engine decides whether the reader may file there).
test("a sheet started in a folder is filed in it, and the bar says where it is saved", async () => {
  const folder = "0192f2c4-4444-7000-8000-00000000000b";
  mount(`#/artifacts/new/sheet?folder=${folder}`);
  expect(screen.getAllByRole("button", { name: /Save in/ }).length).toBeGreaterThan(0);
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Launch plan" },
  });
  fireEvent.keyDown(screen.getByRole("textbox", { name: "Sheet title" }), {
    key: "Enter",
    metaKey: true,
  });
  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${ID}`));
  expect(sent[0]!.body).toEqual({ kind: "sheet", title: "Launch plan", folder });
});

test("a refused row batch still opens the sheet, and says how many rows went in", async () => {
  vi.mocked(keyedWrite).mockImplementation(async (req: KeyedRequest): Promise<KeyedAnswer> => {
    sent.push(req);
    if (req.path === "/artifacts") return { kind: "done", pending: false, body: { artifact: ID } };
    if (req.path.endsWith("/data/writes")) {
      return { kind: "refused", text: "The store is full.", error: null } as unknown as KeyedAnswer;
    }
    return { kind: "done", pending: false, body: {} };
  });
  mount();
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Budget" },
  });
  fireEvent.click(screen.getByRole("radio", { name: /Budget/ }));
  fireEvent.click(create());
  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${ID}`));
  expect(await screen.findByText("The sheet was made, and its rows were not written")).toBeTruthy();
});

test("a press on a header opens its settings; Add column appends one and opens it", () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Add column" }));
  expect(headers()).toEqual(["Name", "Column 2"]);
  const dialog = screen.getByRole("dialog", { name: "Column settings for Column 2" });
  fireEvent.change(within(dialog).getByRole("textbox", { name: "Name" }), {
    target: { value: "Due date" },
  });
  expect(headers()).toEqual(["Name", "Due date"]);
  fireEvent.click(within(dialog).getByRole("button", { name: "Use Date" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  fireEvent.click(within(grid()).getAllByRole("columnheader")[1]!);
  expect(screen.getByRole("dialog", { name: "Column settings for Name" })).toBeTruthy();
});

test("a CSV is read in the browser, typed by its values, and titles the sheet", async () => {
  mount();
  fireEvent.click(screen.getByRole("radio", { name: /Import a CSV/ }));
  const file = new File(
    ['Gate,Budget,Approved\nSecurity review,"12,500",yes\nLoad test,800,no\n'],
    "launch-gates.csv",
    {
      type: "text/csv",
    },
  );
  const input = document.querySelector<HTMLInputElement>('input[type="file"]')!;
  expect(screen.getByText("Choose a CSV file, or drop one here")).toBeTruthy();
  await act(async () => {
    fireEvent.change(input, { target: { files: [file] } });
  });
  await waitFor(() => expect(headers()).toEqual(["Gate", "Budget", "Approved"]));
  expect(screen.getByRole("textbox", { name: "Sheet title" })).toHaveProperty(
    "value",
    "Launch gates",
  );
  expect(screen.getByText(/2 rows, 3 columns\. Every value fits its column\./)).toBeTruthy();
  expect(within(grid()).getByText("12,500")).toBeTruthy();
});

test("a file that cannot be read as CSV says why, on the page", async () => {
  mount();
  fireEvent.click(screen.getByRole("radio", { name: /Import a CSV/ }));
  const file = new File(['a,b\n1,"never closed\n'], "broken.csv", { type: "text/csv" });
  await act(async () => {
    fireEvent.change(document.querySelector('input[type="file"]')!, { target: { files: [file] } });
  });
  expect(
    await screen.findByText("broken.csv has a quote that is never closed, starting on line 2."),
  ).toBeTruthy();
});

test("Cancel with something typed asks before discarding it", () => {
  mount();
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Plan" },
  });
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(screen.getByRole("alertdialog", { name: "Discard this sheet?" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  expect(location.hash).toBe("#/artifacts");
});

test("Ask an agent opens the hand-off beside the grid, files a task with the brief, and lands on it", async () => {
  fileArtifactTask.mockResolvedValue({ item: "ENG-42", key: "ENG-42", linked: null });
  mount();
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Hiring pipeline" },
  });
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  // THE PAGE STAYS: the grid and the row policy are still there beside it.
  expect(screen.getByRole("grid")).toBeTruthy();
  expect(screen.getByText("Who can change rows")).toBeTruthy();
  const ask = within(panel).getByRole("button", { name: "Ask Analyst" });
  fireEvent.click(ask);
  expect(fileArtifactTask).not.toHaveBeenCalled();
  expect(within(panel).getAllByText("Say what the sheet should hold.").length).toBeGreaterThan(0);
  // AN UNTOUCHED BLANK GRID says nothing the brief does not: only the title goes.
  expect(within(panel).getByText("“Hiring pipeline”")).toBeTruthy();
  expect(within(panel).queryByText(/columns?$/)).toBeNull();
  fireEvent.click(within(panel).getByRole("button", { name: "Hiring pipeline" }));
  fireEvent.click(ask);
  await waitFor(() => expect(location.hash).toBe("#/worklet/ENG-42"));
  expect(fileArtifactTask).toHaveBeenCalledWith({
    kind: "sheet",
    title: "Hiring pipeline",
    brief: "A hiring pipeline: each candidate's role, stage, interviewer and next step.",
    seat: "analyst",
    project: "ENG",
  });
  expect(sent).toEqual([]);
});

test("the columns shaped in the grid go to the agent with the brief", async () => {
  fileArtifactTask.mockResolvedValue({ item: "ENG-43", key: "ENG-43", linked: null });
  mount();
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Launch" },
  });
  fireEvent.click(screen.getByRole("radio", { name: /Tracker/ }));
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  expect(within(panel).getByText(/^\d+ columns/)).toBeTruthy();
  fireEvent.change(within(panel).getByRole("textbox", { name: "What should the sheet hold?" }), {
    target: { value: "The launch gates." },
  });
  fireEvent.click(within(panel).getByRole("button", { name: "Ask Analyst" }));
  await waitFor(() => expect(fileArtifactTask).toHaveBeenCalled());
  const brief = fileArtifactTask.mock.calls.at(-1)![0].brief as string;
  expect(brief.startsWith("The launch gates.\n\nStart from these columns:\n- ")).toBe(true);
});

// WHAT IS SHAPED IS THE ASSISTANT'S TO READ, and a change it proposes picks
// the way in as a press of its card would; the grid stays the reader's.
test("the sheet being made is the assistant's to read, and a draft.set picks its template", async () => {
  openFrame("artifacts-new");
  mount();
  expect(draftField("start")).toMatchObject({ value: "blank", editable: true });
  expect(draftField("columns")).toMatchObject({ value: "Name (Text)", kind: "list" });
  expect(draftField("columns")).not.toHaveProperty("editable");
  expect(setDraft({ title: "Launch tracker", start: "tracker" })).toMatchObject({
    outcome: "applied",
  });
  expect(headers()).toEqual(["Task", "Owner", "Status", "Priority", "Due", "Notes"]);
  expect(screen.getByRole("textbox", { name: "Sheet title" })).toHaveProperty(
    "value",
    "Launch tracker",
  );
  await waitFor(() => expect(draftField("start")?.value).toBe("tracker"));
  expect(setDraft({ columns: "Task" })).toMatchObject({ outcome: "invalid" });
});
