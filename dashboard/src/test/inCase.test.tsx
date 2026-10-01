/**
 * What a case does ends with the case.
 *
 * Each pair below is what a timeout leaves behind, without the deadline: the
 * first case ends with something still out — a wait of its OWN that nothing in
 * the harness can see, an `act` scope, a `findBy` — and the case after it
 * draws its own page, releases what the first one waited for, and reads what
 * the late case came to.
 *
 * Through the library alone, every one of these reached the second case: the
 * late `act` ran its body beside it, the open scope held React's act count
 * raised so the second case's own render never landed, the polling `findBy`
 * held the act environment set aside, and the late wait and click found and
 * pressed the second case's control. Each assertion below goes red when the
 * matching half of `inCase.ts` is taken away.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test } from "vitest";
import { act, answered } from "./inCase.ts";

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

/* A late act, and a late flush whose step would move the timers. */

let release: () => void = () => {};
let lateAct: Promise<string> = Promise.resolve("never started");
let lateFlush: Promise<string> = Promise.resolve("never started");
/** Whether a late move's body ran — the move a late case must not make. */
let lateMoved = false;

test("a case that ends while it waits for something of its own", () => {
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
});

test("is refused its next act and its next flush, before either moves", async () => {
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

/* An act scope still open when its case ends. */

let unhold: () => void = () => {};
let openScope: Promise<string> = Promise.resolve("never started");

test("a case that ends inside an act scope of its own", () => {
  const held = new Promise<void>((resolve) => {
    unhold = resolve;
  });
  openScope = settled(
    act(async () => {
      await held;
    }),
  );
});

function Counter() {
  const [presses, setPresses] = useState(0);
  return <button onClick={() => setPresses(presses + 1)}>{`pressed ${presses}`}</button>;
}

test("finds that scope closed when it begins, so its own renders land", async () => {
  // Left open, the scope keeps React's one act count raised, and this render's
  // own act — nested inside it — queues the render and flushes none of it.
  render(<Counter />);
  fireEvent.click(screen.getByRole("button"));
  expect(screen.getByRole("button").textContent).toBe("pressed 1");
  expect(await openScope).toBe(`act: ${ENDED}`);
  unhold();
});

/* A findBy still polling when its case ends. */

let polling = "pending";
/** The act environment as the first case found it, before its wait set it aside. */
let environment: unknown = "unread";

function actEnvironment(): unknown {
  return (globalThis as { IS_REACT_ACT_ENVIRONMENT?: unknown }).IS_REACT_ACT_ENVIRONMENT;
}

test("a case that ends while a findBy of its own polls", () => {
  environment = actEnvironment();
  void settled(screen.findByText("drawn by the next case")).then((what) => {
    polling = what;
  });
});

test("finds that wait refused, and the act environment it set aside put back", async () => {
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

test("a case that ends with a wait and a click still to make", () => {
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
});

test("is refused both, rather than finding and pressing this case's control", async () => {
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
