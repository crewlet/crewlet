/**
 * What a case does ends with the case.
 *
 * Each pair below is a case that RUNS OUT OF ITS TIME with something still
 * out — a wait of its OWN that nothing in the harness can see, an `act` scope,
 * a scope inside a scope or a wait inside one, a `findBy`, a render or a
 * cleanup still to make, a poll on a fake clock — and the case after it,
 * which draws its own page, releases what the first one waited for, and reads
 * what the late case came to. The timeout is real: the first case is
 * `test.fails` with a budget of [RUNS_OUT], waiting on something that never
 * comes inside it, so what is checked is what Vitest itself does with a case
 * it has given up on — fails it, starts the next, and leaves its function
 * running. And the case after it first holds it to having failed by that
 * timeout and by nothing else ([runsOut]).
 *
 * Through the library alone, every one of these reached the second case: the
 * late `act` ran its body beside it, the open scope held React's act count
 * raised so the second case's own render never landed, the polling `findBy`
 * held the act environment set aside, the late wait and click found and
 * pressed the second case's control, and the late render and cleanup drew
 * into the second case's page and took it away. Ended in any order but
 * innermost first, a scope inside a scope left the count raised just the
 * same, and a wait inside a scope put back the scope's environment over the
 * case's. And Vitest's own `vi.waitFor` moved the next case's fake clock.
 * Each assertion below goes red when the matching half of `inCase.ts` or
 * `cases.ts` is taken away.
 */

import { act, answered, cleanup, fireEvent, poll, render, renderHook, screen } from "./inCase.ts";
import { useState } from "react";
import { afterEach, expect, onTestFailed, onTestFinished, test, vi } from "vitest";

afterEach(cleanup);

/** What a late move came to: "returned", or why it was refused. */
function settled(work: Promise<unknown>): Promise<string> {
  return work.then(
    () => "returned",
    (cause: unknown) => (cause instanceof Error ? cause.message : String(cause)),
  );
}

const ENDED =
  "the case that asked has ended — this is that case, still running after its time ran out";

/**
 * The budget each first case runs out of. It is always spent, since what the
 * case waits for comes only in the next case, so it sets how long the file
 * takes and nothing else.
 */
const RUNS_OUT = 100;

/**
 * A case that RUNS OUT of its time: `test.fails` on a budget of [RUNS_OUT].
 *
 * `test.fails` reports ANY failure as the one expected — a case that threw
 * before it ever waited would pass exactly as one the runner gave up on, and
 * every pair below would then be checking something other than a timeout. So
 * what the case failed with is kept, and the check handed back — which the
 * case after it calls first — holds it to the runner's own timeout.
 */
function runsOut(name: string, body: () => Promise<void>): () => void {
  let failedWith = "it did not fail";
  test.fails(
    name,
    async () => {
      onTestFailed(({ task }) => {
        failedWith = (task.result?.errors ?? []).map((error) => error.message).join("\n");
      });
      await body();
    },
    RUNS_OUT,
  );
  return () => {
    expect(failedWith, `"${name}" failed, but not by running out of its time`).toMatch(
      new RegExp(`^Test timed out in ${RUNS_OUT}ms`),
    );
  };
}

/* A late act, and a late flush whose step would move the timers. */

let release: () => void = () => {};
let lateAct: Promise<string> = Promise.resolve("never started");
let lateFlush: Promise<string> = Promise.resolve("never started");
/** Whether a late move's body ran — the move a late case must not make. */
let lateMoved = false;

const waitingRanOut = runsOut(
  "a case that runs out of time while it waits for something of its own",
  async () => {
    const own = new Promise<void>((resolve) => {
      release = resolve;
    });
    lateAct = settled(
      (async () => {
        await own;
        await act(async () => {
          lateMoved = true;
        });
      })(),
    );
    lateFlush = settled(
      (async () => {
        await own;
        // A STEP, as a case that holds the timers takes one: advancing them
        // fires whatever is armed — by then, the next case's timers.
        await answered(() => {
          lateMoved = true;
        });
      })(),
    );
    await Promise.all([lateAct, lateFlush]);
  },
);

test("is refused its next act and its next flush, before either moves", async () => {
  waitingRanOut();
  release();
  expect(await lateAct).toBe(`act: ${ENDED}`);
  expect(await lateFlush).toBe(`act: ${ENDED}`);
  expect(lateMoved).toBe(false);
  // And the live case's own, step and all, is unaffected.
  let mine = 0;
  await act(async () => {
    mine++;
  });
  await answered(() => {
    mine++;
  });
  expect(mine).toBe(2);
});

/* An act scope still open when its case runs out of time. */

let unhold: () => void = () => {};
let openScope: Promise<string> = Promise.resolve("never started");

const scopeRanOut = runsOut(
  "a case that runs out of time inside an act scope of its own",
  async () => {
    const held = new Promise<void>((resolve) => {
      unhold = resolve;
    });
    openScope = settled(
      act(async () => {
        await held;
      }),
    );
    await openScope;
  },
);

function Counter() {
  const [presses, setPresses] = useState(0);
  return <button onClick={() => setPresses(presses + 1)}>{`pressed ${presses}`}</button>;
}

test("finds that scope closed when it begins, so its own renders land", async () => {
  scopeRanOut();
  // Left open, the scope keeps React's one act count raised, and this render's
  // own act — nested inside it — queues the render and flushes none of it.
  render(<Counter />);
  fireEvent.click(screen.getByRole("button"));
  expect(screen.getByRole("button").textContent).toBe("pressed 1");
  expect(await openScope).toBe(`act: ${ENDED}`);
  unhold();
});

/* An act scope inside another, opened after an await, when the case runs out. */

let nestedScope: Promise<string> = Promise.resolve("never started");

const nestedRanOut = runsOut(
  "a case that runs out of time inside a scope it opened inside another",
  async () => {
    const never = new Promise<void>(() => {});
    nestedScope = settled(
      act(async () => {
        // AFTER AN AWAIT, so the inner scope begins waiting for the case's
        // end after the outer one does: ended in that order, the outer scope
        // was popped first and React's count was left at the inner one's.
        await Promise.resolve();
        await act(async () => {
          await never;
        });
      }),
    );
    await nestedScope;
  },
);

test("finds both scopes closed, innermost first, so its own renders land", async () => {
  nestedRanOut();
  render(<Counter />);
  fireEvent.click(screen.getByRole("button"));
  expect(screen.getByRole("button").textContent).toBe("pressed 1");
  expect(await nestedScope).toBe(`act: ${ENDED}`);
});

/* A wait inside an act scope when the case runs out. */

let scopedWait: Promise<string> = Promise.resolve("never started");
/** The act environment as the case found it, before its scope and its wait each set it. */
let environmentBefore: unknown = "unread";

const scopedWaitRanOut = runsOut(
  "a case that runs out of time in a wait inside a scope of its own",
  async () => {
    environmentBefore = actEnvironment();
    scopedWait = settled(
      act(async () => {
        await Promise.resolve();
        // The scope set the environment on; the wait sets it off, and each
        // puts back what it found — so only the wait put back first leaves
        // the case's own.
        await screen.findByText("never drawn", undefined, { timeout: 10 * RUNS_OUT });
      }),
    );
    await scopedWait;
  },
);

test("finds the act environment the late case began with", async () => {
  scopedWaitRanOut();
  expect(actEnvironment()).toBe(environmentBefore);
  expect(await scopedWait).toBe(`findBy/waitFor: ${ENDED}`);
});

/* A findBy still polling when its case runs out of time. */

let polling = "pending";
/** The act environment as the first case found it, before its wait set it aside. */
let environment: unknown = "unread";

function actEnvironment(): unknown {
  return (globalThis as { IS_REACT_ACT_ENVIRONMENT?: unknown }).IS_REACT_ACT_ENVIRONMENT;
}

const findByRanOut = runsOut(
  "a case that runs out of time while a findBy of its own polls",
  async () => {
    environment = actEnvironment();
    // The wait's own deadline is the library's second, ten times the case's
    // budget, so it is still polling when the case is given up on.
    const found = settled(screen.findByText("drawn by the next case"));
    void found.then((what) => {
      polling = what;
    });
    await found;
  },
);

test("finds that wait refused, and the act environment it set aside put back", async () => {
  findByRanOut();
  expect(actEnvironment()).toBe(environment);
  render(<p>drawn by the next case</p>);
  // Long enough for the poll to see this page — which, still the first case's,
  // it would have found.
  await answered();
  expect(polling).toBe(`findBy/waitFor: ${ENDED}`);
});

/* A wait and a click a late case would make on the next case's page. */

let releaseLate: () => void = () => {};
let lateWait: Promise<string> = Promise.resolve("never started");
let lateClick: Promise<string> = Promise.resolve("never started");

const movesRanOut = runsOut(
  "a case that runs out of time with a wait and a click still to make",
  async () => {
    const own = new Promise<void>((resolve) => {
      releaseLate = resolve;
    });
    lateWait = settled(
      (async () => {
        await own;
        await screen.findByRole("button", { name: "the next case's" });
      })(),
    );
    lateClick = settled(
      (async () => {
        await own;
        fireEvent.click(screen.getByRole("button", { name: "the next case's" }));
      })(),
    );
    await Promise.all([lateWait, lateClick]);
  },
);

test("is refused both, rather than finding and pressing this case's control", async () => {
  movesRanOut();
  let presses = 0;
  render(<button onClick={() => presses++}>the next case's</button>);
  releaseLate();
  expect(await lateWait).toBe(`findBy/waitFor: ${ENDED}`);
  expect(await lateClick).toBe(`fireEvent: ${ENDED}`);
  expect(presses).toBe(0);
  // And this case's own wait and click are its to make.
  fireEvent.click(await screen.findByRole("button", { name: "the next case's" }));
  expect(presses).toBe(1);
});

/* A render, a rerender, a hook and a cleanup a late case would make on the next case's page. */

let releaseDrawing: () => void = () => {};
let lateRender: Promise<string> = Promise.resolve("never started");
let lateRerender: Promise<string> = Promise.resolve("never started");
let lateHook: Promise<string> = Promise.resolve("never started");
let lateCleanup: Promise<string> = Promise.resolve("never started");

const drawingRanOut = runsOut(
  "a case that runs out of time with a render, a hook and a cleanup still to make",
  async () => {
    const own = new Promise<void>((resolve) => {
      releaseDrawing = resolve;
    });
    const page = render(<p>drawn by the late case</p>);
    const late = (move: () => void) =>
      settled(
        (async () => {
          await own;
          move();
        })(),
      );
    lateRender = late(() => render(<p>drawn late</p>));
    lateRerender = late(() => page.rerender(<p>redrawn late</p>));
    lateHook = late(() => renderHook(() => useState(0)));
    // `cleanup` unmounts EVERY page the library mounted — by then, the next
    // case's.
    lateCleanup = late(() => cleanup());
    await Promise.all([lateRender, lateRerender, lateHook, lateCleanup]);
  },
);

test("is refused all four, and this case's page is its own", async () => {
  drawingRanOut();
  render(<p>drawn by this case</p>);
  releaseDrawing();
  expect(await lateRender).toBe(`render: ${ENDED}`);
  expect(await lateRerender).toBe(`rerender: ${ENDED}`);
  expect(await lateHook).toBe(`renderHook: ${ENDED}`);
  expect(await lateCleanup).toBe(`cleanup: ${ENDED}`);
  expect(screen.queryByText("drawn late")).toBeNull();
  expect(screen.getByText("drawn by this case")).toBeTruthy();
});

/* A poll still out, on a fake clock, when its case runs out of time. */

/** The host's timers, taken as the file loads, before any case fakes them. */
const realSetTimeout = globalThis.setTimeout.bind(globalThis);
/** How often the late poll looks — and how far it moved the fake clock each time. */
const LOOKS_EVERY = 10;
let latePoll: Promise<string> = Promise.resolve("never started");

const pollRanOut = runsOut(
  "a case that runs out of time while a poll of its own looks, on a fake clock",
  async () => {
    vi.useFakeTimers();
    onTestFinished(() => {
      vi.useRealTimers();
    });
    latePoll = settled(
      poll(
        () => {
          throw new Error("not yet");
        },
        { timeout: 10 * RUNS_OUT, interval: LOOKS_EVERY },
      ),
    );
    await latePoll;
  },
);

test("finds that poll refused, and its own fake clock where it left it", async () => {
  pollRanOut();
  vi.useFakeTimers();
  onTestFinished(() => {
    vi.useRealTimers();
  });
  const fired = vi.fn();
  setTimeout(fired, 5 * LOOKS_EVERY);
  // REAL TIME, ten of the late poll's looks: Vitest's `vi.waitFor` moved
  // whatever clock was installed one interval before each, so this case's
  // timer went off with nothing in this case touching its clock.
  await new Promise<void>((resolve) => {
    realSetTimeout(resolve, 10 * LOOKS_EVERY);
  });
  expect(fired).not.toHaveBeenCalled();
  expect(await latePoll).toBe(`poll: ${ENDED}`);
});

/* A call that runs while a case does, from no case's context. */

let wake: () => void = () => {};
// Chained while the file loads, outside every case, so what it calls carries
// no case's context — a case that lost its own looks exactly like this.
const fromNoCase = settled(
  new Promise<void>((resolve) => {
    wake = resolve;
  }).then(() => {
    act(() => {});
  }),
);

test("an act from no case's context, while a case runs, is refused rather than bound to nothing", async () => {
  wake();
  expect(await fromNoCase).toMatch(/^act: called while a case runs but from none's async context/);
});
