import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

const { call, openTurnStream, streams } = vi.hoisted(() => {
  const streams: {
    url: string;
    frame: (f: unknown) => void;
    stop: (r: unknown) => void;
    closed: boolean;
  }[] = [];
  return {
    streams,
    call: vi.fn(async (route: string, ..._rest: unknown[]) => {
      switch (route) {
        case "POST /assistant/conversations":
          return { conversation: { id: "c1", title: "" } };
        default:
          return { turn_id: "t1", stream_url: "/assistant/stream?turn=t1", after_seq: 0 };
      }
    }),
    openTurnStream: vi.fn(
      (url: string, handlers: { frame: (f: unknown) => void; stop: (r: unknown) => void }) => {
        const stream = { url, frame: handlers.frame, stop: handlers.stop, closed: false };
        streams.push(stream);
        return {
          close: () => {
            stream.closed = true;
          },
        };
      },
    ),
  };
});

vi.mock("~/protocol/assistant.ts", async (importOriginal) => ({
  ...(await importOriginal<typeof import("~/protocol/assistant.ts")>()),
  call,
  openTurnStream,
}));

const { useRoomBrief, roomPageContext } = await import("./roomBrief.ts");
const { takeQueuedConversation } = await import("./control.ts");

afterEach(() => {
  call.mockClear();
  openTurnStream.mockClear();
  streams.length = 0;
  takeQueuedConversation();
});

const brief = {
  scope: { screen: "chatlet", title: "Chatlet", record: { kind: "room", id: "r1" } },
  about: "The release waits on GP-12.",
  needs: [],
  points: [
    {
      text: "Sam asks whether 2.4.0 waits for GP-12.",
      source: { channel_id: "r1", message_id: "m1" },
    },
  ],
  follow_ups: ["Who owns GP-12?"],
  as_of: "2031-05-06T07:08:00Z",
};

// A ROOM'S CATCH UP IS A BRIEF TURN OF THE ROOM: one new conversation, one
// question in brief mode with the room as the page's selection, read off its
// stream until the brief card arrives and the turn is done.
test("a room's catch up asks a brief of the room and draws its card", async () => {
  const { result } = renderHook(() => useRoomBrief("r1"));
  await waitFor(() => expect(streams).toHaveLength(1));
  const turn = call.mock.calls.find(
    ([route]) => route === "POST /assistant/conversations/{id}/turns",
  );
  const body = turn?.[1] as {
    mode: string;
    page_context: { selection: { kind: string; id: string } };
  };
  expect(body.mode).toBe("brief");
  expect(body.page_context.selection).toMatchObject({ kind: "room", id: "r1" });
  expect(result.current.phase).toBe("asking");

  act(() => {
    streams[0]!.frame({ kind: "step", step: { state: "running", label: "Reading #engineering" } });
  });
  expect(result.current.status).toBe("Reading #engineering");
  act(() => {
    streams[0]!.frame({ kind: "card", card: { card: { type: "brief", brief } } });
    streams[0]!.frame({ kind: "done", done: { state: "completed" } });
  });
  expect(result.current.phase).toBe("done");
  expect(result.current.brief?.points[0]?.source.message_id).toBe("m1");
  expect(streams[0]!.closed).toBe(true);
});

// A TURN THAT ENDS WITH NO BRIEF, OR SAYS WHY IT FAILED, FAILS IN WORDS, and
// asking again starts a new turn.
test("a catch up that ends without a brief fails, and again asks anew", async () => {
  const { result } = renderHook(() => useRoomBrief("r1"));
  await waitFor(() => expect(streams).toHaveLength(1));
  act(() => {
    streams[0]!.frame({
      kind: "error",
      error: {
        code: "brief_invalid",
        message: "The brief could not be written. Ask for it again.",
      },
    });
  });
  expect(result.current.phase).toBe("failed");
  expect(result.current.error).toBe("The brief could not be written. Ask for it again.");
  act(() => result.current.again());
  await waitFor(() => expect(streams).toHaveLength(2));
  expect(result.current.phase).toBe("asking");
});

// UNMOUNTING STOPS READING: the pane closed, so nothing it would draw is read.
test("closing the pane stops reading the stream", async () => {
  const { unmount } = renderHook(() => useRoomBrief("r1"));
  await waitFor(() => expect(streams).toHaveLength(1));
  unmount();
  expect(streams[0]!.closed).toBe(true);
});

// CONTINUING IN ASK CREWLET OPENS THE SAME CONVERSATION in the panel.
test("open in Ask Crewlet queues the catch up's conversation for the panel", async () => {
  const { result } = renderHook(() => useRoomBrief("r1"));
  await waitFor(() => expect(streams).toHaveLength(1));
  act(() => result.current.openInAssistant());
  expect(takeQueuedConversation()).toBe("c1");
});

// THE ROOM ALONE, where the screen has not published this room as its selection.
test("the page asked about is the room, whatever the screen published", () => {
  expect(roomPageContext("r9").selection).toEqual({ kind: "room", id: "r9", noun: "room" });
});
