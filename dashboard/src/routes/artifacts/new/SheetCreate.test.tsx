/**
 * A new sheet, made the way a person makes one: a title, a way in, columns
 * shaped in the grid, and Create, which writes the sheet, its schema and its
 * rows and opens it.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { SheetCreate } from "./SheetCreate.tsx";

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
vi.mock("./fileTask.ts", () => ({
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
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <SheetCreate />
        </ToastProvider>
      </LayerHost>
    </Router>,
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

test("Ask an agent files a real task to the agent with the brief, and lands on it", async () => {
  fileArtifactTask.mockResolvedValue({ item: "ENG-42", key: "ENG-42", linked: null });
  mount();
  fireEvent.change(screen.getByRole("textbox", { name: "Sheet title" }), {
    target: { value: "Hiring pipeline" },
  });
  fireEvent.click(screen.getByRole("radio", { name: /Ask an agent/ }));
  expect(screen.queryByRole("grid")).toBeNull();
  expect(screen.queryByText("Who can change rows")).toBeNull();
  const ask = screen.getByRole("button", { name: /Ask Analyst/ });
  fireEvent.click(ask);
  expect(fileArtifactTask).not.toHaveBeenCalled();
  expect(screen.getByText("Say what the sheet should hold.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "A hiring pipeline" }));
  expect(screen.getByText(/Analyst gets a task to set up “Hiring pipeline”/)).toBeTruthy();
  fireEvent.click(ask);
  await waitFor(() => expect(location.hash).toBe("#/work/ENG-42"));
  expect(fileArtifactTask).toHaveBeenCalledWith({
    kind: "sheet",
    title: "Hiring pipeline",
    brief: "A hiring pipeline: each candidate's role, stage, interviewer and next step.",
    seat: "analyst",
    project: "ENG",
  });
  expect(sent).toEqual([]);
});
