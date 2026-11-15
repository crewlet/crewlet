import { afterEach, describe, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { KeyedAnswer, KeyedRequest } from "~/lib/keyedWrite.ts";
import { RestError, type ArtifactDetail, type LiveSocket } from "~/protocol/index.ts";
import { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { PresenceReport } from "./presence.ts";
import { SheetView } from "./SheetView.tsx";

vi.mock("~/app/router.tsx", () => ({
  useParam: () => ["", () => {}],
  useNavigator: () => () => {},
  useRoute: () => ({ path: [], query: {}, hash: "" }),
}));
vi.mock("~/lib/store-hooks.ts", () => ({ useConnection: () => ({ connected: true }) }));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({ data: null, loading: false, error: null }),
}));
const schemaWrite = vi.fn();
vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return {
    ...actual,
    useKeyedGesture: () => ({ run: schemaWrite, busy: false, answer: null, reset: () => {} }),
  };
});
vi.mock("~/lib/artifacts/people.ts", () => ({
  useArtifactPeople: () => ({ sheetPeople: [] }),
  cellPresence: () => [],
}));

afterEach(cleanup);

const SCHEMA = {
  role: "editor",
  collections: [
    {
      name: "rows",
      title: "Tasks",
      create: "contributor",
      delete: "contributor",
      columns: [
        { key: "note", label: "Note", type: "text" },
        { key: "task", label: "Task", type: "text", required: true },
      ],
    },
  ],
};

function draw(schema: unknown, write: (req: KeyedRequest) => Promise<KeyedAnswer>) {
  const query = vi.fn(async (kind: string) => {
    if (kind === "artifact_data_schema") return schema;
    if (kind === "artifact_data") {
      return {
        docs: [],
        through: 1,
        position: { stream: "data", generation: 0, seq: 1 },
        complete: true,
      };
    }
    return {};
  });
  const store = new ArtifactDataStore({
    artifact: "a1",
    socket: { query } as unknown as LiveSocket,
    via: { via: "dashboard" },
    clientID: "client-1",
    newKey: () => "key",
    write,
  });
  render(
    <SheetView
      detail={{ id: "a1", title: "Tracker", role: "editor" } as unknown as ArtifactDetail}
      store={store}
      room={{ others: [] } as unknown as ArtifactRoom}
      presence={{ at: () => {} } as unknown as PresenceReport}
      drawn={[]}
    />,
  );
  return store;
}

describe("Add row", () => {
  test("on a sheet with a required column writes nothing, names what it needs and takes the cursor there", async () => {
    const write = vi.fn(async (): Promise<KeyedAnswer> => {
      throw new Error("nothing should be written");
    });
    draw(SCHEMA, write);
    fireEvent.click(await screen.findByRole("button", { name: "Add row" }));
    expect(await screen.findByText("Row 1 needs Task before it saves.")).toBeTruthy();
    expect(write).not.toHaveBeenCalled();
    await waitFor(() => expect(document.activeElement?.getAttribute("aria-colindex")).toBe("3"));
    expect(document.activeElement?.getAttribute("role")).toBe("gridcell");
  });

  test("a refused add says why on the strip instead of Saved", async () => {
    const store = draw(
      {
        role: "editor",
        collections: [
          { ...SCHEMA.collections[0], columns: [{ key: "note", label: "Note", type: "text" }] },
        ],
      },
      async () => ({
        kind: "refused",
        text: "Refused",
        stale: false,
        error: new RestError(409, {
          error: "quota_exceeded",
          verdict: "quota",
          detail: "This sheet is full.",
        }),
      }),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Add row" }));
    expect(await screen.findByText(/^Not saved: /)).toBeTruthy();
    expect(screen.queryByText("Saved")).toBeNull();
    act(() => fireEvent.click(screen.getByRole("button", { name: "Dismiss" })));
    expect(store.addRefusal).toBeNull();
    expect(await screen.findByText("Saved")).toBeTruthy();
  });
});

describe("an empty sheet", () => {
  // A SHEET IS ONE TABLE: its editor names the columns in the sheet itself,
  // in words, and the one collection behind them is written for them. Set up
  // columns opened the Data panel's schema editor, which asked for a
  // lowercase collection name before a single column.
  test("names its columns in place and saves them as the sheet's one table", async () => {
    schemaWrite.mockReset();
    schemaWrite.mockResolvedValue({ kind: "done", pending: false, body: {} });
    const write = vi.fn(async (): Promise<KeyedAnswer> => {
      throw new Error("no row is written");
    });
    draw({ role: "editor", collections: [] }, write);
    expect(await screen.findByText("No columns yet")).toBeTruthy();
    expect(screen.queryByText(/collection/i)).toBeNull();
    const save = screen.getByRole("button", { name: "Save columns" });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(screen.getByRole("textbox", { name: "Column 1" }), {
      target: { value: "Team" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add a column" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Column 2" }), {
      target: { value: "Due date" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save columns" }));
    });
    expect(schemaWrite).toHaveBeenCalledOnce();
    const req = schemaWrite.mock.calls[0]![0];
    expect(req).toMatchObject({ method: "PUT", path: "/artifacts/a1/data/schema" });
    expect(req.body.collections).toHaveLength(1);
    expect(req.body.collections[0]).toMatchObject({ name: "rows", preset: "shared" });
    expect(req.body.collections[0].columns).toEqual([
      { key: "team", type: "text", label: "Team", primary: true },
      { key: "due_date", type: "text", label: "Due date" },
    ]);
  });

  test("two names that would be one column are held, said in the names typed", async () => {
    draw({ role: "editor", collections: [] }, vi.fn());
    fireEvent.change(await screen.findByRole("textbox", { name: "Column 1" }), {
      target: { value: "Due date" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add a column" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Column 2" }), {
      target: { value: "Due-date" },
    });
    const save = screen.getByRole("button", { name: "Save columns" });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(save.getAttribute("aria-describedby")!)?.textContent).toBe(
      "“Due date” and “Due-date” would be the same column. Rename one.",
    );
  });

  test("a reader who cannot edit is told who sets the columns up", async () => {
    draw({ role: "viewer", collections: [] }, vi.fn());
    expect(
      await screen.findByText("The people who can edit this sheet set up its columns."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save columns" })).toBeNull();
  });
});
