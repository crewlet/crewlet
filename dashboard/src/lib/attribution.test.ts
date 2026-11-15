/**
 * Who set each property, and — more importantly — when the honest answer is
 * nobody.
 *
 * The failure this guards is silent in both directions: a key naming no field
 * renders no line, and a rail that borrowed the oldest entry in the window
 * would render a confident sentence about a fact it cannot see.
 */

import { describe, expect, test } from "vitest";

import { CHANGE_FIELDS } from "~/contract/attribution.ts";
import {
  assisted,
  attribution,
  authorLabel,
  authorWords,
  credentialOf,
  credentialWords,
  throughOf,
} from "./attribution.ts";
import { changeAuthor } from "~/routes/doclet/parts.tsx";
import type { WorkChange } from "~/protocol/index.ts";

function change(over: Partial<WorkChange>): WorkChange {
  return {
    id: over.id ?? "c1",
    kind: over.kind ?? "fields",
    at: over.at ?? "2026-03-01T09:00:00Z",
    log_seq: over.log_seq ?? 1,
    ...over,
  };
}

describe("the latest change to each property", () => {
  test("names the actor, the kind and the turn", () => {
    const who = attribution([
      change({
        actor: "ada",
        actor_kind: "agent",
        turn_id: "t-9",
        at: "2026-03-01T12:00:00Z",
        fields: { status: { from: "todo", to: "in_progress" } },
      }),
    ]);
    expect(who.get("status")).toEqual({
      actor: "ada",
      actorKind: "agent",
      turnId: "t-9",
      at: "2026-03-01T12:00:00Z",
      // A CHANGE THAT FILLED THE FIELD, which is what the rail needs to know
      // before it draws a by-line under a blank one — see below.
      cleared: false,
    });
  });

  test("says whether the change emptied the field", () => {
    // THE ONE THING THAT LICENSES PROVENANCE UNDER A BLANK. A rail draws no
    // "set by" under a value that is not there — it would claim a record of
    // somebody setting nothing — and a change that took the value AWAY is a
    // record the log genuinely holds, which reads "cleared by" instead.
    // `worklet.Delta` carries `From` and `To` with no `omitempty`, so an
    // emptied field is `to: ""` rather than an absent key.
    const who = attribution([
      change({
        actor: "ada",
        fields: {
          due: { from: "2026-03-09T00:00:00Z", to: "" },
          status: { from: "todo", to: "in_progress" },
        },
      }),
    ]);
    expect(who.get("due")?.cleared).toBe(true);
    expect(who.get("status")?.cleared).toBe(false);
  });

  test("a delta this build cannot read claims no clearing either way", () => {
    // The defensive arm: a payload whose shape is unreadable says nothing
    // about whether a value was emptied, and `false` is the honest answer —
    // it suppresses the by-line under an absent value rather than inventing
    // a clearing nobody recorded.
    const who = attribution([
      change({ actor: "ada", fields: { status: "in_progress" as unknown as object } }),
    ]);
    expect(who.get("status")?.cleared).toBe(false);
  });

  test("the newest change wins, and the input order is what decides", () => {
    // The server orders `log_seq DESC` — a position on one stream rather than
    // a clock, which is the ordering that holds when two nodes wrote in the
    // same second. So the FIRST entry naming a field is the answer.
    const who = attribution([
      change({ log_seq: 9, actor: "bo", fields: { assignee: { to: "bo" } } }),
      change({ log_seq: 4, actor: "ada", fields: { assignee: { to: "ada" } } }),
    ]);
    expect(who.get("assignee")?.actor).toBe("bo");
  });

  test("one change attributes every field it moved", () => {
    const who = attribution([
      change({ actor: "ada", fields: { due: {}, points: {}, estimate: {} } }),
    ]);
    expect([...who.keys()].sort()).toEqual(["due", "estimate", "points"]);
  });

  test("a create attributes nothing — the rail states who filed it once", () => {
    // Attributed, a create drew one line naming every field it set under the
    // last of them, which read as that one field's provenance. A later change
    // still wins, and a field only the create set draws no line.
    const who = attribution([
      change({ log_seq: 2, actor: "ada", fields: { status: { from: "todo", to: "done" } } }),
      change({
        log_seq: 1,
        kind: "created",
        actor: "founder",
        actor_kind: "operator",
        fields: { title: {}, status: {} },
      }),
    ]);
    expect(who.has("title")).toBe(false);
    expect(who.get("status")?.actor).toBe("ada");
  });

  // A PERSON THE DIRECTORY BINDS TO A SEAT WRITES AS THE SEAT (`iam.ActorFor`),
  // whichever credential they wrote through — that rides beside it.
  test("a bound person's change names the seat they write as, as the activity does", () => {
    const who = attribution([
      change({
        actor: "jane-founder",
        actor_kind: "human",
        operator_id: "pat:t-1",
        fields: { priority: { from: "low", to: "high" } },
      }),
    ]);
    expect(who.get("priority")?.actor).toBe("jane-founder");
    expect(who.get("priority")?.actorKind).toBe("human");
  });
});

describe("when there is no honest answer", () => {
  test("a property no visible change names carries nothing", () => {
    // NOT the oldest entry in the window. `history` is the newest fifty
    // changes, so a property last moved before that is a fact about the page
    // size rather than about anybody.
    const who = attribution([change({ actor: "ada", fields: { status: {} } })]);
    expect(who.has("points")).toBe(false);
  });

  test("no history at all is an empty answer rather than a throw", () => {
    expect(attribution(undefined).size).toBe(0);
    expect(attribution([]).size).toBe(0);
  });

  test("a change with no actor attributes nothing", () => {
    // The line's whole content is who, and "set by" with nobody after it is
    // worse than silence.
    const who = attribution([change({ fields: { status: {} } })]);
    expect(who.size).toBe(0);
  });

  test("an actor kind the wire does not spell is dropped, not passed through", () => {
    const who = attribution([
      change({ actor: "ada", actor_kind: "robot", fields: { status: {} } }),
    ]);
    expect(who.get("status")?.actorKind).toBe(undefined);
  });
});

test("every field name is one the engine writes, spelled its way", () => {
  // The rail labels it "Estimate" and the task row calls the number
  // `estimate_minutes`; the history bag calls it `estimate`. "Watching" is
  // `watchers` and "Routes to" is `routing_unit`. Held against
  // `worklet.TaskDeltas` by `internal/worklet/attribution_test.go`; this half
  // only pins the shape so a rename here is visible in the diff.
  expect([...CHANGE_FIELDS]).toEqual([
    "title",
    "status",
    "assignee",
    "reporter",
    "collaborators",
    "watchers",
    "priority",
    "project",
    "type",
    "tags",
    "due",
    "start",
    "estimate",
    "points",
    "routing_unit",
  ]);
});

test("the people and routing rows can be attributed", () => {
  // THE ROWS THE RAIL DRAWS WITHOUT A LINE ARE THE POINT. Reporter,
  // Collaborators, Watching and "Routes to" rendered no provenance at all
  // until `worklet.TaskDeltas` started comparing those fields: a watcher
  // arrived and the rail could not say who added them, which is the one thing
  // this product can answer that a tracker cannot.
  const who = attribution([
    change({
      actor: "ada",
      actor_kind: "human",
      kind: "watchers",
      fields: {
        reporter: { from: "", to: "bo" },
        collaborators: { from: "bo", to: "bo, cy" },
        watchers: { from: "bo", to: "bo, ada" },
        routing_unit: { from: "Engineering", to: "Platform" },
      },
    }),
  ]);
  for (const field of ["reporter", "collaborators", "watchers", "routing_unit"] as const) {
    // DECLARED AND RESOLVED, both halves: `by()` on the rail takes a
    // [ChangeField], so a name missing from the list is a row that cannot ask
    // the question — and one in the list that the log never keys is a row that
    // asks and is answered with nothing.
    expect([...CHANGE_FIELDS]).toContain(field);
    expect(who.get(field)?.actor).toBe("ada");
    expect(who.get(field)?.cleared).toBe(false);
  }
  // AND A SET EMPTIED IS A CLEARING like any other value: the last watcher
  // leaving is a record the log holds, and the row reads "cleared by".
  const emptied = attribution([
    change({ actor: "ada", fields: { watchers: { from: "bo", to: "" } } }),
  ]);
  expect(emptied.get("watchers")?.cleared).toBe(true);
});

// A WRITE THROUGH THE ASSISTANT IS THE PERSON'S, WITH THE ASSISTANT. The engine
// records `assist:<grant>` as the credential (server-set, never claimed by a
// page), and every history surface printed the grant id: "Morgan (through
// assist:...)" in Config and Audit, "Written through assist:..." in the
// tracker, and "Morgan saved it" with nothing at all on a page.
describe("a write through the assistant", () => {
  const GRANT = "assist:0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a";
  test("reads as the person's, with the assistant, and never prints the grant", () => {
    expect(assisted(GRANT)).toBe(true);
    for (const words of [
      authorWords("Morgan", GRANT),
      authorLabel("morgan.davis", GRANT),
      throughOf("morgan.davis", GRANT),
      changeAuthor(
        { actor: "morgan", actor_kind: "human", operator_id: GRANT },
        () =>
          ({
            name: "Morgan",
          }) as never,
      ),
    ]) {
      expect(words).toContain("Ask Crewlet");
      expect(words).not.toContain("assist:");
      expect(words).not.toContain("0b6f5a4e");
    }
    expect(authorWords("Morgan", GRANT)).toBe("Morgan, with Ask Crewlet");
  });

  test("a plain session write reads as before", () => {
    const session = "session:5c1d2e3f-0000-4000-8000-000000000000";
    expect(assisted(session)).toBe(false);
    expect(assisted(undefined)).toBe(false);
    expect(authorWords("Morgan", session)).toBe("Morgan");
    expect(authorLabel("morgan.davis", session)).toBe("morgan.davis (through a signed-in session)");
    expect(throughOf("token:ops", "token:ops")).toBe("");
    expect(
      changeAuthor(
        { actor: "morgan", actor_kind: "human", operator_id: session },
        () =>
          ({
            name: "Morgan",
          }) as never,
      ),
    ).toBe("Morgan");
  });
});

// A CREDENTIAL IS SAID BY ITS KIND, never its id: "Through pat:01a1dc39-…"
// and "By token:founder" named a credential by a string nobody calls it. The
// id is the screen's to give behind a copy control. Mutation: return the id
// as written and every line below reads a `pat:` or `session:` again.
describe("a credential, as a reader is told it", () => {
  test("is its kind, or a Tier A token's own name", () => {
    expect(credentialWords("pat:01a1dc39-0000-4000-8000-000000000000")).toBe(
      "a personal access token",
    );
    expect(credentialWords("session:01a1dc41-0000-4000-8000-000000000000")).toBe(
      "a signed-in session",
    );
    expect(credentialWords("token:founder")).toBe("the API token founder");
    expect(credentialWords("assist:0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a")).toBe("Ask Crewlet");
  });

  test("keeps the id apart, for a copy control, only where it has one", () => {
    expect(credentialOf("pat:01a1dc39")).toEqual({ kind: "personal access token", id: "01a1dc39" });
    expect(credentialOf("session:01a1dc41")).toEqual({ kind: "signed-in session", id: "01a1dc41" });
    expect(credentialOf("token:founder")).toBeNull();
    expect(credentialOf("assist:0b6f5a4e")).toBeNull();
  });

  test("and an author's line names it so", () => {
    expect(authorLabel("jane.doe", "pat:01a1dc39-0000-4000-8000-000000000000")).toBe(
      "jane.doe (through a personal access token)",
    );
    for (const id of ["pat:01a1dc39", "session:01a1dc41"]) {
      expect(authorLabel("Jane Founder", id)).not.toContain(id);
    }
  });

  // THE LABEL IS WHAT THE CREDENTIAL IS COMPARED WITH, the name what is said.
  // A Tier A token writing as itself is recorded under its own login, and
  // told as "founder (API token)" it is still not "through" itself.
  // Mutation: compare the credential with the name and the first line reads
  // "founder (API token) (through the API token founder)".
  test("is compared with the recorded label, whatever name the reader is told", () => {
    expect(authorLabel("token:founder", "token:founder", "founder (API token)")).toBe(
      "founder (API token)",
    );
    expect(authorLabel("jane-founder", "pat:01a1dc39", "Jane Founder")).toBe(
      "Jane Founder (through a personal access token)",
    );
    expect(authorLabel("jane-founder", "token:founder", "Jane Founder")).toBe(
      "Jane Founder (through the API token founder)",
    );
  });
});
