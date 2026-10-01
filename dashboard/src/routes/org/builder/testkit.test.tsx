/**
 * What the Builder suites' own harness promises about a case that ended with
 * a wait still out: the wait stops before the next case begins.
 *
 * A CASE THAT TIMES OUT IS NOT STOPPED. Vitest fails it and starts the next,
 * but its function is a promise nothing can cancel, and it goes on running
 * beside the cases after it, waking at whatever it was waiting for. Through
 * the harness, what it did next was an `act` — and React keeps one act scope
 * count for the process, so two cases' scopes interleaving left every later
 * case in the file queueing renders it never flushed. One case that timed out
 * on a loaded runner failed the rest of its file as "the Builder draws no
 * toolbar".
 *
 * Each pair below is a case that ENDS WITH A WAIT STILL OUT — started and not
 * awaited, which is where a timeout leaves a case, without the deadline — and
 * the case after it, which reads what that wait came to and mounts a lens of
 * its own.
 */

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { company, Engine, mountBuilder, pressInToolbar, pressInView } from "./testkit.tsx";
import { waitInCase } from "./viewTestkit.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** What a wait came to, once it came to anything: "returned", or why it refused. */
function outcome(wait: Promise<void>): Promise<string> {
  return wait.then(
    () => "returned",
    (cause: unknown) => (cause instanceof Error ? cause.message : String(cause)),
  );
}

/** What each case's wait came to, by case; read by the case after it. */
const left: Record<string, Promise<string>> = {};

/** The refusal a wait out past its case's end comes to. */
const ended = (what: string) => new RegExp(`^${what}: the case that mounted this lens has ended`);

/** Mounts a fresh lens, and holds it to drawing and settling. */
async function nextLensDraws(hash?: string): Promise<void> {
  const { checked } = mountBuilder({ engine: new Engine(company()), ...(hash ? { hash } : {}) });
  await checked();
}

// A READ NOBODY ANSWERS. The lens gives it up when it unmounts, so the scope
// closes on its own — and the settle went on, returning into a test body that
// would have acted on the next case's page.
test("a case that ends while it settles on a read", () => {
  const engine = new Engine(company());
  engine.script = (r) => (r.method === "GET" && r.path === "/chart" ? new Promise(() => {}) : null);
  const { settle } = mountBuilder({ engine });
  left.read = outcome(settle());
});

test("stops at that settle, and the next case's lens draws", async () => {
  expect(await left.read).toMatch(ended("settle"));
  await nextLensDraws();
});

// A WRITE NOBODY ANSWERS. A save is never abandoned when the lens unmounts,
// so nothing but the case's end can close the act scope that waits for it.
test("a case that ends while it settles on a save", async () => {
  const engine = new Engine(company());
  const { settle, checked } = mountBuilder({ engine });
  await checked();
  pressInView("Edit CEO");
  await checked();
  pressInToolbar("Review and save");
  engine.script = (r) => (r.method === "PATCH" ? new Promise(() => {}) : null);
  const review = screen.getByRole("dialog", { name: "Review and save" });
  fireEvent.click(within(review).getByRole("button", { name: "Save" }));
  // The write is what is out when the case ends, not the read before it.
  await engine.reached(() => engine.chartWrites().length > 0);
  left.write = outcome(settle());
});

test("stops at that settle too", async () => {
  expect(await left.write).toMatch(ended("settle"));
  await nextLensDraws();
});

/** A lens address the default mount is not on: the landing below never reached. */
const ON_THE_TABLE = "#/company?lens=builder&view=table";

// A MOVE THAT NEVER LANDS. The next mount writes a hash of its own, which a
// wait still out would take for the landing it wanted and settle the next
// case's lens.
test("a case that ends while it waits for a move to land", async () => {
  const { checked, navigate } = mountBuilder({ engine: new Engine(company()) });
  await checked();
  left.move = outcome(navigate(() => {}, ON_THE_TABLE));
});

test("stops at that move, whatever the next mount writes", async () => {
  expect(await left.move).toMatch(ended("navigate"));
  await nextLensDraws(ON_THE_TABLE);
});

// A REQUEST THAT NEVER ARRIVES. The wrapper that waits for it turns React's
// act environment off until its wait ends, so one left waiting kept it off.
test("a case that ends while it waits for a request", async () => {
  const engine = new Engine(company());
  const { checked } = mountBuilder({ engine });
  await checked();
  left.request = outcome(engine.reached(() => false));
});

test("stops at that wait for a request", async () => {
  expect(await left.request).toMatch(ended("reached"));
  await nextLensDraws();
});

// A WAIT OF THE CASE'S OWN, and then a settle. The harness cannot wake a wait
// it does not own, so a case that timed out inside one resumes whenever that
// wait ends — after the next case has mounted — and its next settle has to be
// refused there rather than settle whichever lens is mounted by then.
let release: () => void = () => {};

test("a case that ends while it waits for something of its own", async () => {
  const { settle, checked } = mountBuilder({ engine: new Engine(company()) });
  await checked();
  const own = new Promise<void>((resolve) => {
    release = resolve;
  });
  left.own = outcome(
    (async () => {
      await own;
      await settle();
    })(),
  );
});

test("stops at its next settle, and never settles the next case's lens", async () => {
  // THE NEXT CASE'S LENS IS MOUNTED FIRST: this is the lens a settle that
  // found its lens anywhere but in its own case would take.
  const { checked } = mountBuilder({ engine: new Engine(company()) });
  release();
  expect(await left.own).toMatch(ended("settle"));
  await checked();
});

// A WAIT OUTSIDE THE LENS. A suite that mounts no lens through this harness —
// the company screen's, the node editor's — waits for the page with
// [waitInCase], and the same holds: the next case's page does what such a
// wait listens for (a mount writes a hash, a lens draws its toolbar), so one
// still listening takes it and hands the next case's page to a case that
// already failed.
let stoppedListening = false;

test("a case that ends while it waits for something the page does", () => {
  left.event = outcome(
    waitInCase<void>("the landing", (done) => {
      const moved = () => {
        if (location.hash === ON_THE_TABLE) done();
      };
      window.addEventListener("hashchange", moved);
      return () => {
        stoppedListening = true;
        window.removeEventListener("hashchange", moved);
      };
    }),
  );
});

test("stops at that wait and stops listening, whatever the next mount writes", async () => {
  // Mounted FIRST, so the hash this mount writes is the landing that wait
  // would take if it were still listening.
  await nextLensDraws(ON_THE_TABLE);
  expect(await left.event).toMatch(/^the landing: the case that waited for it has ended/);
  expect(stoppedListening).toBe(true);
});

// AND A GESTURE THAT THROWS ENDS ITS WAIT AT ONCE. The case fails on the
// gesture's own error, and nothing awaits the wait any more: left listening,
// its refusal at the case's end would be a rejection nobody handles, which
// the runner reports as an error of the whole run.
test("a gesture that throws stops its wait and fails with its own error", async () => {
  let listening = false;
  const wait = waitInCase<void>(
    "the landing",
    () => {
      listening = true;
      return () => {
        listening = false;
      };
    },
    () => {
      throw new Error("no such control");
    },
  );
  await expect(wait).rejects.toThrow("no such control");
  expect(listening).toBe(false);
});
