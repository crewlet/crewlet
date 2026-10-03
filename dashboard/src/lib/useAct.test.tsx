/**
 * A press, end to end: what the person is told, and what the screen reads
 * next.
 *
 * WRITES ARE CONFIRMED, NOT OPTIMISTIC — so the case that matters is the whole
 * loop: the change answers with a position, this tab's floor rises to it, and
 * the question on screen is asked again AT that floor, which is what makes the
 * redraw include the press. A hook that toasted "done" and left the screen
 * polling at `stale` would pass every smaller test here.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { useAct } from "./useAct.ts";
import { useQuery } from "./useQuery.ts";
import { useClient, useConnection } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import { WriteButton } from "~/components/WriteButton.tsx";
import { tabFloors } from "~/protocol/floors.ts";

vi.mock("./store-hooks.ts", () => ({ useClient: vi.fn(), useConnection: vi.fn() }));
vi.mock("./viewer.ts", () => ({ useViewer: vi.fn() }));

const BOUND: ViewerState = {
  login: "jane.founder",
  grants: ["state:read", "work:write"],
  operatesFleet: false,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: ["set_pins", "update_work_item"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};

interface Sent {
  kind: string;
  params: Record<string, unknown>;
}

let sent: Sent[];

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(BOUND);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useClient).mockReturnValue({
    socket: {
      query: vi.fn(async (kind: string, params: Record<string, unknown>) => {
        sent.push({ kind, params });
        return { views: [] };
      }),
    },
  } as never);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** One press as the act route received it: its operation key, and its body. */
interface Post {
  /** The `Idempotency-Key` it carried — the write's identity. */
  opId: string | null;
  args: unknown;
}

/** What one request to the act route carried. */
function received(init: RequestInit): Post {
  return {
    opId: new Headers(init.headers).get("Idempotency-Key"),
    args: (JSON.parse(init.body as string) as { args: unknown }).args,
  };
}

/** The engine's act route, answering every press with `answer`. */
function engine(answer: () => Response) {
  const posts: Post[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init: RequestInit) => {
      posts.push(received(init));
      return answer();
    }),
  );
  return posts;
}

const json = (payload: unknown, status: number) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** A screen that reads a tracker question and pins a view. */
function Screen() {
  useQuery("work_views", { container: "workspace" });
  const pin = useAct("set_pins");
  return (
    <WriteButton
      write={pin}
      onPress={() => void pin.run({ views: { add: ["v1"] } }, { done: "Pinned Triage" })}
    >
      Pin
    </WriteButton>
  );
}

function mount() {
  return render(
    <ToastProvider>
      <Screen />
    </ToastProvider>,
  );
}

test("an applied press is re-read at the position it landed at, and named in a toast", async () => {
  engine(() =>
    json({ tool: "set_pins", outcome: "applied", position: "CREWLET_TRACKER_LOG@1:900" }, 200),
  );
  mount();
  await waitFor(() => expect(sent).toHaveLength(1));
  // BEFORE ANY WRITE the question names no floor of its own — unless an
  // earlier case in this file raised the tab's, which is exactly what a tab
  // does, so only the next read is asserted on.
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1]!.params).toMatchObject({
    container: "workspace",
    read_level: "session",
    min_position: "CREWLET_TRACKER_LOG@1:900",
  });
  expect((await screen.findAllByText("Pinned Triage")).length).toBeGreaterThan(0);
});

test("pending tells the person this node has not applied it, and the reads wait for it", async () => {
  engine(() =>
    json({ tool: "set_pins", outcome: "pending", position: "CREWLET_TRACKER_LOG@1:901" }, 200),
  );
  mount();
  await waitFor(() => expect(sent).toHaveLength(1));
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  expect((await screen.findAllByText(/this node has not applied it yet/)).length).toBeGreaterThan(
    0,
  );
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1]!.params).toMatchObject({ min_position: "CREWLET_TRACKER_LOG@1:901" });
  expect(tabFloors.floor("tracker")).toBe("CREWLET_TRACKER_LOG@1:901");
});

// UNKNOWN IS NEVER RETRIED ON ITS OWN, and a Retry the person presses sends
// the SAME operation key: the engine takes it as the write's identity, so the
// retry is the first attempt again rather than a second change.
test("unknown stays on screen, and its Retry sends the same operation key", async () => {
  const posts = engine(() => new Response("bad gateway", { status: 502 }));
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  expect(
    (await screen.findAllByText(/Could not confirm — it may have landed/)).length,
  ).toBeGreaterThan(0);
  await new Promise((r) => setTimeout(r, 20));
  expect(posts).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(posts).toHaveLength(2));
  expect(posts[0]!.opId).toBeTruthy();
  expect(posts[1]!.opId).toBe(posts[0]!.opId);

  // AND A NEW PRESS IS A NEW OPERATION.
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  await waitFor(() => expect(posts).toHaveLength(3));
  expect(posts[2]!.opId).not.toBe(posts[0]!.opId);
});

/** Two presses of one control, each pinning a different view. */
function TwoPresses() {
  const pin = useAct("set_pins");
  return (
    <>
      <WriteButton
        write={pin}
        onPress={() => void pin.run({ views: { add: ["va"] } }, { done: "Pinned A" })}
      >
        Pin A
      </WriteButton>
      <WriteButton
        write={pin}
        onPress={() => void pin.run({ views: { add: ["vb"] } }, { done: "Pinned B" })}
      >
        Pin B
      </WriteButton>
    </>
  );
}

// A TOAST'S RETRY IS BOUND TO ITS OWN PRESS. The toast stays while the person
// goes on working, so a Retry that read "the last press" when clicked sent
// whatever was pressed since — another change, under another operation key —
// and the press the toast was about was never retried at all.
test("an unknown press's Retry sends that press, whatever was pressed since", async () => {
  const answers = [
    () => new Response("bad gateway", { status: 502 }),
    () =>
      json({ tool: "set_pins", outcome: "applied", position: "CREWLET_TRACKER_LOG@1:950" }, 200),
    () =>
      json({ tool: "set_pins", outcome: "applied", position: "CREWLET_TRACKER_LOG@1:951" }, 200),
  ];
  const posts: Post[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init: RequestInit) => {
      posts.push(received(init));
      return answers[posts.length - 1]!();
    }),
  );
  render(
    <ToastProvider>
      <TwoPresses />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Pin A" }));
  await screen.findAllByText(/Could not confirm — it may have landed/);
  fireEvent.click(screen.getByRole("button", { name: "Pin B" }));
  await waitFor(() => expect(posts).toHaveLength(2));
  await screen.findAllByText("Pinned B");

  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(posts).toHaveLength(3));
  expect(posts[2]).toEqual(posts[0]);
  expect(posts[2]!.args).toEqual({ views: { add: ["va"] } });
});

// A REFUSAL OUTLASTS THE TOAST: it is drawn beside the control that caused it
// and stays until the next press, because a sentence naming the field that
// was wrong is gone from a toast before it has been read.
test("a refusal is drawn beside the control and stays until the next press", async () => {
  engine(() =>
    json({ error: "invalid", tool: "set_pins", detail: "views: v9 is not a view" }, 422),
  );
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  const refusal = () => document.querySelector(".write-refusal");
  await waitFor(() => expect(refusal()?.textContent).toContain("views: v9 is not a view"));
  expect(refusal()?.getAttribute("role")).toBe("alert");
  await new Promise((r) => setTimeout(r, 50));
  expect(refusal()?.textContent).toContain("views: v9 is not a view");
});

/** A screen that pins a view and reads back the refusal its press came to. */
function Refused({ onRefusal }: { onRefusal: (r: unknown) => void }) {
  const pin = useAct("set_pins");
  onRefusal(pin.refusal);
  return (
    <WriteButton
      write={pin}
      onPress={() => void pin.run({ views: { add: ["v1"] } }, { done: "Pinned Triage" })}
    >
      Pin
    </WriteButton>
  );
}

// A REFUSAL ON AUTHORITY SAYS WHAT WOULD CHANGE IT: the grants the engine's
// deciding rule would have admitted this person on travel from the act
// transport's refused arm onto the hook's refusal, and the control draws
// them. Dropped on the way, the person was told "you do not hold what this
// change needs" and never what that was.
test("a refusal on authority carries the grants the engine named, and draws them", async () => {
  engine(() =>
    json(
      {
        error: "unauthorized",
        tool: "set_pins",
        reason: "needs a grant this person does not hold",
        grants: ["work:write", "fleet:operate"],
      },
      403,
    ),
  );
  let seen: unknown = null;
  render(
    <ToastProvider>
      <Refused onRefusal={(r) => (seen = r)} />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  const refusal = () => document.querySelector(".write-refusal");
  await waitFor(() => expect(refusal()).not.toBeNull());
  expect(seen).toMatchObject({ grants: ["work:write", "fleet:operate"] });
  expect(refusal()?.textContent).toContain("work:write");
  expect(refusal()?.textContent).toContain("fleet:operate");
});

// AND A REFUSAL THAT IS NOT ON AUTHORITY NAMES NONE, rather than a grant
// nobody asked for: the control's "Needs …" line is for authority alone.
test("a refusal that is not on authority carries no grants", async () => {
  engine(() =>
    json({ error: "invalid", tool: "set_pins", detail: "views: v9 is not a view" }, 422),
  );
  let seen: unknown = null;
  render(
    <ToastProvider>
      <Refused onRefusal={(r) => (seen = r)} />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  await waitFor(() => expect(document.querySelector(".write-refusal")).not.toBeNull());
  expect(seen).toMatchObject({ grants: [] });
  expect(document.querySelector(".write-refusal")?.textContent).not.toContain("Needs");
});

// OFFLINE IS DISABLED, NEVER QUEUED.
test("offline, the press sends nothing and says why", async () => {
  vi.mocked(useConnection).mockReturnValue({ connected: false } as never);
  const posts = engine(() => json({}, 200));
  mount();
  const button = screen.getByRole("button", { name: "Pin" });
  expect(button.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(button);
  await new Promise((r) => setTimeout(r, 20));
  expect(posts).toHaveLength(0);
  expect(document.body.textContent).toContain("Offline");
});

/** A press whose outcome the caller draws itself, abandonable through a signal. */
function Quiet({ signal, onResult }: { signal?: AbortSignal; onResult: (r: unknown) => void }) {
  const pin = useAct("set_pins");
  return (
    <button
      onClick={() =>
        void pin
          .run({ views: { add: ["v1"] } }, { done: "Pinned Triage", quiet: true }, { signal })
          .then(onResult)
      }
    >
      Quiet
    </button>
  );
}

// A QUIET PRESS IS DRAWN BY ITS CALLER — the palette's answer, over which a
// toast saying "Answered" would be the same fact twice — and resolves with the
// outcome for it to draw.
test("a quiet press raises no toast, and hands its caller the outcome", async () => {
  engine(() =>
    json({ tool: "set_pins", outcome: "applied", position: "CREWLET_TRACKER_LOG@1:950" }, 200),
  );
  const got = vi.fn();
  render(
    <ToastProvider>
      <Quiet onResult={got} />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Quiet" }));
  await waitFor(() => expect(got).toHaveBeenCalled());
  expect(got.mock.calls[0]![0]).toMatchObject({ kind: "applied" });
  expect(screen.queryByText("Pinned Triage")).toBeNull();
});

// AN ABANDONED PRESS IS NOT AN ANSWER: a later question superseded it, so it
// reports nothing and resolves with null rather than rejecting.
test("an abandoned press resolves with null and reports nothing", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise((_, reject) => {
          init.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
        }),
    ),
  );
  const controller = new AbortController();
  const got = vi.fn();
  render(
    <ToastProvider>
      <Quiet signal={controller.signal} onResult={got} />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Quiet" }));
  controller.abort();
  await waitFor(() => expect(got).toHaveBeenCalledWith(null));
  expect(screen.queryByText(/Could not confirm/)).toBeNull();
});
