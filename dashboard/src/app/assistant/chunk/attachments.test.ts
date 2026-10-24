/**
 * Images attached to a question: bounded at the engine's own limits before
 * anything is sent, each uploaded at once as its raw bytes under its own
 * operation key, and the question carrying exactly the ids the engine
 * answered.
 */

import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, onTestFinished, test, vi } from "vitest";
import { ASSISTANT_ATTACHMENTS_PER_TURN, ASSISTANT_FILE_MAX_BYTES } from "~/contract/assistant.ts";

const { call } = vi.hoisted(() => ({
  call: vi.fn(async (route: string, body: unknown, options: { query?: { name?: string } }) => {
    switch (route) {
      case "POST /assistant/conversations":
        return { conversation: { id: "c1", title: "" } };
      case "POST /assistant/conversations/{id}/files": {
        const name = options.query?.name ?? "";
        return {
          file: {
            file_id: `f-${name}`,
            name,
            media_type: (body as Blob).type,
            size: (body as Blob).size,
            download: `/assistant/conversations/c1/files/f-${name}`,
          },
        };
      }
      case "POST /assistant/conversations/{id}/turns":
        return {
          turn_id: "t1",
          stream_url: "/assistant/conversations/c1/stream?turn=t1",
          after_seq: 0,
        };
      default:
        throw new Error(`unexpected ${route}`);
    }
  }),
}));

vi.mock("~/protocol/assistant.ts", async (importOriginal) => ({
  ...(await importOriginal<typeof import("~/protocol/assistant.ts")>()),
  call,
  openTurnStream: vi.fn(() => ({ close: () => {} })),
}));

const { admit, useAttachments } = await import("./attachments.ts");
const { send, resetSessionForTest } = await import("./session.ts");

const png = (name: string, size = 10) =>
  new File([new Uint8Array(size)], name, { type: "image/png" });

beforeEach(() => {
  let n = 0;
  vi.stubGlobal(
    "URL",
    Object.assign(URL, {
      createObjectURL: () => `blob:local/${(n += 1)}`,
      revokeObjectURL: () => {},
    }),
  );
});

afterEach(() => {
  call.mockClear();
  resetSessionForTest();
  vi.unstubAllGlobals();
});

test("a file of another type, or too large, is refused in words that say what to do", () => {
  const big = { name: "huge.png", type: "image/png", size: ASSISTANT_FILE_MAX_BYTES + 1 } as File;
  const pdf = new File(["%PDF"], "brief.pdf", { type: "application/pdf" });
  const svg = new File(["<svg/>"], "logo.svg", { type: "image/svg+xml" });
  const { admitted, refusals } = admit([big, pdf, svg, png("ok.png")], 0);
  expect(admitted.map((f) => f.name)).toEqual(["ok.png"]);
  expect(refusals).toEqual([
    "huge.png is larger than 5.0 MiB. Attach a smaller image.",
    "brief.pdf cannot be attached. Attach a PNG, JPEG, WebP or GIF image.",
    "logo.svg cannot be attached. Attach a PNG, JPEG, WebP or GIF image.",
  ]);
});

test("no more than a question's limit is admitted, with one sentence for the rest", () => {
  const files = Array.from({ length: ASSISTANT_ATTACHMENTS_PER_TURN + 2 }, (_, i) =>
    png(`${i}.png`),
  );
  const { admitted, refusals } = admit(files, 1);
  expect(admitted).toHaveLength(ASSISTANT_ATTACHMENTS_PER_TURN - 1);
  expect(refusals).toEqual([
    `A question can carry up to ${ASSISTANT_ATTACHMENTS_PER_TURN} images. Remove one to attach another.`,
  ]);
});

test("each image is uploaded at once as its own bytes, and the question carries their ids", async () => {
  const { result } = renderHook(() => useAttachments(null));
  act(() => result.current.add([png("a.png"), png("b.png")]));
  expect(result.current.drafts.map((d) => d.state)).toEqual(["uploading", "uploading"]);
  expect(result.current.uploading).toBe(true);
  await waitFor(() => expect(result.current.drafts.every((d) => d.state === "ready")).toBe(true));

  // ONE CONVERSATION for both uploads, though they started together.
  expect(
    call.mock.calls.filter(([route]) => route === "POST /assistant/conversations"),
  ).toHaveLength(1);
  const uploads = call.mock.calls.filter(
    ([route]) => route === "POST /assistant/conversations/{id}/files",
  );
  expect(uploads).toHaveLength(2);
  for (const [, body, options] of uploads) {
    expect(body).toBeInstanceOf(File);
    expect(options).toMatchObject({ id: "c1", opKey: expect.any(String) });
  }
  expect(new Set(uploads.map(([, , options]) => (options as { opKey: string }).opKey)).size).toBe(
    2,
  );

  let taken: ReturnType<typeof result.current.take> = [];
  act(() => {
    taken = result.current.take();
  });
  expect(result.current.drafts).toEqual([]);
  await send("What is wrong here?", null, taken);
  const turn = call.mock.calls.find(
    ([route]) => route === "POST /assistant/conversations/{id}/turns",
  );
  expect(turn?.[1]).toMatchObject({ attachments: ["f-a.png", "f-b.png"] });
});

test("a refused file is said above the composer and never uploaded", () => {
  const { result } = renderHook(() => useAttachments(null));
  act(() => result.current.add([new File(["x"], "notes.txt", { type: "text/plain" })]));
  expect(result.current.drafts).toEqual([]);
  expect(result.current.refusal).toBe(
    "notes.txt cannot be attached. Attach a PNG, JPEG, WebP or GIF image.",
  );
  expect(call).not.toHaveBeenCalled();
});

test("an upload a condition of the node kept out is said in the engine's words for it", async () => {
  // THE ENGINE'S DETAIL, which says who clears the condition and asks for a
  // wait only where one helps: here a log at its byte ceiling, which no wait
  // clears. The code's own sentence beside it says only that.
  const { RestError } = await import("~/protocol/rest.ts");
  const original = call.getMockImplementation()!;
  onTestFinished(() => {
    call.mockImplementation(original);
  });
  const refuse = (body: Record<string, unknown>) => {
    call.mockImplementation(async (route, ...rest) => {
      if (route === "POST /assistant/conversations/{id}/files") {
        throw new RestError(503, body, null);
      }
      return original(route, ...rest);
    });
  };
  const full =
    "There's no room left to save this kind of change. An administrator needs to make room for it.";
  refuse({
    error: "unavailable",
    message: "This can't be done here, and waiting won't change that.",
    detail: full,
  });
  const { result } = renderHook(() => useAttachments(null));
  act(() => result.current.add([png("chart.png")]));
  await waitFor(() => expect(result.current.drafts[0]?.state).toBe("failed"));
  expect(result.current.refusal).toBe(`chart.png could not be attached. ${full}`);

  // A WRITE WHOSE OUTCOME IS UNKNOWN keeps the code's sentence: its detail is
  // written for a program, which sends the same key again.
  refuse({
    error: "unavailable",
    message: "Can't answer right now. Try again in a moment, or ask another node.",
    detail: "Send it again with the same Idempotency-Key.",
    outcome: "unknown",
    op_id: "op-1",
  });
  act(() => result.current.add([png("photo.png")]));
  await waitFor(() => expect(result.current.drafts[1]?.state).toBe("failed"));
  expect(result.current.refusal).toBe(
    "photo.png could not be attached. Can't answer right now. Try again in a moment, or ask another node.",
  );
});

test("a question with nothing attached sends no attachments member", async () => {
  await send("Hello", null);
  const turn = call.mock.calls.find(
    ([route]) => route === "POST /assistant/conversations/{id}/turns",
  );
  expect(turn?.[1]).not.toHaveProperty("attachments");
});
