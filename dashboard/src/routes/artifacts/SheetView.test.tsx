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
