/**
 * The Agents tab's readings, held one by one: a row's status word, which
 * actions a seat can take, the sentence an off seat reads as, and the
 * vendor's words for what a seat holds.
 */

import { describe, expect, test } from "vitest";
import {
  CONFLUENCE_LANE,
  DATADOG_LANE,
  DIGITALOCEAN_LANE,
  EXPO_LANE,
  GOOGLE_CLOUD_LANE,
  JIRA_LANE,
  GITHUB_LANE,
  MATTERMOST_LANE,
  NEUTRAL_LANE,
  SCALEWAY_LANE,
  datadogAnswer,
  digitaloceanAnswer,
  digitaloceanWaitingAnswer,
  expoAnswer,
  expoWaitingAnswer,
  githubAnswer,
  neutralAnswer,
  offLane,
  onLane,
  scalewayAnswer,
} from "./fixtures.ts";
import {
  accessHold,
  accessWords,
  availableActions,
  emptyScopeHint,
  handoffOf,
  rotateHandoffOf,
  scopeGuide,
  joinLevels,
  levelName,
  boxSettings,
  boxSettingsFact,
  ceilingNote,
  changeSummary,
  clockValue,
  credentialNote,
  noToolRemedy,
  onlyScope,
  productOnly,
  refusedLevel,
  refusedScope,
  reviewWords,
  scopeApplies,
  offSentence,
  sameDesired,
  scopeChoices,
  scopeUnavailable,
  scopeWords,
  statusOf,
  takesAccessAway,
  toolWords,
  type Lane,
} from "./logic.ts";
import type { AccessLaneStatus, AccessSeatLane, AccessStep } from "~/contract/access.ts";
import { laneWords } from "./words.ts";

const sre = () => datadogAnswer().seats.find((s) => s.handle === "sre")!.lanes.datadog!;

describe("a row's status", () => {
  test("a draft reads Not saved before anything the engine said", () => {
    expect(statusOf(DATADOG_LANE, { ...sre(), status: "refused" }, true).label).toBe("Not saved");
  });

  // THE RULE IS THE ENGINE'S (`status`); the tab only words it and picks its tone.
  test("each status the engine sends reads in the tab's words and tone", () => {
    const cases: [AccessLaneStatus, string, string][] = [
      ["syncing", "Syncing", "info"],
      ["refused", "Refused", "danger"],
      ["changed_at_vendor", "Changed at Datadog", "warning"],
      ["needs_you", "Needs you", "warning"],
      ["next_sync", "Next sync", "info"],
      ["in_sync", "In sync", "success"],
      ["off_account_kept", "Off, account kept", "neutral"],
      ["off", "Off", "neutral"],
      ["departed", "Agent left", "warning"],
    ];
    for (const [status, label, tone] of cases) {
      expect(statusOf(DATADOG_LANE, { ...sre(), status }, false)).toMatchObject({ label, tone });
    }
    expect(statusOf(DATADOG_LANE, { ...sre(), status: "syncing" }, false).busy).toBe(true);
    expect(statusOf(DATADOG_LANE, offLane(), false).label).toBe("Off");
  });

  // A NEWER OR OLDER ENGINE may send a status this bundle has no words for, or
  // none: the row reads the engine's own label, neutral, and the tab still draws.
  test("a status the tab does not know reads the engine's label, neutral", () => {
    const unknown = { ...sre(), status: "quarantined", status_label: "Quarantined" };
    expect(statusOf(DATADOG_LANE, unknown as unknown as AccessSeatLane, false)).toEqual({
      label: "Quarantined",
      tone: "neutral",
    });
    const missing: Partial<AccessSeatLane> = { ...sre() };
    delete missing.status;
    delete missing.status_label;
    expect(statusOf(DATADOG_LANE, missing as unknown as AccessSeatLane, false)).toEqual({
      label: "Status unknown",
      tone: "neutral",
    });
  });
});

describe("a row's clocks", () => {
  const now = Date.parse("2026-11-10T12:00:00Z");
  const at = (minutes: number) => new Date(now - minutes * 60_000).toISOString();

  // TWO CLOCKS IN ONE VALUE: Check now moves the first and never the second, so
  // a row just checked is never read as just applied.
  test("a lane says when it was checked and when a sync last applied it", () => {
    const lane = {
      ...sre(),
      observed_at: at(1),
      sync: { at: at(10), outcome: "ok" as const, steps: [] },
    };
    expect(clockValue(lane, now)).toBe("Checked 1m ago, last applied 10m ago");
    const refused = { ...lane, sync: { ...lane.sync, outcome: "failed" as const } };
    expect(clockValue(refused, now)).toBe("Checked 1m ago, refused 10m ago");
    const waiting = { ...lane, sync: { ...lane.sync, outcome: "waiting" as const } };
    expect(clockValue(waiting, now)).toBe("Checked 1m ago, waiting on a person since 10m ago");
    const { sync: _, ...unsynced } = lane;
    expect(clockValue(unsynced, now)).toBe("Checked 1m ago, never applied");
  });

  test("a lane no pass has checked says so, or what its last sync did", () => {
    const { observed_at: _o, sync: _s, ...never } = sre();
    expect(clockValue(never, now)).toBe("Not checked yet");
    const { observed_at: _, ...applied } = {
      ...sre(),
      sync: { at: at(10), outcome: "ok" as const, steps: [] },
    };
    expect(clockValue(applied, now)).toBe("Last applied 10m ago");
  });
});

describe("what the app holds past the grant", () => {
  const qa = () => githubAnswer().seats[0]!.lanes.github!;
  const withCeiling = (ceiling: Record<string, "read" | "write">) => ({
    ...qa().observed,
    ceiling,
  });

  test("a permission the app holds and the grant lacks is one the agent does not use", () => {
    const want = qa().desired;
    expect(
      ceilingNote(GITHUB_LANE, want, withCeiling({ ...want.permissions, workflows: "write" })),
    ).toBe("The app also holds Workflows; this agent does not use it.");
    expect(
      ceilingNote(
        GITHUB_LANE,
        { enabled: true, permissions: { issues: "write" } },
        withCeiling({ issues: "write", workflows: "write", deployments: "read" }),
      ),
    ).toBe("The app also holds Deployments and Workflows; this agent does not use them.");
  });

  test("a level the app holds above the grant names the level the agent uses", () => {
    const want = qa().desired;
    expect(
      ceilingNote(GITHUB_LANE, want, withCeiling({ ...want.permissions, contents: "write" })),
    ).toBe("The app also holds Contents Read and write; this agent uses Read-only.");
    expect(
      ceilingNote(
        GITHUB_LANE,
        { enabled: true, permissions: { contents: "read", checks: "read" } },
        withCeiling({ contents: "write", checks: "write" }),
      ),
    ).toBe(
      "The app also holds Contents and Checks Read and write; this agent uses them Read-only.",
    );
  });

  test("nothing past the grant, an off agent or another vendor says nothing", () => {
    const want = qa().desired;
    expect(ceilingNote(GITHUB_LANE, want, withCeiling({ ...want.permissions }))).toBeNull();
    expect(ceilingNote(GITHUB_LANE, want, { ...qa().observed, ceiling: undefined })).toBeNull();
    expect(
      ceilingNote(GITHUB_LANE, { ...want, enabled: false }, withCeiling({ workflows: "write" })),
    ).toBeNull();
    expect(
      ceilingNote(DATADOG_LANE, sre().desired, withCeiling({ workflows: "write" })),
    ).toBeNull();
  });
});

describe("the actions a seat can take", () => {
  test("an on seat with a key can rotate it, and cannot be removed", () => {
    expect(availableActions(DATADOG_LANE, sre(), false, false)).toEqual(["rotate"]);
  });

  test("an off seat whose account is kept can be removed", () => {
    const kept = { ...offLane(), observed: { exists: true, enabled: false } };
    expect(availableActions(DATADOG_LANE, kept, false, false)).toEqual(["remove"]);
  });

  test("nothing is offered to a reader who may not change it, or while a sync runs", () => {
    expect(availableActions(DATADOG_LANE, sre(), true, false)).toEqual([]);
    expect(availableActions(DATADOG_LANE, sre(), false, true)).toEqual([]);
  });

  test("a vendor whose tokens are minted per use offers no rotation", () => {
    const github = onLane({ variable: "GITHUB_APP_KEY_QA" });
    expect(availableActions(GITHUB_LANE, github, false, false)).toEqual([]);
  });
});

test("an off seat reads as one sentence about what turning it on does", () => {
  expect(offSentence(DATADOG_LANE, offLane())).toBe(
    "Off. Turning it on creates a Datadog service account with the chosen role and its own key.",
  );
  const kept = { ...offLane(), observed: { exists: true, enabled: false } };
  expect(offSentence(DATADOG_LANE, kept)).toMatch(/^Off\. Its account is disabled and kept/);
});

test("the tool reads as one value", () => {
  expect(toolWords(DATADOG_LANE, sre())).toBe("Live, Datadog MCP server, one per agent");
  expect(toolWords(DATADOG_LANE, { ...sre(), tool: "no_server" })).toBe(
    "Not set up: no datadog tool server is declared",
  );
  expect(toolWords(DATADOG_LANE, { ...sre(), tool: "server_off" })).toBe(
    "Not running: the datadog tool server is turned off",
  );
});

// EXPO'S TOOL IS THE EAS CLI IN THE AGENT'S OWN BOX, with no tool server to
// start: an on seat reads as the CLI it drives, by the token Crewlet sets in
// its box, and its level as Expo's role name. Nothing tells a person to add a
// line: Crewlet sets it.
test("an Expo seat's tool is the EAS CLI in its box, and its level is Expo's role", () => {
  const qa = expoAnswer().seats.find((s) => s.handle === "qa")!.lanes.expo!;
  expect(toolWords(EXPO_LANE, qa)).toBe("Live, EAS CLI in its box, as EXPO_TOKEN");
  expect(boxSettings(qa)).toBeNull();
  expect(toolWords(EXPO_LANE, { ...qa, tool: "no_server" })).toBe(
    "Not set up: no tool server is declared",
  );
  expect(noToolRemedy(DATADOG_LANE)).toMatch(/^the company declares no datadog tool server\./);
  expect(accessWords(EXPO_LANE, qa.observed)).toBe("Developer");
  expect(availableActions(EXPO_LANE, qa, false, false)).toEqual(["rotate"]);
  expect(offSentence(EXPO_LANE, offLane("viewer"))).toBe(
    "Off. Turning it on asks you to add a robot for the agent at Expo with the chosen role and paste its token here. Crewlet checks the token and keeps it.",
  );
});

test("what the vendor holds is said in the vendor's words", () => {
  expect(accessWords(DATADOG_LANE, sre().observed)).toBe("Datadog Admin Role");
  expect(
    accessWords(GITHUB_LANE, {
      exists: true,
      enabled: true,
      permissions: { contents: "write", issues: "read" },
    }),
  ).toBe("Contents: Read and write, Issues: Read-only");
});

test("two desired states are the same whatever order their scope is in", () => {
  expect(
    sameDesired(
      GITHUB_LANE,
      { enabled: true, scope: ["a", "b"] },
      { enabled: true, scope: ["b", "a"] },
    ),
  ).toBe(true);
  expect(
    sameDesired(GITHUB_LANE, { enabled: true, permissions: { issues: "read" } }, { enabled: true }),
  ).toBe(false);
});

test("a credential provided externally is never rotated by Crewlet", () => {
  const external = sre();
  external.observed.credential = {
    variable: "V",
    minted_at: new Date().toISOString(),
    source: "external",
  };
  expect(availableActions(DATADOG_LANE, external, false, false)).toEqual([]);
});

test("an agent that left the company reads as such, whatever its sync", () => {
  expect(statusOf(DATADOG_LANE, { ...sre(), status: "departed" }, true)).toMatchObject({
    label: "Agent left",
    tone: "warning",
  });
});

test("an empty Mattermost channel list is literal: no channels, and a hint while on", () => {
  expect(scopeWords(MATTERMOST_LANE, [])).toBe("No channels");
  expect(emptyScopeHint(MATTERMOST_LANE, { enabled: true, scope: [] })).toMatch(
    /cannot read or post anywhere/,
  );
  expect(emptyScopeHint(MATTERMOST_LANE, { enabled: false })).toBeNull();
  expect(emptyScopeHint(MATTERMOST_LANE, { enabled: true, scope: ["incidents"] })).toBeNull();
  expect(emptyScopeHint(GITHUB_LANE, { enabled: true })).toBeNull();
});

test("an operator-written document holds only a reader who is not an admin; any other managed one holds all", () => {
  const line = "Access settings are managed by the operator through each agent's Seat object.";
  expect(accessHold("Managed by the operator.", true, line)).toBeUndefined();
  expect(accessHold("Managed by the operator.", false, line)).toBe(
    "Admins change an agent's access.",
  );
  expect(accessHold("Managed by GitOps.", true, undefined)).toBe("Managed by GitOps.");
  expect(accessHold(null, true, undefined)).toBeUndefined();
});

describe("a lane of sets (Confluence by operation)", () => {
  test("stores the chosen ids sorted, once each, and the empty level as nothing", () => {
    expect(joinLevels(CONFLUENCE_LANE, ["read:space", "create:page", "read:space"])).toBe(
      "create:page read:space",
    );
    expect(joinLevels(CONFLUENCE_LANE, [""])).toBe("");
    expect(joinLevels(CONFLUENCE_LANE, [])).toBe("");
  });

  test("any choice brings the level the engine requires, and none brings nothing", () => {
    expect(joinLevels(CONFLUENCE_LANE, ["create:page"])).toBe("create:page read:space");
    // A LANE THE ENGINE REQUIRES NOTHING OF stores what was chosen.
    expect(joinLevels({ ...CONFLUENCE_LANE, required_level: undefined }, ["create:page"])).toBe(
      "create:page",
    );
  });

  test("a set without the required level is held, in the engine's words", () => {
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "create:page" })).toBe(
      "Confluence grants nothing else without View. Add it to the set.",
    );
    expect(
      refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "create:page read:space" }),
    ).toBeNull();
  });

  test("names the set in the vendor's order, and the empty set as product access only", () => {
    expect(levelName(CONFLUENCE_LANE, "create:page read:space")).toBe("View, Add pages");
    expect(levelName(CONFLUENCE_LANE, "")).toBe("Product access only");
  });

  test("the same set in another order is no change", () => {
    expect(
      sameDesired(
        CONFLUENCE_LANE,
        { enabled: true, level: "read:space create:page" },
        { enabled: true, level: "create:page read:space" },
      ),
    ).toBe(true);
  });

  test("product access only is valid, and refused only together with a space list", () => {
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "" })).toBeNull();
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "", scope: ["ENG"] })).toMatch(
      /takes no spaces/,
    );
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: false })).toBeNull();
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "read:space" })).toBeNull();
  });
});

test("Jira's empty level is product access only, which has no scope", () => {
  expect(productOnly(JIRA_LANE, "")).toBe("Product access only");
  expect(productOnly(JIRA_LANE, undefined)).toBe("Product access only");
  expect(productOnly(JIRA_LANE, "10003")).toBeNull();
  expect(productOnly(DATADOG_LANE, "")).toBeNull();
  expect(productOnly(CONFLUENCE_LANE, "")).toBe("Product access only");
  expect(levelName(JIRA_LANE, "")).toBe("Product access only");
  expect(scopeApplies(JIRA_LANE, { enabled: true, level: "" })).toBe(false);
  expect(scopeApplies(JIRA_LANE, { enabled: true, level: "10003" })).toBe(true);
  // A LIST HELD WITH IT is offered, so it can be cleared.
  expect(scopeApplies(JIRA_LANE, { enabled: true, level: "", scope: ["ENG"] })).toBe(true);
});

test("a lane with roles and no empty level refuses an agent that is on with none", () => {
  expect(refusedLevel(DATADOG_LANE, { enabled: true })).toBe(
    "Choose a Datadog role for every agent that is on.",
  );
  expect(refusedLevel(DATADOG_LANE, { enabled: true, level: "r-ro" })).toBeNull();
  expect(refusedLevel(DATADOG_LANE, { enabled: false })).toBeNull();
  // GitHub has permissions rather than levels, and Jira an empty level of its own.
  expect(refusedLevel(GITHUB_LANE, { enabled: true })).toBeNull();
  expect(refusedLevel(JIRA_LANE, { enabled: true, level: "" })).toBeNull();
});

test("an agent that left the company reads as such when its details open, not as off", () => {
  const kept = { ...offLane(), observed: { exists: true, enabled: false } };
  expect(offSentence(DATADOG_LANE, kept, true)).toBe(
    "This agent left the company. Its account is kept, disabled, until you remove it.",
  );
  expect(offSentence(DATADOG_LANE, offLane(), true)).toBe(
    "This agent left the company and has no account here.",
  );
});

// A HANDOFF IS READ OFF THE ENGINE'S WAIT AND THE LANE'S OWN LEVELS, never
// guessed: the role in the vendor's word with its own line, the role it shows
// now, the robot by the name the engine sent, and whether this is a new token.
describe("a handoff", () => {
  const pm = (answer: ReturnType<typeof expoWaitingAnswer>) => answer.seats[0]!.lanes.expo!;

  test("a new agent's wait is a handoff from its robot, at the role chosen here", () => {
    expect(handoffOf(EXPO_LANE, pm(expoWaitingAnswer("create")))).toEqual({
      step: "create",
      url: "https://expo.dev/accounts/acme/settings/access-tokens",
      said: "Add a robot named crewlet-acme-pm with the Runner role at Expo",
      account: "crewlet-acme-pm",
      name: "crewlet-acme-pm",
      role: { name: "Runner", note: "View all projects and perform low-risk actions on them" },
      level: "runner",
      current: undefined,
      renew: false,
      rotating: false,
    });
  });

  test("a refused token is a new one, and a changed role says what Expo shows", () => {
    expect(handoffOf(EXPO_LANE, pm(expoWaitingAnswer("mint")))).toMatchObject({
      step: "mint",
      renew: true,
    });
    expect(handoffOf(EXPO_LANE, pm(expoWaitingAnswer("rerole")))).toMatchObject({
      step: "rerole",
      current: "Developer",
      role: { name: "Runner" },
    });
  });

  test("the robot is named only as the engine named it", () => {
    for (const step of ["create", "mint"] as const) {
      const handoff = handoffOf(EXPO_LANE, pm(expoWaitingAnswer(step, { named: false })));
      expect(handoff?.account).toBe(undefined);
      expect(handoff?.name).toBe(undefined);
    }
  });

  // A ROTATE IS A PASTE FOR THE ROBOT THE SEAT ALREADY HAS: a new token, opened
  // at the paste on the lane's own page at the vendor, with no wait behind it.
  // Every other lane rotates by itself.
  test("a rotate opens a new token for the robot the seat has, and only on Expo", () => {
    const qa = expoAnswer().seats.find((s) => s.handle === "qa")!.lanes.expo!;
    expect(rotateHandoffOf(EXPO_LANE, qa)).toMatchObject({
      step: "mint",
      url: "https://expo.dev/accounts/acme/settings/access-tokens",
      account: "crewlet-acme-qa",
      role: { name: "Developer" },
      renew: true,
      rotating: true,
    });
    expect(rotateHandoffOf(DATADOG_LANE, sre())).toBeUndefined();
  });

  // DELETING AT THE VENDOR IS A WAIT OF ITS OWN, drawn on the row with its
  // page; and a vendor that makes its own accounts never hands one off.
  test("deleting a robot or a token is no handoff, and no other lane has one", () => {
    const lane = pm(expoWaitingAnswer("mint"));
    for (const step of ["revoke", "remove"] as const) {
      expect(
        handoffOf(EXPO_LANE, {
          ...lane,
          sync: { ...lane.sync!, error: { ...lane.sync!.error!, step } },
        }),
      ).toBeUndefined();
    }
    expect(
      handoffOf(EXPO_LANE, { ...lane, sync: { ...lane.sync!, outcome: "ok" } }),
    ).toBeUndefined();
    expect(handoffOf(DATADOG_LANE, lane)).toBeUndefined();
  });

  // A DIGITALOCEAN TOKEN IS NAMED BY THE ENGINE, NEVER BY WHOSE IT IS: every
  // token there acts as the person who made it, so the observed account
  // ("Alex on acme") is no name to type. Mutation: fall back to the
  // observed account and a new token is offered its owner's name.
  test("a DigitalOcean token is named by hand_in alone, never by its owner", () => {
    const pm = (answer: ReturnType<typeof digitaloceanWaitingAnswer>) =>
      answer.seats[0]!.lanes.digitalocean!;
    expect(handoffOf(DIGITALOCEAN_LANE, pm(digitaloceanWaitingAnswer("create")))).toMatchObject({
      step: "create",
      url: "https://cloud.digitalocean.com/account/api/tokens",
      name: "crewlet-acme-pm-operate",
      level: "operate",
      renew: false,
    });
    const renewed = handoffOf(
      DIGITALOCEAN_LANE,
      pm(digitaloceanWaitingAnswer("mint", { named: false })),
    );
    expect(renewed).toMatchObject({ account: "Alex on acme", renew: true });
    expect(renewed?.name).toBeUndefined();
    const sre = digitaloceanAnswer().seats[1]!.lanes.digitalocean!;
    expect(rotateHandoffOf(DIGITALOCEAN_LANE, sre)).toMatchObject({
      name: "crewlet-acme-sre-operate",
      rotating: true,
    });
  });
});

// A LEVEL'S SCOPES ARE TICKED FROM THE ENGINE'S LISTS: every read at once,
// the reads it must not hold unticked, then its writes section by section,
// the whole section where that takes fewer clicks. Nothing is listed here.
describe("a scope guide", () => {
  // Mutation: count a higher level's grants as unneeded rather than refused,
  // or drop the section's own extras, and the Operate guide changes.
  test("Operate unticks the refused and unneeded reads, then ticks its writes by section", () => {
    expect(scopeGuide(DIGITALOCEAN_LANE, "operate")).toEqual({
      refused: ["billing:read", "security:read"],
      unneeded: ["genai:read"],
      sections: [
        { resource: "droplet", tick: [], untick: ["droplet:admin"] },
        { resource: "firewall", tick: [], untick: [] },
        { resource: "kubernetes", tick: [], untick: ["kubernetes:access_cluster"] },
        { resource: "project", tick: ["project:assign_resource"], untick: [] },
        { resource: "snapshot", tick: [], untick: [] },
      ],
    });
  });

  test("Administer keeps its own scopes, and Read ticks no section", () => {
    expect(scopeGuide(DIGITALOCEAN_LANE, "administer")?.sections).toEqual([
      { resource: "droplet", tick: [], untick: [] },
      { resource: "firewall", tick: [], untick: [] },
      { resource: "kubernetes", tick: [], untick: [] },
      { resource: "project", tick: ["project:assign_resource"], untick: [] },
      { resource: "snapshot", tick: [], untick: [] },
    ]);
    expect(scopeGuide(DIGITALOCEAN_LANE, "read")).toEqual({
      refused: ["billing:read", "security:read"],
      unneeded: ["genai:read"],
      sections: [],
    });
  });

  // A READ ONLY A HIGHER LEVEL GRANTS is ticked by the bulk select too, and the
  // paste refuses it below that level, so it is unticked with the refused.
  // Mutation: leave higher levels' grants out of the refused list.
  test("a read only a higher level grants is unticked below it", () => {
    const lane: Lane = {
      ...DIGITALOCEAN_LANE,
      levels: [
        { id: "low", name: "Low", grants: ["droplet:read"] },
        { id: "high", name: "High", grants: ["droplet:read", "database:read"] },
      ],
      refused: [],
      unneeded: [],
    };
    expect(scopeGuide(lane, "low")?.refused).toEqual(["database:read"]);
    expect(scopeGuide(lane, "high")?.refused).toEqual([]);
  });

  test("a level that names no scopes has no guide", () => {
    expect(scopeGuide(EXPO_LANE, "runner")).toBeUndefined();
    expect(scopeGuide(DIGITALOCEAN_LANE, undefined)).toBeUndefined();
  });
});

// A SCOPE ENTRY IS STORED BY ITS IDENTIFIER AND READ BY ITS NAME: where the
// vendor names it apart from its ID, the picker labels it by name with the ID
// beneath, and every sentence names it. One the seat holds and the connection
// no longer reaches is still offered, by its ID, so a person can clear it.
describe("a scope entry's name", () => {
  const named: Lane = {
    ...JIRA_LANE,
    scope_names: { ENG: "Engineering", OPS: "Operations" },
  };

  test("labels the picker by name, with the identifier beneath", () => {
    expect(scopeChoices(named, ["ENG"])).toEqual([
      { value: "ENG", label: "Engineering", description: "ENG" },
      { value: "OPS", label: "Operations", description: "OPS" },
    ]);
    expect(scopeChoices(JIRA_LANE, [])).toEqual([
      { value: "ENG", label: "ENG" },
      { value: "OPS", label: "OPS" },
    ]);
  });

  test("keeps an entry the connection no longer reaches, by its identifier", () => {
    expect(scopeChoices(named, ["ENG", "GONE"]).at(-1)).toEqual({ value: "GONE", label: "GONE" });
  });

  test("names the entries in every sentence", () => {
    expect(scopeWords(named, ["OPS", "ENG"])).toBe("Operations and Engineering");
    expect(scopeWords(named, ["ENG", "OPS", "GONE"])).toBe("Engineering, Operations and GONE");
    expect(scopeWords(JIRA_LANE, ["ENG"])).toBe("ENG");
  });
});

// A PICKER WITH NOTHING TO OFFER SAYS WHY: the list could not be read, which
// says nothing about what exists, or it was read and holds nothing. An open
// connection takes typed entries, so it always has something to offer.
test("a scope picker with nothing to offer says whether the list was read", () => {
  const empty: Lane = { ...GITHUB_LANE, scope: undefined };
  expect(scopeUnavailable({ ...empty, scope_live: false }, [])).toEqual({
    words: "Crewlet could not read your repositories",
  });
  expect(scopeUnavailable(empty, [])).toEqual({ words: "No repositories to choose from" });
  expect(scopeUnavailable({ ...empty, scope_open: true }, [])).toBeNull();
  expect(scopeUnavailable({ ...empty, scope_live: false }, ["acme/backend"])).toBeNull();
  expect(scopeUnavailable(GITHUB_LANE, [])).toBeNull();
});

// A LANE WHERE AN EMPTY SCOPE REACHES NOTHING refuses an agent that is on with
// none, as the engine would, and says so under the row before the review.
test("a lane whose empty scope reaches nothing refuses an agent that is on with none", () => {
  const required: Lane = { ...GITHUB_LANE, scope_required: true };
  expect(refusedScope(required, { enabled: true, scope: [] }, "SRE")).toBe(
    "Choose at least one repository for SRE: an agent in no repository can reach nothing.",
  );
  expect(refusedScope(required, { enabled: true, scope: ["acme/backend"] }, "SRE")).toBeNull();
  expect(refusedScope(required, { enabled: false }, "SRE")).toBeNull();
  expect(refusedScope(GITHUB_LANE, { enabled: true, scope: [] }, "SRE")).toBeNull();
  expect(emptyScopeHint(required, { enabled: true })).toBe(
    "Not in any repository yet, so it can reach nothing. Choose its repositories.",
  );
  expect(emptyScopeHint(GITHUB_LANE, { enabled: true })).toBeNull();
});

// A LANE THAT IS NOT SCOPED offers no scope, whatever an answer lists.
test("a scope is offered only on a lane that narrows by scope", () => {
  expect(scopeApplies(GITHUB_LANE, { enabled: true })).toBe(true);
  expect(scopeApplies({ ...GITHUB_LANE, scoped: false }, { enabled: true })).toBe(false);
});

// THE ONLY ENTRY THERE IS: on a lane where an empty scope reaches nothing, a
// seat turned on takes the one entry a list read just now holds, and nowhere
// else: not on a lane without the rule, not from several entries or a list
// that could not be read, and never for a seat that is already on.
test("a seat turned on takes the only entry there is, and only then", () => {
  const one: Lane = { ...GITHUB_LANE, scope: ["acme/backend"], scope_required: true };
  expect(onlyScope(one, { enabled: false })).toEqual(["acme/backend"]);
  expect(onlyScope({ ...one, scope_required: false }, { enabled: false })).toBeUndefined();
  expect(onlyScope({ ...one, scope_live: false }, { enabled: false })).toBeUndefined();
  expect(onlyScope({ ...one, scope: ["a/b", "a/c"] }, { enabled: false })).toBeUndefined();
  expect(onlyScope(one, { enabled: true })).toBeUndefined();
  expect(onlyScope(one, { enabled: false, scope: ["acme/backend"] })).toBeUndefined();
});

// A BOX TOOL READING SEVERAL VARIABLES names every one its tool reads, never
// the settings Crewlet puts beside them, which the details list on their own.
test("a box tool reading several variables names each, and its settings apart", () => {
  const boxed = {
    ...offLane(),
    tool: "live",
    box: {
      env: [
        { name: "SCW_ACCESS_KEY", value: "${SCW_ACCESS_KEY_SRE}", tool: true },
        { name: "SCW_SECRET_KEY", value: "${SCW_SECRET_KEY_SRE}", tool: true },
        { name: "SCW_DEFAULT_PROJECT_ID", value: "p-1" },
      ],
    },
  } satisfies AccessSeatLane;
  expect(toolWords(EXPO_LANE, boxed)).toBe(
    "Live, EAS CLI in its box, as SCW_ACCESS_KEY and SCW_SECRET_KEY",
  );
  expect(boxSettings(boxed)).toBe("SCW_DEFAULT_PROJECT_ID");
  expect(boxSettings(offLane())).toBeNull();
});

// A SEAT WITH NO TOOL IS TOLD ONLY ABOUT A TOOL SERVER: a box tool's
// credentials are Crewlet's to set, so no remedy ever names a sandbox.env line.
test("the no-tool remedy names the tool server alone", () => {
  const both: Lane = { ...EXPO_LANE, tool_server: "expo" };
  const qa = {
    ...expoAnswer().seats.find((s) => s.handle === "qa")!.lanes.expo!,
    tool: "no_server" as const,
  };
  expect(toolWords(both, qa)).toBe("Not set up: no expo tool server is declared");
  expect(toolWords(both, { ...qa, tool: "server_off" })).toBe(
    "Not running: the expo tool server is turned off",
  );
  expect(noToolRemedy(both)).toBe(
    "the company declares no expo tool server. Add it under Settings › MCP & tools as one instance per agent, and each agent's own credential is sent with it.",
  );
  expect(noToolRemedy(EXPO_LANE)).not.toMatch(/sandbox/);
});

// WHAT CREWLET PUTS IN THE BOX FOR A CLI names its files too, after its
// variables, so a token file a CLI reads is said where it lives.
test("the settings Crewlet puts in a box name its files after its variables", () => {
  const sre = neutralAnswer().seats[1]!.lanes[NEUTRAL_LANE.key]!;
  expect(boxSettings(sre)).toBe("VENDOR_TOKEN_FILE and .config/crewlet/token");
  expect(boxSettings({ ...sre, box: { env: [], files: sre.box!.files } })).toBe(
    ".config/crewlet/token",
  );
});

// A LANE WHOSE CLI MUST COME WITH THE BOX IMAGE says so beside what Crewlet
// sets there, and a lane with nothing set says nothing at all.
test("the settings Crewlet sets carry the lane's note on them, and nothing without settings", () => {
  const sre = neutralAnswer().seats[1]!.lanes[NEUTRAL_LANE.key]!;
  const words = laneWords(NEUTRAL_LANE.key);
  expect(boxSettingsFact(NEUTRAL_LANE, sre)).toEqual({
    settings: "VENDOR_TOKEN_FILE and .config/crewlet/token",
  });
  expect(
    boxSettingsFact(NEUTRAL_LANE, sre, { ...words, boxNote: "the CLI must be in the box image" }),
  ).toEqual({
    settings: "VENDOR_TOKEN_FILE and .config/crewlet/token",
    note: "the CLI must be in the box image",
  });
  expect(
    boxSettingsFact(NEUTRAL_LANE, offLane(), {
      ...words,
      boxNote: "the CLI must be in the box image",
    }),
  ).toBeNull();
});

// A CREDENTIAL CREWLET ISSUES AT USE has no time any node shares, so it reads
// as how it is issued, for as long as its lane says one lasts; a minted one
// keeps its time, and one provided from outside says so.
test("a credential reads as issued at use, minted when, or provided externally", () => {
  const now = Date.now();
  const issued = neutralAnswer().seats[1]!.lanes[NEUTRAL_LANE.key]!.observed;
  const words = laneWords(NEUTRAL_LANE.key);
  expect(credentialNote(NEUTRAL_LANE, issued, now)).toBe(", issued at use");
  expect(credentialNote(NEUTRAL_LANE, issued, now, { ...words, issued: "15 minutes" })).toBe(
    ", issued for 15 minutes at a time, at use",
  );
  const minted = {
    ...issued,
    credential: onLane({ variable: "VENDOR_TOKEN_SRE" }).observed.credential!,
  };
  expect(credentialNote(NEUTRAL_LANE, minted, now)).toBe(", minted 1d ago");
  expect(
    credentialNote(
      NEUTRAL_LANE,
      { ...minted, credential: { ...minted.credential, source: "external" } },
      now,
    ),
  ).toBe(", provided externally");
});

// A WAIT THE ENGINE CAN NAME says whose it is, so a vendor still applying a
// grant is never read as a credential somebody owes, nor as Needs you.
test("a tool waiting with the engine's reason says the reason", () => {
  const sre = neutralAnswer().seats[1]!.lanes[NEUTRAL_LANE.key]!;
  const waiting = { ...sre, tool: "waiting" as const };
  expect(
    toolWords(NEUTRAL_LANE, { ...waiting, tool_reason: "the vendor is still applying the grant" }),
  ).toBe("Waiting: the vendor is still applying the grant, Its tool server, one per agent");
  expect(toolWords(NEUTRAL_LANE, waiting)).toBe(
    "Waiting for the credential, Its tool server, one per agent",
  );
  expect(statusOf(NEUTRAL_LANE, waiting, false).label).toBe("In sync");
});

// DIGITALOCEAN'S TOOL IS ITS MCP SERVER OR DOCTL IN THE BOX, and the variable
// names the box's route, never the server's. Mutation: put doctl first in the
// words and "as DIGITALOCEAN_ACCESS_TOKEN" reads as the server's.
test("a DigitalOcean agent's tool names the MCP server, then doctl with its variable", () => {
  const sre = digitaloceanAnswer().seats[1]!.lanes.digitalocean!;
  expect(toolWords(DIGITALOCEAN_LANE, sre)).toBe(
    "Live, the DigitalOcean MCP server, or doctl in its box, as DIGITALOCEAN_ACCESS_TOKEN",
  );
  expect(toolWords(DIGITALOCEAN_LANE, { ...sre, tool: "no_server" })).toBe(
    "Not set up: no digitalocean tool server is declared",
  );
});

// SCALEWAY'S LEVELS ARE CREWLET'S OWN SETS of Scaleway permission sets, named
// in the tab, with Administer marked as the administration grant it is; its
// tool reads as both routes, and an off seat says what turning it on creates.
test("a Scaleway seat reads its level, its tool by both routes and what turning it on does", () => {
  const sre = scalewayAnswer().seats.find((s) => s.handle === "sre")!.lanes.scaleway!;
  expect(accessWords(SCALEWAY_LANE, sre.observed)).toBe("Operate");
  expect(SCALEWAY_LANE.levels.find((l) => l.id === "administer")?.administers).toBe(true);
  expect(toolWords(SCALEWAY_LANE, sre)).toBe(
    "Live, the Scaleway MCP server, or scw in its box, as SCW_ACCESS_KEY and SCW_SECRET_KEY",
  );
  expect(toolWords(SCALEWAY_LANE, { ...sre, tool: "no_server" })).toBe(
    "Not set up: no scaleway tool server is declared",
  );
  expect(scopeWords(SCALEWAY_LANE, sre.desired.scope)).toBe("acme-prod");
  expect(offSentence(SCALEWAY_LANE, offLane("read"))).toBe(
    "Off. Turning it on creates a Scaleway IAM application for the agent, with a policy holding the chosen permission sets on its projects and its own API key.",
  );
  expect(availableActions(SCALEWAY_LANE, sre, false, false)).toEqual(["rotate"]);
});

// A LANE WHOSE ENTRIES A PERSON PREPARES AT THE VENDOR points an empty list at
// the docs section that says how, and only where the list was read: a list
// that could not be read says nothing about what exists.
test("an empty project list links to preparing one, only once the list was read", () => {
  const none: Lane = { ...GOOGLE_CLOUD_LANE, scope: [], scope_names: {} };
  expect(scopeUnavailable(none, [])).toEqual({
    words: "No projects are set up for Crewlet yet",
    link: {
      label: "Prepare a project",
      href: "https://docs.crewlet.ai/engine/latest/integrations/google-cloud/#prepare-a-project-for-crewlet",
    },
  });
  expect(scopeUnavailable({ ...none, scope_live: false }, [])).toEqual({
    words: "Crewlet could not read your projects",
  });
  expect(scopeUnavailable(GOOGLE_CLOUD_LANE, [])).toBeNull();
});

// THE CHANGE IN A WORD heads each agent's card in the review: what a reader
// scanning several agents looks for first. One part that moved is named by
// the lane's own noun; two at once are "Access changed", and the card's rows
// say which. Mutation: name the level for every edit and the projects and
// GitHub's permissions are miscalled.
describe("a review card's change in a word", () => {
  const [prod, staging] = SCALEWAY_LANE.scope!;
  const on = { enabled: true, level: "read", scope: [prod!] };

  test("on and off are said as such, in the tones the card takes", () => {
    expect(changeSummary(SCALEWAY_LANE, { enabled: false }, on)).toEqual({
      label: "Turned on",
      tone: "success",
    });
    expect(changeSummary(SCALEWAY_LANE, on, { ...on, enabled: false })).toEqual({
      label: "Turned off",
      tone: "danger",
    });
  });

  test("one part that moved is named in the lane's words", () => {
    expect(changeSummary(SCALEWAY_LANE, on, { ...on, level: "administer" }).label).toBe(
      "Level changed",
    );
    expect(changeSummary(SCALEWAY_LANE, on, { ...on, scope: [prod!, staging!] }).label).toBe(
      "Projects changed",
    );
    expect(
      changeSummary(
        GITHUB_LANE,
        { enabled: true, permissions: { issues: "read" } },
        { enabled: true, permissions: { issues: "write" } },
      ).label,
    ).toBe("Permissions changed");
  });

  test("two parts at once are a change of access", () => {
    expect(
      changeSummary(SCALEWAY_LANE, on, { ...on, level: "operate", scope: [staging!] }),
    ).toEqual({ label: "Access changed", tone: "info" });
  });
});

// A STEP THAT TAKES ACCESS AWAY takes the danger node in the review: a
// disable, revoke or delete done to turn an agent off, revoke its credential
// or remove its account. The revoke that retires a key a mint, a level change
// or a person's clean-up replaced leaves the agent working, and an off lane's
// narrowing of a shared token is a mint. Mutation: judge by the verb alone and
// the retiring revokes turn red.
test("only a step that takes an agent's access away is drawn as danger", () => {
  const step = (op: AccessStep["op"], verb: AccessStep["verb"]): AccessStep => ({
    op,
    verb,
    by: "crewlet",
    text: "A step",
  });
  expect(takesAccessAway(step("disable", "disable"))).toBe(true);
  expect(takesAccessAway(step("disable", "revoke"))).toBe(true);
  expect(takesAccessAway(step("revoke", "revoke"))).toBe(true);
  expect(takesAccessAway(step("remove", "remove"))).toBe(true);
  expect(takesAccessAway(step("disable", "mint"))).toBe(false);
  expect(takesAccessAway(step("mint", "revoke"))).toBe(false);
  expect(takesAccessAway(step("level", "revoke"))).toBe(false);
  expect(takesAccessAway(step("retire", "revoke"))).toBe(false);
  expect(takesAccessAway(step("enable", "revoke"))).toBe(false);
});

describe("reviewWords", () => {
  test("names the agent and its count, one and many", () => {
    expect(reviewWords(1, "Site Reliability Engineer")).toBe(
      "1 open item from Site Reliability Engineer was raised under its current access; it will review them after you save.",
    );
    expect(reviewWords(3, "Site Reliability Engineer")).toBe(
      "3 open items from Site Reliability Engineer were raised under its current access; it will review them after you save.",
    );
  });

  // A FLOOR SAYS SO: past the tracker's ceiling the count is the least it holds.
  test("says at least where the count is a floor", () => {
    expect(reviewWords(10000, "Site Reliability Engineer", true)).toBe(
      "At least 10,000 open items from Site Reliability Engineer were raised under its current access; it will review them after you save.",
    );
  });
});
