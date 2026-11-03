/**
 * After a save, the builder follows the revision until every node has applied
 * it, or says which node refused it, and the org chart admits that it still
 * draws the revision before it.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FleetAnswer } from "~/protocol/index.ts";
import { Teams } from "~/routes/agents/Company.tsx";
import { OrgChart } from "~/routes/agents/OrgChart.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { AfterSaveStrip, applyState } from "./AfterSaveStrip.tsx";
import {
  clearSavedRevision,
  recordSavedRevision,
  useSavedRevision,
  type SavedRevision,
} from "./savedRevision.ts";
import { company, EDITOR, Engine, InertWebSocket, mountBuilder } from "./testkit.tsx";
import { toastText } from "~/testing.tsx";
import { healthFrame } from "~/test/health.ts";
import type { EngineHealth } from "~/contract/health.ts";
import { withDerived } from "~/test/org.ts";

beforeEach(() => {
  sessionStorage.clear();
  clearSavedRevision();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  clearSavedRevision();
  location.hash = "#/";
});

const node = (over: Partial<FleetAnswer["nodes"][number]> & { id: string }) => ({
  roles: ["seats"],
  broker: "member",
  seats: 1,
  ...over,
});

/**
 * A fleet answer whose target is `target`, activating `revision`, as the
 * engine renders one: the epoch and the activation come from one read of the
 * pointer, and an answer with no target (0) carries no activation.
 */
const fleet = (nodes: FleetAnswer["nodes"], target: number, revision = "r-saved"): FleetAnswer => ({
  nodes,
  seats: [],
  duties: [],
  unplaceable: [],
  unmanned_roles: [],
  this_node: nodes[0]?.id ?? "",
  target_epoch: target,
  activation: target === 0 ? null : { epoch: target, revision_id: revision, at: "", summary: "" },
});

describe("what the strip says", () => {
  const saved = { revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 };

  test("a node that has applied the epoch, with no fleet answer, is applied", () => {
    expect(applyState(saved, healthFrame({ applied_epoch: 4 }), null, true)).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
    expect(applyState(saved, healthFrame({ applied_epoch: 3 }), null, true)).toMatchObject({
      message: "The engine is applying it.",
      resolved: false,
    });
  });

  // A LEAD WITHOUT ADMIN IS ASKED NOTHING OF THE FLEET, and a save with its
  // own epoch is followed by this node's health push alone.
  test("a reader who may not read the fleet follows a save with an epoch by this node", () => {
    expect(applyState(saved, healthFrame({ applied_epoch: 3 }), null, false)).toMatchObject({
      tone: "info",
      message: "The engine is applying it.",
      resolved: false,
    });
    expect(applyState(saved, healthFrame({ applied_epoch: 4 }), null, false)).toMatchObject({
      tone: "positive",
      message: "Applied.",
      resolved: true,
    });
  });

  // A REFUSAL HERE IS SAID AS ONE. The applied epoch alone cannot say it — a
  // failed attempt leaves it where it was — so a lead whose save this node
  // refused was told the engine was applying it for as long as the tab stayed
  // open. This node's last attempt says it, matched on the revision too.
  // Mutations: drop the refusal from nodeStanding and the first case applies
  // for good; match the attempt on its epoch alone and the second refuses.
  test("a reader who may not read the fleet is told this node refused the save", () => {
    const refusedHere = healthFrame({
      applied_epoch: 3,
      config_attempt: { epoch: 4, revision_id: "r-saved", status: "error" },
    });
    expect(applyState(saved, refusedHere, null, false)).toEqual({
      tone: "critical",
      message: "This node refused this revision and goes on running the one before.",
      resolved: true,
      showFleet: false,
    });
    // One epoch number, another revision: a coordination store rebuilt under
    // the fleet restarted the count, and that attempt is not about this save.
    const another = healthFrame({
      applied_epoch: 3,
      config_attempt: { epoch: 4, revision_id: "r-other", status: "error" },
    });
    expect(applyState(saved, another, null, false)).toMatchObject({
      tone: "info",
      message: "The engine is applying it.",
      resolved: false,
    });
    // An attempt at a later epoch, this one never applied: the node has
    // passed the save and will not come back to it.
    const passed = healthFrame({
      applied_epoch: 3,
      config_attempt: { epoch: 5, revision_id: "r-later-0001", status: "error" },
    });
    expect(applyState(saved, passed, null, false)).toEqual({
      tone: "neutral",
      message: "Revision r-later-00 has been activated since.",
      resolved: true,
      showFleet: false,
    });
    // A RUNNING NODE KEEPS THE EPOCH IT APPLIED UNDER THE PREVIOUS POINTER,
    // so after a rebuilt coordination store restarted the count an applied
    // epoch of 41 stands beside a refusal of this save at 4. Mutation:
    // compare the applied epoch before reading the attempt and this reads
    // "Applied.".
    const restarted = healthFrame({
      applied_epoch: 41,
      config_attempt: { epoch: 4, revision_id: "r-saved", status: "error" },
    });
    expect(applyState(saved, restarted, null, false)).toMatchObject({
      tone: "critical",
      message: "This node refused this revision and goes on running the one before.",
    });
  });

  test("a fleet part way through says how far", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({ id: "b", config_epoch: 3, config_status: "ok" }),
      ],
      4,
    );
    expect(applyState(saved, null, answer, true)).toMatchObject({
      message: "Applied on 1 of 2 nodes.",
      resolved: false,
    });
  });

  // A NODE THAT REFUSED IS THE WHOLE POINT: the revision is active and the
  // node goes on serving the previous one, which nothing else on this screen
  // would show.
  test("a node that refused the revision is named, with its reason", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({
          id: "b",
          config_epoch: 4,
          config_status: "error",
          config_error: "provider openai: model is required",
          config_revision_id: "r-saved",
          config_serving_revision_id: "r1",
        }),
      ],
      4,
    );
    expect(applyState(saved, null, answer, true)).toMatchObject({
      tone: "critical",
      message: "Node b refused this revision: provider openai: model is required.",
      resolved: true,
      showFleet: true,
    });
  });

  // A node records the epoch it ATTEMPTED beside the outcome, so a failure
  // names the revision it failed on. Read without that match, a node that
  // refused an earlier revision would be reported as refusing this one.
  test("a node that refused another epoch is not read as refusing this one", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({
          id: "b",
          config_epoch: 3,
          config_status: "error",
          config_error: "provider openai: model is required",
        }),
      ],
      4,
    );
    expect(applyState(saved, null, answer, true)).toMatchObject({
      tone: "info",
      message: "Applied on 1 of 2 nodes.",
      resolved: false,
    });
  });

  // The strip follows ONE revision. A node reporting a later epoch it did
  // not apply, and running another revision, has passed this one without
  // running it: waiting on it would leave the strip applying for ever, and
  // counting it applied called a save Applied that it never ran. Said as the
  // lead's strip says it of this node, by the one rule (`fleetNodeStanding`).
  // Mutation: count a later attempt as applied whatever its status, and this
  // reads "Applied.".
  test("a node that went past the save to a later epoch it refused has passed it", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({
          id: "b",
          config_epoch: 5,
          config_status: "degraded",
          config_error: "mcp child",
          config_revision_id: "r-later-0001",
          config_serving_revision_id: "r1",
        }),
      ],
      5,
      "r-later-0001",
    );
    expect(applyState(saved, null, answer, true)).toEqual({
      tone: "neutral",
      message: "Revision r-later-00 has been activated since.",
      resolved: true,
      showFleet: false,
    });
  });

  // A SECOND ACTIVATION INSIDE ONE RECONCILE INTERVAL IS NEVER ATTEMPTED at
  // the epoch before it: every node went straight to 5, refused it, and runs
  // the revision before the save. None ran the save, and none will. Mutation:
  // count a later attempt as applied whatever its status, and this reads
  // "Applied." and stops the polling with no node running it.
  test("a fleet that skipped the save and refused the next one is not told it applied", () => {
    const skipped = (id: string) =>
      node({
        id,
        config_epoch: 5,
        config_status: "error",
        config_error: "provider openai: model is required",
        config_revision_id: "r-later-0001",
        config_serving_revision_id: "r1",
      });
    const answer = fleet([skipped("a"), skipped("b")], 5, "r-later-0001");
    expect(applyState(saved, null, answer, true)).toEqual({
      tone: "neutral",
      message: "Revision r-later-00 has been activated since.",
      resolved: true,
      showFleet: false,
    });
  });

  // APPLIED ON EVIDENCE: a node that runs the saved revision ran it, whatever
  // it made of a later one since — node b restarted on it, so the epoch it
  // applied went with the process — and one that applied a later epoch runs
  // past it. Mutations: drop the serving revision from the rule and node b
  // reads as passed; leave the applied epoch out of a row's report and so
  // does node c.
  test("a node that runs the saved revision, or applied a later epoch, has applied it", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({
          id: "b",
          config_epoch: 5,
          config_status: "error",
          config_error: "env var CREWLET_X is not set",
          config_revision_id: "r-later",
          config_serving_revision_id: "r-saved",
        }),
        node({
          id: "c",
          config_epoch: 5,
          config_status: "ok",
          config_revision_id: "r-later",
          config_serving_revision_id: "r-later",
          config_applied_epoch: 5,
        }),
      ],
      5,
      "r-later",
    );
    expect(applyState(saved, null, answer, true)).toEqual({
      tone: "positive",
      message: "Applied.",
      resolved: true,
      showFleet: false,
    });
  });

  // SKIPPED THE SAVE, APPLIED THE NEXT, REFUSED THE ONE AFTER: the node runs
  // past the save, and an Admin reading its fleet row is told what a lead
  // reading its push is. Judged by a rule of its own that saw only the epoch
  // a row last attempted, the fleet path called this node passed and told
  // the Admin "activated since" while the lead was told "Applied.".
  // Mutation: leave the applied epoch out of a row's report and the Admin's
  // reads "Revision r-third has been activated since.".
  test("a node that applied a later epoch and refused a still later one has applied it, for Admin and lead alike", () => {
    const answer = fleet(
      [
        node({
          id: "a",
          config_epoch: 6,
          config_status: "error",
          config_error: "env var CREWLET_X is not set",
          config_revision_id: "r-third",
          config_serving_revision_id: "r-later",
          config_applied_epoch: 5,
        }),
      ],
      6,
      "r-third",
    );
    const push = healthFrame({
      applied_epoch: 5,
      config_attempt: { epoch: 6, revision_id: "r-third", status: "error" },
    });
    const applied = { tone: "positive", message: "Applied.", resolved: true, showFleet: false };
    expect(applyState(saved, null, answer, true)).toEqual(applied);
    expect(applyState(saved, push, null, false)).toEqual(applied);
  });

  // A NODE THAT PASSED THE SAVE IS NOT COUNTED AMONG THOSE THAT APPLIED IT,
  // and one still to report keeps the strip asking. Mutation: count the
  // passed node and this reads "Applied on 2 of 3 nodes.".
  test("a node that passed the save is done with it, but not counted as applying it", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({ id: "b", config_epoch: 5, config_status: "error", config_revision_id: "r-later" }),
        node({ id: "c", config_epoch: 3, config_status: "ok", config_revision_id: "r1" }),
      ],
      5,
      "r-later",
    );
    expect(applyState(saved, null, answer, true)).toMatchObject({
      tone: "info",
      message: "Applied on 1 of 3 nodes.",
      resolved: false,
    });
  });

  // ONE EPOCH NUMBER CAN NAME TWO REVISIONS once a rebuilt coordination
  // store restarts the count: a node's report on the other one is neither
  // this save applied nor this save refused. Mutation: drop the revision
  // match from `attempted` and node b reads as applied, or as refusing it.
  test("a report at this epoch on another revision is about that revision", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-saved" }),
        node({ id: "b", config_epoch: 4, config_status: "ok", config_revision_id: "r-other" }),
        node({
          id: "c",
          config_epoch: 4,
          config_status: "error",
          config_error: "not this save",
          config_revision_id: "r-other",
        }),
      ],
      4,
    );
    expect(applyState(saved, null, answer, true)).toMatchObject({
      tone: "info",
      message: "Applied on 1 of 3 nodes.",
      resolved: false,
    });
  });

  describe("a save whose answer was lost", () => {
    const lost = { revisionId: "r-saved", parentRevisionId: "r1", epoch: null };

    test("takes its epoch from the activation that names it", () => {
      const answer = fleet(
        [node({ id: "a", config_epoch: 7, config_status: "ok", config_revision_id: "r-saved" })],
        7,
      );
      expect(applyState(lost, null, answer, true)).toMatchObject({
        message: "Applied.",
        resolved: true,
      });
    });

    // THE HISTORY WALK FINDS A LOST WRITE FROM A LATER REVISION as readily as
    // from itself, and the fleet's target is then that later revision's
    // epoch. Read as this save's, every node converged on the later revision
    // counts as neither applied nor refused, and the strip applies until the
    // next activation. Mutation: take the epoch from `target_epoch` and this
    // reads "The engine is applying it." for ever. Said as activated SINCE,
    // never as replacing the save: the later revision is as often one built
    // on it, carrying its changes.
    test("one a later activation has passed says so, and settles", () => {
      const answer = fleet(
        [
          node({ id: "a", config_epoch: 8, config_status: "ok", config_revision_id: "r-later" }),
          node({ id: "b", config_epoch: 8, config_status: "ok", config_revision_id: "r-later" }),
        ],
        8,
        "r-later",
      );
      expect(applyState(lost, null, answer, true)).toEqual({
        tone: "neutral",
        message: "Revision r-later has been activated since.",
        resolved: true,
        showFleet: false,
      });
    });

    // AN ANSWER WITH NO ACTIVATION HAS NO TARGET: nothing activated, or a
    // pointer the engine could not read (`fleet_activation_unavailable`),
    // both answered as `target_epoch` 0. Mutation: compare against that 0
    // and every node that has reported anything is past it — Applied, and
    // the polling stops, with nothing saying any node applied this save.
    test("with no activation in the answer keeps asking rather than claiming it applied", () => {
      const answer = fleet(
        [
          node({ id: "a", config_epoch: 3, config_status: "ok", config_revision_id: "r1" }),
          node({ id: "b", config_epoch: 3, config_status: "ok", config_revision_id: "r1" }),
        ],
        0,
      );
      expect(answer.activation).toBeNull();
      expect(applyState(lost, healthFrame({ applied_epoch: 3 }), answer, true)).toEqual({
        tone: "info",
        message: "The engine is applying it.",
        resolved: false,
        showFleet: false,
      });
    });

    // A LEAD WITHOUT ADMIN READS NO FLEET, so no activation will ever name
    // this save's epoch, and this node's applied epoch is a number with
    // nothing to compare it against. Mutation: let a reader who may not read
    // the fleet wait like one whose answer is out, and the strip says the
    // engine is applying it for good, polling nothing, whatever the node did.
    test("for a reader who may not read the fleet says it cannot be followed, and settles", () => {
      const unfollowable = {
        tone: "neutral",
        message:
          "Whether it has been applied can’t be followed from here. The org chart shows its changes once this node applies them.",
        resolved: true,
        showFleet: false,
      };
      expect(applyState(lost, healthFrame({ applied_epoch: 9 }), null, false)).toEqual(
        unfollowable,
      );
      // THIS NODE'S ATTEMPT AT THE SAVED REVISION supplies its epoch, so the
      // strip follows it from there. Mutation: leave a lead's lost save
      // unfollowable whatever the node attempted.
      const tried = (status: string, applied: number) =>
        healthFrame({
          applied_epoch: applied,
          config_attempt: { epoch: 10, revision_id: lost.revisionId, status },
        });
      expect(applyState(lost, tried("ok", 10), null, false)).toMatchObject({
        tone: "positive",
        message: "Applied.",
      });
      expect(applyState(lost, tried("error", 9), null, false)).toMatchObject({
        tone: "critical",
        resolved: true,
      });
      // An Admin whose fleet answer is still out is waited on instead.
      expect(applyState(lost, healthFrame({ applied_epoch: 9 }), null, true)).toMatchObject({
        tone: "info",
        message: "The engine is applying it.",
        resolved: false,
      });
    });
  });
});

describe("the strip on screen", () => {
  /**
   * Mounts the strip alone, this node behind every save here (applied epoch
   * 3), with the socket answering each question through `answer` and
   * recording what it was asked.
   */
  function mountStrip(answer: (what: string) => Promise<unknown>) {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    const store = new Store();
    store.applyHealth(healthFrame({ applied_epoch: 3 }));
    const socket = new LiveSocket(store);
    const asked: string[] = [];
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
      asked.push(what);
      return answer(what);
    };
    const strip = (saved: SavedRevision) => (
      <ClientContext.Provider value={{ store, socket }}>
        <ViewerProvider>
          <Router>
            <AfterSaveStrip saved={saved} onDismiss={() => {}} />
          </Router>
        </ViewerProvider>
      </ClientContext.Provider>
    );
    // THE STRIP AS THE BUILDER MOUNTS IT, handed the tab's saved revision
    // from its store, so an epoch the strip pins there reaches it.
    const recorded = () => (
      <ClientContext.Provider value={{ store, socket }}>
        <ViewerProvider>
          <Router>
            <FromTheStore />
          </Router>
        </ViewerProvider>
      </ClientContext.Provider>
    );
    return { strip, recorded, asked, store };
  }

  function FromTheStore() {
    const saved = useSavedRevision();
    return saved ? <AfterSaveStrip saved={saved} onDismiss={() => {}} /> : null;
  }

  const unfollowable = /can’t be followed from here/;

  // ONE MOUNT PER SAVED REVISION. The strip kept across saves went on reading
  // the answer it held for the previous save until its next read came back,
  // and a save with no epoch of its own takes the activation in that answer
  // at its word. Mutation: render the strip without its key and the previous
  // save's answer reports a later revision activated since this one, and
  // stops the polling.
  test("an answer read for the previous save says nothing about the next one", async () => {
    let fleetAnswer: Promise<FleetAnswer> = Promise.resolve(
      fleet(
        [node({ id: "a", config_epoch: 4, config_status: "ok", config_revision_id: "r-first" })],
        4,
        "r-first",
      ),
    );
    // Behind both saves, so only the fleet's answer can resolve the first.
    const { strip } = mountStrip((what) =>
      what === "viewer"
        ? Promise.resolve(EDITOR)
        : what === "fleet"
          ? fleetAnswer
          : Promise.resolve(null),
    );

    const view = render(strip({ revisionId: "r-first", parentRevisionId: "r1", epoch: 4 }));
    expect(await screen.findByText("Applied.")).toBeDefined();

    // The next save's answer was lost, and the fleet has not answered since.
    let answer: (fleet: FleetAnswer) => void = () => {};
    fleetAnswer = new Promise((resolve) => {
      answer = resolve;
    });
    view.rerender(strip({ revisionId: "r-second", parentRevisionId: "r-first", epoch: null }));
    expect(await screen.findByText("The engine is applying it.")).toBeDefined();
    expect(screen.queryByText(/activated since/)).toBeNull();

    act(() =>
      answer(
        fleet(
          [node({ id: "a", config_epoch: 5, config_status: "ok", config_revision_id: "r-second" })],
          5,
          "r-second",
        ),
      ),
    );
    expect(await screen.findByText("Applied.")).toBeDefined();
  });

  // A LEAD READS NO FLEET, so a save of theirs whose answer was lost has
  // nothing to be followed by: said to be applying, it stayed so for as long
  // as the strip stood, whatever this node or any other did with it.
  test("a lead's save without an epoch says it cannot be followed, asking no fleet", async () => {
    const lead = { login: "dev.person", owner: "dev", handle: "dev", grants: [] };
    const { strip, asked } = mountStrip((what) => Promise.resolve(what === "viewer" ? lead : null));
    render(strip({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null }));
    expect(await screen.findByText(unfollowable)).toBeDefined();
    expect(screen.queryByText("The engine is applying it.")).toBeNull();
    expect(asked).toContain("viewer");
    expect(asked).not.toContain("fleet");
  });

  // WHO IS READING IS NOT KNOWN UNTIL THE VIEWER ANSWER ARRIVES, and until
  // then the strip waits as on any answer still out. Mutation: read a reader
  // not known yet as one who may not read the fleet, and an Admin is told
  // the save cannot be followed before their fleet answer is even asked for.
  test("an Admin not known yet is not told the save cannot be followed", async () => {
    let known: (viewer: unknown) => void = () => {};
    const viewer = new Promise((resolve) => {
      known = resolve;
    });
    const { strip } = mountStrip((what) =>
      what === "viewer"
        ? viewer
        : what === "fleet"
          ? Promise.resolve(
              fleet(
                [
                  node({
                    id: "a",
                    config_epoch: 5,
                    config_status: "ok",
                    config_revision_id: "r-saved",
                  }),
                ],
                5,
              ),
            )
          : Promise.resolve(null),
    );
    render(strip({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null }));
    expect(await screen.findByText("The engine is applying it.")).toBeDefined();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByText(unfollowable)).toBeNull();

    act(() => known(EDITOR));
    expect(await screen.findByText("Applied.")).toBeDefined();
  });

  describe("an epoch learned for a save whose answer was lost", () => {
    const lead = { login: "dev.person", owner: "dev", handle: "dev", grants: [] };
    const attempt = (applied: number, epoch: number, revision_id: string, status: string) =>
      healthFrame({ applied_epoch: applied, config_attempt: { epoch, revision_id, status } });

    // LEARNED ONCE AND KEPT. This node's attempt names the saved revision
    // only until the next activation; re-read from it on every push, an
    // applied save turned into one that "can't be followed from here" the
    // moment a colleague activated anything. Mutation: drop the strip's
    // pin and this reads the unfollowable sentence after the second push.
    test("a lead's applied save stays applied when this node moves on", async () => {
      const { recorded, store } = mountStrip((what) =>
        Promise.resolve(what === "viewer" ? lead : null),
      );
      recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null });
      render(recorded());
      act(() => store.applyHealth(attempt(12, 12, "r-saved", "ok")));
      expect(await screen.findByText("Applied.")).toBeDefined();
      act(() => store.applyHealth(attempt(13, 13, "r-later", "ok")));
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(screen.getByText("Applied.")).toBeDefined();
      expect(screen.queryByText(unfollowable)).toBeNull();
    });

    // A REFUSAL IS NOT ERASED into "can't be followed" either: kept at the
    // epoch it was refused at, the save is one a later activation has
    // passed. Mutation: drop the pin and this reads the unfollowable
    // sentence.
    test("a lead's refused save is passed, not unfollowable, when this node moves on", async () => {
      const { recorded, store } = mountStrip((what) =>
        Promise.resolve(what === "viewer" ? lead : null),
      );
      recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null });
      render(recorded());
      act(() => store.applyHealth(attempt(11, 12, "r-saved", "error")));
      expect(await screen.findByText(/This node refused this revision/)).toBeDefined();
      act(() => store.applyHealth(attempt(11, 13, "r-later-0001", "error")));
      expect(
        await screen.findByText("Revision r-later-00 has been activated since."),
      ).toBeDefined();
      expect(screen.queryByText(unfollowable)).toBeNull();
    });

    // AN ADMIN'S EPOCH COMES FROM THE FLEET'S ACTIVATION, which names the
    // saved revision only until the next one: a reconnect that read the
    // fleet again turned an applied save into one activated since.
    // Mutation: drop the strip's pin and the second answer reads
    // "Revision r-later has been activated since.".
    test("an Admin's applied save stays applied when the fleet is read after the next activation", async () => {
      let fleetAnswer = fleet(
        [node({ id: "a", config_epoch: 7, config_status: "ok", config_revision_id: "r-saved" })],
        7,
      );
      const { recorded, store, asked } = mountStrip((what) =>
        what === "viewer"
          ? Promise.resolve(EDITOR)
          : what === "fleet"
            ? Promise.resolve(fleetAnswer)
            : Promise.resolve(null),
      );
      recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null });
      render(recorded());
      expect(await screen.findByText("Applied.")).toBeDefined();

      fleetAnswer = fleet(
        [
          node({
            id: "a",
            config_epoch: 8,
            config_status: "ok",
            config_revision_id: "r-later",
            config_applied_epoch: 8,
          }),
        ],
        8,
        "r-later",
      );
      const reads = asked.filter((what) => what === "fleet").length;
      // A reconnect asks the fleet again.
      act(() => store.setConnected(true));
      await waitFor(() =>
        expect(asked.filter((what) => what === "fleet").length).toBeGreaterThan(reads),
      );
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(screen.getByText("Applied.")).toBeDefined();
      expect(screen.queryByText(/activated since/)).toBeNull();
    });
  });

  // A RECONNECT READS THE FLEET AGAIN, and a node that applied a later epoch
  // before refusing a still later one has not taken the save back. Judged by
  // the epoch a row last attempted, an Admin's "Applied." became "activated
  // since" on that read. Mutation: leave the applied epoch out of a row's
  // report and the second answer reads "Revision r-third has been activated
  // since.".
  test("an Admin's applied save stays applied when a reconnect reads a node gone on past it", async () => {
    let fleetAnswer = fleet(
      [
        node({
          id: "a",
          config_epoch: 4,
          config_status: "ok",
          config_revision_id: "r-saved",
          config_applied_epoch: 4,
        }),
      ],
      4,
    );
    const { strip, store, asked } = mountStrip((what) =>
      what === "viewer"
        ? Promise.resolve(EDITOR)
        : what === "fleet"
          ? Promise.resolve(fleetAnswer)
          : Promise.resolve(null),
    );
    render(strip({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 }));
    expect(await screen.findByText("Applied.")).toBeDefined();

    fleetAnswer = fleet(
      [
        node({
          id: "a",
          config_epoch: 6,
          config_status: "error",
          config_error: "env var CREWLET_X is not set",
          config_revision_id: "r-third",
          config_serving_revision_id: "r-later",
          config_applied_epoch: 5,
        }),
      ],
      6,
      "r-third",
    );
    const reads = asked.filter((what) => what === "fleet").length;
    act(() => store.setConnected(true));
    await waitFor(() =>
      expect(asked.filter((what) => what === "fleet").length).toBeGreaterThan(reads),
    );
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.getByText("Applied.")).toBeDefined();
    expect(screen.queryByText(/activated since/)).toBeNull();
  });

  // A DROPPED SOCKET CLEARS THE HEALTH SLICE, and says nothing about the
  // save: read as this node's standing, the missing push was "applying", and
  // a lead's refusal, or "Applied.", went back to "The engine is applying
  // it." for as long as the socket was down. Mutation: read the live slice
  // (`useEngineHealth`) in the strip and both verdicts read applying.
  test("a lead's verdict stays where this node's last push put it through a dropped socket", async () => {
    const lead = { login: "dev.person", owner: "dev", handle: "dev", grants: [] };
    const { strip, store } = mountStrip((what) => Promise.resolve(what === "viewer" ? lead : null));
    render(strip({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 }));
    const said = (applied: number, status: string) =>
      healthFrame({
        applied_epoch: applied,
        config_attempt: { epoch: 4, revision_id: "r-saved", status },
      });

    act(() => store.applyHealth(said(3, "error")));
    expect(await screen.findByText(/This node refused this revision/)).toBeDefined();
    act(() => store.setConnected(false));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.getByText(/This node refused this revision/)).toBeDefined();
    expect(screen.queryByText("The engine is applying it.")).toBeNull();

    // Back, and this node applied it on a retry since.
    act(() => store.applyHealth(said(4, "ok")));
    expect(await screen.findByText("Applied.")).toBeDefined();
    act(() => store.setConnected(false));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.getByText("Applied.")).toBeDefined();
    expect(screen.queryByText("The engine is applying it.")).toBeNull();
  });

  // THE STRIP MOUNTS AFRESH on every return to the Builder, and the save it
  // follows is the tab's: so is what this node last said of it. Kept per
  // mount, a strip mounted while the socket was down read no push, nor a
  // fleet answer, which goes with the mount and does not come while the
  // engine is gone — and took its "Applied." back to "The engine is applying
  // it.", for a lead and an Admin alike. Mutation: keep the last push per
  // mount (`useState` in `useLastHealth`) and both read applying.
  test.each([
    ["a lead", { login: "dev.person", owner: "dev", handle: "dev", grants: [] }],
    ["an Admin", EDITOR],
  ])("%s's strip mounted again while the socket is down keeps its Applied.", async (_, viewer) => {
    let engineGone = false;
    const { strip, store } = mountStrip((what) =>
      what === "viewer"
        ? Promise.resolve(viewer)
        : what !== "fleet"
          ? Promise.resolve(null)
          : engineGone
            ? new Promise(() => {})
            : Promise.resolve(
                fleet(
                  [
                    node({
                      id: "a",
                      config_epoch: 4,
                      config_status: "ok",
                      config_revision_id: "r-saved",
                      config_applied_epoch: 4,
                    }),
                  ],
                  4,
                ),
              ),
    );
    const saved = { revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 };
    act(() =>
      store.applyHealth(
        healthFrame({
          applied_epoch: 4,
          config_attempt: { epoch: 4, revision_id: "r-saved", status: "ok" },
        }),
      ),
    );
    const builder = render(strip(saved));
    expect(await screen.findByText("Applied.")).toBeDefined();

    // Off to another screen; the engine goes; back to the Builder.
    builder.unmount();
    act(() => store.setConnected(false));
    engineGone = true;
    render(strip(saved));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.getByText("Applied.")).toBeDefined();
    expect(screen.queryByText("The engine is applying it.")).toBeNull();
  });
});

describe("in the builder", () => {
  async function save(engine: Engine, query: (what: string) => unknown) {
    const { store, checkChange } = mountBuilder({ engine, query });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    return store;
  }

  // THIS NODE'S EPOCH ARRIVES ON THE HEALTH PUSH — the strip asks for nothing
  // to learn it, so the tick moving it is what resolves the strip.
  test("the strip follows the saved revision and offers the diff", async () => {
    const engine = new Engine(company());
    const store = await save(engine, () => null);
    act(() => store.applyHealth(healthFrame({ applied_epoch: 1 })));
    expect(await screen.findByText("The engine is applying it.")).toBeDefined();
    expect(screen.getByText("r-saved")).toBeDefined();
    // What the save changed is the saved revision against the one it was
    // built on. Against the active revision, which the save now is, the diff
    // would be empty.
    expect(screen.getByRole("link", { name: "View changes" }).getAttribute("href")).toBe(
      "#/settings/config?lens=diff&revision=r-saved&against=r1",
    );
    act(() => store.applyHealth(healthFrame({ applied_epoch: 2 })));
    expect(await screen.findByText("Applied.")).toBeDefined();
  });

  test("Copy as YAML reads the company as YAML rather than as a refusal", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("format") === "yaml"
        ? new Response("name: Acme\n", {
            status: 200,
            headers: { "Content-Type": "application/yaml", ETag: '"r-saved"' },
          })
        : null;
    await save(engine, () => null);
    fireEvent.click(await screen.findByRole("button", { name: "Copy as YAML" }));
    const dialog = await screen.findByRole("dialog", { name: "The company as YAML" });
    expect(await within(dialog).findByText(/name: Acme/)).toBeDefined();
    expect(within(dialog).getByRole("button", { name: "Copy" })).toBeDefined();
    expect(within(dialog).getByText("Credentials are hidden.")).toBeDefined();
  });

  test("Copy as YAML says when what it read is a later revision", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("format") === "yaml"
        ? new Response("name: Acme\n", {
            status: 200,
            headers: { "Content-Type": "application/yaml", ETag: '"r-later"' },
          })
        : null;
    await save(engine, () => null);
    fireEvent.click(await screen.findByRole("button", { name: "Copy as YAML" }));
    const dialog = await screen.findByRole("dialog", { name: "The company as YAML" });
    expect(await within(dialog).findByText(/Showing active revision/)).toBeDefined();
  });
});

describe("the org chart", () => {
  /**
   * A tab this node has sent one health push, and a way to mount a screen on
   * it — and another after it, as a reader moving from the chart to Teams
   * does.
   */
  function tabWith(appliedEpoch: number, attempt?: EngineHealth["config_attempt"]) {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/company/org";
    const store = new Store();
    // The health push, which is where this node's applied epoch and its last
    // apply attempt come from.
    store.applyHealth(
      healthFrame({ applied_epoch: appliedEpoch, ...(attempt ? { config_attempt: attempt } : {}) }),
    );
    store.applyOrg(
      withDerived({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] }),
    );
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
      Promise.resolve(null);
    const mount = (page: ReactNode) =>
      render(
        <ClientContext.Provider value={{ store, socket }}>
          <ViewerProvider>
            <Router>{page}</Router>
          </ViewerProvider>
        </ClientContext.Provider>,
      );
    return { store, mount };
  }

  function mountCompany(appliedEpoch: number, attempt?: EngineHealth["config_attempt"]) {
    const { store, mount } = tabWith(appliedEpoch, attempt);
    mount(<OrgChart />);
    return store;
  }

  test("say they still draw the previous revision until this node applies the saved one", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mountCompany(3);
    expect(await screen.findByText(/Still applying/)).toBeDefined();
  });

  test("say nothing once the node has applied it", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mountCompany(4);
    await waitFor(() => expect(screen.queryByText(/Still applying/)).toBeNull());
  });

  // A DROPPED SOCKET IS NO NEWS OF THE SAVE. Read as this node's standing,
  // the missing push said "Still applying" of a save this node had applied,
  // or refused, for as long as the socket was down. Mutation: read the live
  // slice (`useEngineHealth`) in the note and both read "Still applying".
  test("keep what this node last said of the save through a dropped socket", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    const store = mountCompany(3, { epoch: 4, revision_id: "r-saved", status: "error" });
    expect(await screen.findByText(/This node refused/)).toBeDefined();
    act(() => store.setConnected(false));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.getByText(/This node refused/)).toBeDefined();
    expect(screen.queryByText(/Still applying/)).toBeNull();

    // Back, and this node applied it on a retry since: the note goes, and
    // stays gone while the socket is down again.
    act(() =>
      store.applyHealth(
        healthFrame({
          applied_epoch: 4,
          config_attempt: { epoch: 4, revision_id: "r-saved", status: "ok" },
        }),
      ),
    );
    await waitFor(() => expect(screen.queryByText(/This node refused/)).toBeNull());
    act(() => store.setConnected(false));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByText(/Still applying/)).toBeNull();
  });

  // THE NOTE MOUNTS AFRESH on the chart, on Teams and on a unit's page, and
  // the save it follows outlives every mount: what this node last said is
  // kept by the tab, so a note mounted while the socket is down reads it as
  // well. Kept per mount, Teams opened during a drop read no push, and said
  // "Still applying … Showing the previous revision." in place of the
  // refusal the chart had just said. Mutation: keep the last push per mount
  // (`useState` in `useLastHealth`) and Teams says "Still applying".
  test("mounted while the socket is down, still say this node refused the save", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    const { store, mount } = tabWith(3, { epoch: 4, revision_id: "r-saved", status: "error" });
    const chart = mount(<OrgChart />);
    expect(await screen.findByText(/This node refused/)).toBeDefined();
    // The engine goes, and the reader moves from the chart to Teams.
    act(() => store.setConnected(false));
    chart.unmount();
    mount(<Teams />);
    expect(await screen.findByText(/This node refused/)).toBeDefined();
    expect(screen.queryByText(/Still applying/)).toBeNull();
  });

  // A SAVE THIS NODE APPLIED has no note, whenever the chart is mounted: the
  // chart draws the saved revision then (the org slice, which a drop does
  // not clear), and a note mounted during a drop said "Showing the previous
  // revision." over it. Mutation: keep the last push per mount, and the
  // chart says "Still applying".
  test("mounted while the socket is down, say nothing of a save this node applied", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    const { store, mount } = tabWith(4, { epoch: 4, revision_id: "r-saved", status: "ok" });
    act(() => store.setConnected(false));
    mount(<OrgChart />);
    expect((await screen.findAllByText("CEO")).length).toBeGreaterThan(0);
    expect(screen.queryByText(/Still applying/)).toBeNull();
    expect(screen.queryByText(/This node/)).toBeNull();
  });

  // A NODE THAT REFUSED THE SAVE WILL NEVER APPLY IT, so "still applying" was
  // a promise kept for ever. Mutation: draw the applying note whatever the
  // last attempt says.
  test("say this node refused the saved revision rather than that it is still applying", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mountCompany(3, { epoch: 4, revision_id: "r-saved", status: "error" });
    expect(await screen.findByText(/This node refused/)).toBeDefined();
    expect(screen.getByText(/Showing the revision it runs/)).toBeDefined();
    expect(screen.queryByText(/Still applying/)).toBeNull();
  });

  // AN APPLIED EPOCH FROM THE PREVIOUS POINTER, above the save's, is no
  // apply of it: the attempt naming the save says the node refused it.
  // Mutation: compare the applied epoch before reading the attempt and the
  // note goes, as though the chart drew the saved revision.
  test("say this node refused the save beside an applied epoch a rebuilt store left above it", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mountCompany(41, { epoch: 4, revision_id: "r-saved", status: "error" });
    expect(await screen.findByText(/This node refused/)).toBeDefined();
  });

  // A SAVE WHOSE ANSWER WAS LOST is followed here too once this node's
  // attempt names it, with the Builder and its strip gone. Mutation: leave
  // the note following a save by its recorded epoch alone and it says
  // nothing of a refusal.
  test("follow a save without an epoch once this node's attempt names it", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null });
    mountCompany(9, { epoch: 10, revision_id: "r-saved", status: "error" });
    expect(await screen.findByText(/This node refused/)).toBeDefined();
  });
});
