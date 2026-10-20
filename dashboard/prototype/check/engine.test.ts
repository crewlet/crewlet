/**
 * The writes the demo engine models change the company the way the engine's
 * own do, every answer that reads the change agrees about it, and a write or a
 * question it does not model is refused out loud.
 *
 * Driven through the two seams the dashboard uses, a query and a REST
 * request, so what is held here is what a screen meets.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { fairsplit } from "../fixtures/fairsplit.ts";
import {
  addAgent,
  compose,
  connectIntegration,
  integrationStatus,
  resumeAll,
  seatFailed,
  seatStopped,
  seatWorking,
  signedInAsPerson,
  signedInAsServiceAccount,
  signedInAsToken,
  withPublicAddress,
} from "../fixtures/scenarios.ts";
import { findItem } from "../harness/engine/work.ts";
import { DemoEngine, type Push } from "../harness/engine/engine.ts";
import { fixtureId } from "../harness/engine/ids.ts";
import { DemoRefusal } from "../harness/engine/refusal.ts";
import type { RestReply } from "../harness/engine/rest.ts";

let engine: DemoEngine;
let pushes: Push[];
let warned: unknown[][];

beforeEach(() => {
  engine = new DemoEngine(fairsplit());
  pushes = [];
  engine.onPush((p) => pushes.push(p));
  warned = [];
  vi.spyOn(console, "warn").mockImplementation((...args) => void warned.push(args));
});

afterEach(() => {
  vi.restoreAllMocks();
});

function rest(
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): RestReply {
  const at = path.indexOf("?");
  return engine.rest({
    method,
    path: at < 0 ? path : path.slice(0, at),
    query: new URLSearchParams(at < 0 ? "" : path.slice(at + 1)),
    headers: new Headers(headers),
    body: body === undefined ? null : typeof body === "string" ? body : JSON.stringify(body),
  });
}

const act = (tool: string, args: Record<string, unknown>) =>
  rest("POST", `/operator/act/${tool}`, {
    request_id: "0192f0c4-0000-7000-8000-000000000000",
    args,
  });

describe("the base company", () => {
  test("reads as the screenshots do: two paused agents, two open decisions, six unread updates", () => {
    const agents = engine.agentRows();
    expect(agents.map((a) => [a.handle, a.activity, a.stopped_reason])).toEqual([
      ["pm", "stopped", "paused"],
      ["engineer", "stopped", "paused"],
    ]);
    expect(engine.answer("decisions", {}).total).toBe(2);
    const inbox = engine.answer("work_inbox", {
      handle: "miles",
      unread: true,
      primary_only: true,
    });
    expect(inbox.notices).toHaveLength(6);
    expect(engine.answer("integrations", {}).tools?.every((t) => t.state === "not_in_use")).toBe(
      true,
    );
    expect(engine.snapshot().org?.derived?.units?.[0]).toMatchObject({
      name: "Product and Engineering",
      lead: "pm",
    });
  });
});

describe("seats", () => {
  test("resume and pause move the seat's state and push it", () => {
    expect(act("resume_seat", { handle: "pm" }).status).toBe(200);
    expect(engine.agentRows().find((a) => a.handle === "pm")).toMatchObject({
      activity: "idle",
      paused: null,
    });
    expect(pushes.some((p) => p.kind === "agents")).toBe(true);
    expect(act("pause_seat", { handle: "pm", reason: "Holding for review" }).status).toBe(200);
    expect(engine.agentRows().find((a) => a.handle === "pm")).toMatchObject({
      activity: "stopped",
      stopped_reason: "paused",
      paused: { by: "miles", reason: "Holding for review" },
    });
  });
});

describe("decisions", () => {
  test("answering one closes it everywhere it is read", () => {
    const reply = act("comment_on_work_item", {
      item: "FAIR-1",
      answers: fixtureId("comment-3"),
      choice: "equal",
    });
    expect(reply.status).toBe(200);
    expect(reply.body).toMatchObject({
      outcome: "applied",
      position: expect.stringMatching(/@1:\d+$/),
    });
    expect(engine.answer("decisions", {}).total).toBe(1);
    const notice = engine
      .answer("work_inbox", { handle: "miles" })
      .notices.find((n) => n.comment_id === fixtureId("comment-3"));
    expect(notice?.ask).toMatchObject({ open: false, choice: "equal", answered_by: "miles" });
    expect(
      act("comment_on_work_item", {
        item: "FAIR-1",
        answers: fixtureId("comment-3"),
        choice: "all",
      }).body,
    ).toMatchObject({ error: "already_answered" });
  });
});

describe("who the browser is", () => {
  const session = () => rest("GET", "/auth/session").body as Record<string, unknown>;
  const viewer = () => engine.answer("viewer", {});

  test("is Miles's review token today: its login and no name of its own, his seat on the viewer", () => {
    expect(session()).toMatchObject({
      login: "token:review",
      kind: "person",
      seat: "miles",
      grants: ["admin", "tracker", "knowledge"],
      status: "signed_in",
      lineage: expect.any(String),
      expires_at: expect.any(String),
    });
    expect(session()).not.toHaveProperty("name");
    expect(viewer()).toMatchObject({
      login: "token:review",
      handle: "miles",
      name: "Miles",
      kind: "human",
      owner: "miles",
      grants: ["admin", "tracker", "knowledge"],
      project: "",
      config_writer: true,
    });
    const left = Date.parse(String(session().expires_at)) - Date.now();
    expect(left).toBeGreaterThan(50 * 60_000);
    expect(left).toBeLessThanOrEqual(52 * 60_000);
    expect(rest("GET", "/iam/node-tokens").body).toMatchObject({
      tokens: [{ id: "review", login: "token:review", row: "held", seat: "miles" }],
    });
  });

  test("is a person with their own name, bound to their seat, with the grants on their row", () => {
    engine = new DemoEngine(
      signedInAsPerson({ name: "Ahmad Mollapour", login: "ahmad.mollapour", grants: ["tracker"] })(
        fairsplit(),
      ),
    );
    expect(session()).toMatchObject({
      name: "Ahmad Mollapour",
      login: "ahmad.mollapour",
      kind: "person",
      seat: "miles",
      grants: ["tracker"],
    });
    expect(viewer()).toMatchObject({ handle: "miles", owner: "miles", grants: ["tracker"] });
    expect(viewer().acts).not.toContain("write_page");
    const id = String(session().person);
    expect(rest("GET", `/iam/people/${id}`).body).toMatchObject({ login: "ahmad.mollapour" });
    expect(rest("GET", "/iam/people").status).toBe(403);
    expect(rest("GET", "/health").body).toMatchObject({ identity: "ready" });
  });

  test("is a service account under its own login, with no seat and no session", () => {
    engine = new DemoEngine(
      signedInAsServiceAccount({ login: "ci:deploy", grants: ["tracker"] })(fairsplit()),
    );
    expect(session()).toMatchObject({ login: "ci:deploy", kind: "machine", grants: ["tracker"] });
    expect(session()).not.toHaveProperty("seat");
    expect(session()).not.toHaveProperty("lineage");
    expect(viewer()).toMatchObject({ handle: "", owner: "ci:deploy", kind: "" });
    act("comment_on_work_item", { item: "FAIR-2", body: "Deployed." });
    expect(findItem(engine.company, "FAIR-2")!.history.at(-1)).toMatchObject({
      actor: "ci:deploy",
      actor_kind: "operator",
      operator_id: expect.stringMatching(/^pat:/),
    });
  });

  test("refuses a seat the engine would not bind", () => {
    expect(() => signedInAsToken("review", "pm")(fairsplit())).toThrow(/agent's seat/);
    expect(() => signedInAsPerson({ name: "X", login: "x" })(fairsplit())).toThrow(/login/);
  });
});

describe("my work's questions", () => {
  const keys = (params: Record<string, unknown>) =>
    engine.answer("work_items", params).items.map((i) => i.key);

  test("asked by and asked of read the open asks", () => {
    expect(keys({ asked_by: "miles", viewer: "miles" })).toEqual([]);
    expect(keys({ asked_by: "pm" })).toEqual(["FAIR-1"]);
    expect(keys({ asked_of: "miles" })).toEqual(["FAIR-1"]);
    expect(keys({ has_open_asks: true })).toEqual(["FAIR-1"]);
  });

  test("an ask made through Miles's token session is his seat's, and so is its answer", () => {
    // A person bound to a seat writes AS the seat, and the credential is kept beside it.
    const asked = act("comment_on_work_item", {
      item: "FAIR-2",
      body: "Which currency should the list print first?",
      ask: "pm",
    });
    const comment = findItem(engine.company, "FAIR-2")!.comments.at(-1)!;
    expect(comment).toMatchObject({ author: "miles", author_kind: "human" });
    expect(findItem(engine.company, "FAIR-2")!.history.at(-1)).toMatchObject({
      actor: "miles",
      actor_kind: "human",
      operator_id: expect.stringMatching(/^session:/),
    });
    expect(keys({ asked_by: "miles", viewer: "miles" })).toEqual(["FAIR-2"]);
    expect(keys({ asked_of: "pm" })).toEqual(["FAIR-2"]);

    const id = (asked.body as { receipt: { id: string } }).receipt.id;
    act("comment_on_work_item", { item: "FAIR-2", answers: id, body: "Euros." });
    const answered = (engine.company.work.inbox.miles ?? []).filter((n) => n.reason === "answered");
    expect(answered).toHaveLength(1);
    expect(keys({ asked_by: "miles", viewer: "miles" })).toEqual([]);
  });
});

describe("the inbox split", () => {
  const inbox = () => engine.answer("work_inbox", { handle: "miles" });
  const primary = () =>
    inbox()
      .notices.filter((n) => n.primary)
      .map((n) => n.reason);

  test("is the engine's default until the person says otherwise, and an empty list takes it back", () => {
    expect(inbox().primary_reasons).toEqual([
      "mention",
      "asked",
      "answered",
      "assignee",
      "unassigned",
      "reporter",
      "unblocked",
      "prioritised",
    ]);
    expect(primary()).toHaveLength(8);
    expect(engine.answer("work_person", { handle: "miles" }).primary_reasons).toBeUndefined();

    expect(act("mark_inbox", { primary_reasons: ["asked"] }).status).toBe(200);
    expect(inbox().primary_reasons).toEqual(["asked"]);
    expect(new Set(primary())).toEqual(new Set(["asked"]));
    expect(engine.answer("work_person", { handle: "miles" }).primary_reasons).toEqual(["asked"]);

    act("mark_inbox", { read: [inbox().notices[0]!.record_id] });
    expect(inbox().primary_reasons).toEqual(["asked"]);

    act("mark_inbox", { primary_reasons: [] });
    expect(primary()).toHaveLength(8);
  });

  test("refuses a reason the engine does not know, and changes nothing", () => {
    const reply = act("mark_inbox", { primary_reasons: ["asked", "urgent"] });
    expect(reply.status).toBe(422);
    expect(reply.body).toMatchObject({
      detail: expect.stringContaining('"urgent" is not a wake reason'),
    });
    expect(inbox().primary_reasons).toHaveLength(8);
  });
});

describe("freshness", () => {
  test("a state-log answer states the level it was asked for, and a malformed request is refused", () => {
    expect(engine.answer("decisions", {}).read_level).toBe("stale");
    expect(
      engine.answer("work_inbox", { handle: "miles", read_level: "linearizable" }).read_level,
    ).toBe("linearizable");
    expect(() => engine.answer("pages", { read_level: "eventual" })).toThrow(DemoRefusal);
    expect(engine.answer("viewer", {})).not.toHaveProperty("read_level");
  });
});

describe("the company document", () => {
  test("an org edit is a new revision, pushed, with the seat derived and given a live state", () => {
    const read = rest("GET", "/config");
    const etag = read.headers?.ETag ?? "";
    const units = structuredClone((read.body as { units: { roles: object[] }[] }).units);
    units[0]!.roles.push({
      name: "Crewlet-QA",
      handle: "qa",
      avatar: { character: "shieldlet", color: "green" },
    });
    const check = rest("PATCH", "/config?dry_run=true", { units }, { "If-Match": etag });
    expect(check.status).toBe(200);
    const saved = rest(
      "PATCH",
      "/config",
      { units, _summary: "Added Crewlet-QA" },
      { "If-Match": etag },
    );
    expect(saved.status).toBe(201);
    expect(saved.body).toMatchObject({ epoch: 9 });
    expect(engine.agentRows().map((a) => a.handle)).toEqual(["pm", "engineer", "qa"]);
    expect(engine.company.seats.qa?.overlay.activity).toBe("idle");
    expect(pushes.map((p) => p.kind)).toEqual(
      expect.arrayContaining(["org", "seats", "agents", "schedules", "health"]),
    );
    // The derived hierarchy is the engine's: the unit's lead manages the new seat.
    const pm = engine.snapshot().org?.derived?.seats?.find((s) => s.handle === "pm");
    expect(pm?.auto_reports).toEqual(["engineer", "qa"]);
  });

  test("a write on a stale revision is a conflict, and a duplicate handle is a located problem", () => {
    expect(rest("PATCH", "/config", { name: "X" }, { "If-Match": '"stale"' }).status).toBe(409);
    const etag = rest("GET", "/config").headers?.ETag ?? "";
    const doc = rest("GET", "/config").body as { units: { roles: object[] }[] };
    // A NAME IS PROSE two seats may share; the handle is the seat's identity.
    doc.units[0]!.roles.push({ name: "Crewlet-PM", handle: "pm2" });
    expect(rest("PATCH", "/config", { units: doc.units }, { "If-Match": etag }).status).toBe(201);
    const next = rest("GET", "/config");
    const again = next.body as { units: { roles: object[] }[] };
    again.units[0]!.roles.push({ name: "Crewlet-PM 3", handle: "pm" });
    const etag2 = next.headers?.ETag ?? "";
    const refused = rest("PATCH", "/config", { units: again.units }, { "If-Match": etag2 });
    expect(refused.status).toBe(422);
    expect(refused.body).toMatchObject({
      error: "validation_error",
      problems: [{ kind: "conflict" }],
    });
  });
});

describe("integrations", () => {
  const setup = (method: string, path: string, body?: unknown) =>
    rest(method, `/setup/integrations${path}`, body);
  const tool = (key: string) => engine.answer("integrations", {}).tools?.find((t) => t.key === key);
  // As the connect dialog submits it: a hidden field from its default.
  const MATTERMOST = {
    enabled: "true",
    url: "https://chat.fairsplit.example",
    team: "fairsplit",
    admin_token: "demo-admin-token",
  };

  test("a connect is a revision the room and the forms both read, and a rotation reloads", () => {
    const connected = setup("POST", "/mattermost/inputs", { values: MATTERMOST });
    expect(connected.status).toBe(201);
    expect(connected.body).toMatchObject({
      reloaded: false,
      epoch: 9,
      wrote_secrets: ["MATTERMOST_ADMIN_TOKEN"],
    });
    expect(engine.answer("integrations", {}).integrations.map((r) => r.key)).toEqual([
      "mattermost",
    ]);
    expect(tool("mattermost")).toMatchObject({ state: "connected", label: "Connected" });
    expect(engine.company.secrets.map((s) => s.name)).toContain("MATTERMOST_ADMIN_TOKEN");
    const rotated = setup("POST", "/mattermost/inputs", { values: { admin_token: "a-new-one" } });
    expect(rotated.body).toMatchObject({
      reloaded: true,
      epoch: 10,
      wrote_secrets: ["MATTERMOST_ADMIN_TOKEN"],
    });
  });

  test("a disconnect is asked of the loop, which drops the block and names what it leaves", () => {
    setup("POST", "/mattermost/inputs", { values: MATTERMOST });
    const asked = setup("DELETE", "/mattermost", { remove_seats: false });
    expect(asked.status).toBe(202);
    expect(asked.body).toMatchObject({
      disconnecting: true,
      removed: false,
      orphaned_secrets: ["MATTERMOST_ADMIN_TOKEN"],
    });
    expect(engine.answer("integrations", {}).integrations).toEqual([]);
    const listing = setup("GET", "").body as { tools: { key: string; configured: boolean }[] };
    expect(listing.tools.find((t) => t.key === "mattermost")?.configured).toBe(false);
    expect(setup("DELETE", "/mattermost", {}).body).toEqual({ key: "mattermost", removed: false });
  });

  test("the setup surface refuses what the engine refuses, in its words", () => {
    expect(setup("GET", "/teams")).toMatchObject({ status: 404, body: { error: "unknown_kind" } });
    expect(setup("POST", "/mattermost/inputs", { values: {} }).body).toMatchObject({
      error: "invalid_input",
      detail: "the submission carried no values",
    });
    expect(
      setup("POST", "/slack/inputs", { seat: "nobody", values: { bot_token: "xoxb-1" } }),
    ).toMatchObject({ status: 404, body: { error: "invalid_input" } });
    expect(
      setup("POST", "/mattermost/inputs", { values: MATTERMOST, colour: "red" }).body,
    ).toMatchObject({
      error: "invalid_body",
    });
  });

  test("the public address scenario offers every agent's app manifest", () => {
    const withAddress = new DemoEngine(withPublicAddress()(fairsplit()));
    const listing = withAddress.rest({
      method: "GET",
      path: "/setup/integrations",
      query: new URLSearchParams(),
      headers: new Headers(),
      body: null,
    }).body as {
      external_url: { value: string };
      tools: { key: string; seats?: { manifest?: string }[] }[];
    };
    expect(listing.external_url.value).toBe("https://fairsplit.example.com");
    expect(
      listing.tools.find((t) => t.key === "slack")?.seats?.every((s) => Boolean(s.manifest)),
    ).toBe(true);
  });
});

describe("scenarios", () => {
  test("compose without changing the company they were given", () => {
    const base = fairsplit();
    const busy = compose(resumeAll, seatWorking("pm", "FAIR-1"))(base);
    expect(busy.seats.pm?.overlay.activity).toBe("working");
    expect(base.seats.pm?.overlay.activity).toBe("stopped");
  });
});

describe("scenario helpers reach every answer that reads what they change", () => {
  test("a connected tool, and a surface that needs a person", () => {
    const step = compose(
      connectIntegration("slack", [
        { values: { typing_status: "addressed" } },
        { seat: "pm", values: { bot_token: "xoxb-demo", signing_secret: "demo" } },
        { seat: "engineer", values: { bot_token: "xoxb-demo", signing_secret: "demo" } },
      ]),
      connectIntegration("mattermost", [
        {
          values: {
            enabled: "true",
            url: "https://chat.fairsplit.example",
            team: "fairsplit",
            admin_token: "demo-admin-token",
          },
        },
      ]),
      integrationStatus("mattermost", {
        phase: "awaiting_admin",
        actor: "admin",
        detail: "Approve the agents' bots.",
      }),
    );
    const c = step(fairsplit());
    const room = new DemoEngine(c).answer("integrations", {});
    // Slack has no pass to report on it, so its card reads connected on its own.
    expect(room.tools?.find((t) => t.key === "slack")).toMatchObject({
      state: "connected",
      label: "Connected",
    });
    expect(room.tools?.find((t) => t.key === "mattermost")).toMatchObject({
      state: "attention",
      label: "Action needed",
      reason: "Approve the agents' bots.",
    });
    expect(c.secrets.map((s) => s.name)).toEqual(
      expect.arrayContaining(["SLACK_BOT_TOKEN_PM", "SLACK_SIGNING_SECRET_ENGINEER"]),
    );
    expect(c.config.epoch).toBe(12);
  });

  test("an added agent is in the roster, idle, and seat states read as the engine's words", () => {
    const c = compose(
      addAgent({
        name: "Crewlet-QA",
        handle: "qa",
        unit: "Product and Engineering",
        character: "shieldlet",
        color: "green",
      }),
      seatStopped("pm", "unplaced"),
      seatFailed("engineer", "The provider answered 401: the API key was revoked."),
    )(fairsplit());
    const agents = new DemoEngine(c).agentRows();
    expect(agents.map((a) => [a.handle, a.activity, a.stopped_reason])).toEqual([
      ["pm", "stopped", "unplaced"],
      ["engineer", "stopped", "provider"],
      ["qa", "idle", null],
    ]);
    expect(agents.find((a) => a.handle === "engineer")?.last_error?.message).toMatch(/revoked/);
  });
});

describe("what is not modelled is refused out loud", () => {
  test("an act", () => {
    const reply = act("steer_turn", { turn_id: "x", note: "hi" });
    expect(reply.status).toBe(422);
    expect(reply.body).toMatchObject({
      error: "invalid",
      detail: expect.stringContaining("not modelled"),
    });
    expect(warned).toHaveLength(1);
  });

  test("a REST path", () => {
    expect(rest("GET", "/nowhere").body).toMatchObject({ error: "no_route" });
    expect(String(warned[0]?.[1])).toContain("/nowhere");
  });

  test("a question", () => {
    expect(() => engine.answer("nonsense" as never, {})).toThrow(DemoRefusal);
    expect(warned).toHaveLength(1);
  });
});
