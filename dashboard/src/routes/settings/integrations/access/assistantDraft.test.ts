// @vitest-environment node

/**
 * The Agents tab's draft as Ask Crewlet reads it and proposes to it.
 *
 * The invariants: every control on the tab is a field, read in the tab's own
 * words and named by the agent's place and its name and handle, the rows
 * holding a change not saved yet first; a field takes exactly what its control
 * offers and lands it through the update path the control calls; and what the
 * control would not take is refused in a sentence the person reads.
 */

import { describe, expect, test, vi } from "vitest";
import { buildPageContext, type FramePage, type PageActionSpec } from "~/app/assistant/context.ts";
import { ASSISTANT_ACTION_REPORT_LINE_MAX_BYTES } from "~/contract/assistant.ts";
import { bytesOf } from "~/lib/clip.ts";
import {
  draftSetAction,
  draftSetArgs,
  draftSnapshot,
  parseDraftChange,
} from "~/app/assistant/draft.ts";
import type { AccessAnswer, AccessSeat } from "~/contract/access.ts";
import { pageContextFits } from "~/protocol/assistantWire.ts";
import type { AccessDraft } from "./api.ts";
import { accessDraft, accessDraftPart, accessDraftsOf } from "./assistantDraft.ts";
import {
  cloudflareAnswer,
  confluenceAnswer,
  datadogAnswer,
  githubAnswer,
  gitlabAnswer,
  jiraAnswer,
  offLane,
  scalewayAnswer,
  SCW_PROD,
  SCW_STAGING,
  seat,
} from "./fixtures.ts";
import {
  joinLevels,
  lanesOf,
  withEnabled,
  withLevel,
  withPermission,
  withScope,
  type Lane,
} from "./logic.ts";
import { laneWords } from "./words.ts";

/** An answer's tab as it declares its draft, holding `draft`. */
function tab(answer: AccessAnswer, draft: AccessDraft = {}) {
  const apply = vi.fn();
  const part = accessDraftPart(
    {
      kind: answer.kind,
      name: laneWords(answer.options[0]!.lane).name,
      syncing: answer.syncing,
      lanes: lanesOf(answer),
      seats: answer.seats,
    },
    draft,
    apply,
  );
  return { spec: accessDraft(part, answer.revision), part, apply };
}

/** An answer's seat and lane by handle and key. */
function at(answer: AccessAnswer, handle: string, lane: string): { seat: AccessSeat; lane: Lane } {
  return {
    seat: answer.seats.find((s) => s.handle === handle)!,
    lane: lanesOf(answer).find((l) => l.key === lane)!,
  };
}

/** A change's draft, or the sentence it was refused in. */
function proposed(answer: AccessAnswer, values: Record<string, unknown>, draft: AccessDraft = {}) {
  const parsed = parseDraftChange(tab(answer, draft).spec, { values });
  return parsed.ok ? parsed.value.next : parsed.reason;
}

describe("the Agents tab's draft, as Ask Crewlet reads it", () => {
  // EVERY CONTROL IS A FIELD in the tab's words, and the row holding a change
  // comes first. Mutation: order the rows as the tab draws them, and QA's
  // turned-on row is second; read a level's id, and "r-ro" is sent.
  test("lists each control's value in the tab's words, the rows not saved yet first", () => {
    const answer = datadogAnswer();
    const { seat: qa, lane } = at(answer, "qa", "datadog");
    const { spec } = tab(answer, withEnabled({}, qa, lane, true));
    const levels =
      "One of: Datadog Read Only Role, Datadog Standard Role, Datadog Admin Role, SRE Responder (custom)";
    expect(draftSnapshot(spec)).toEqual({
      kind: "client",
      id: "datadog",
      revision: "rev-7",
      help: expect.stringContaining("rows with a change not saved yet first"),
      fields: [
        {
          path: "agent_2.datadog.enabled",
          label: "QA on Datadog (@qa)",
          kind: "text",
          value: "On",
          hint: "On or Off",
          editable: true,
        },
        {
          path: "agent_2.datadog.level",
          label: "QA's Datadog access (@qa)",
          kind: "text",
          value: "Datadog Read Only Role",
          hint: levels,
          editable: true,
        },
        {
          path: "agent_1.datadog.enabled",
          label: "PM on Datadog (@pm)",
          kind: "text",
          value: "Off",
          hint: "On or Off",
          editable: true,
        },
        {
          path: "agent_1.datadog.level",
          label: "PM's Datadog access (@pm)",
          kind: "text",
          value: "Datadog Read Only Role",
          hint: levels,
          editable: true,
        },
        {
          path: "agent_3.datadog.enabled",
          label: "SRE on Datadog (@sre)",
          kind: "text",
          value: "On",
          hint: "On or Off",
          editable: true,
        },
        {
          path: "agent_3.datadog.level",
          label: "SRE's Datadog access (@sre)",
          kind: "text",
          value: "Datadog Admin Role",
          hint: levels,
          editable: true,
        },
      ],
      more: 0,
    });
    expect(spec.scope).toBe("Agents' access on Datadog (not saved)");
  });

  // A PERMISSION IN GITHUB'S WORDS, a scope as the vendor names its entries,
  // and a set of levels as its picker's names. Mutation: send a permission's
  // key ("read"), or a set's ids, and the field reads as no control does.
  test("reads permissions, a scope and a set of levels as their controls show them", () => {
    const github = draftSnapshot(tab(githubAnswer()).spec).fields;
    expect(github.map((f) => [f.path, f.value])).toEqual([
      ["agent_1.github.enabled", "On"],
      ["agent_1.github.permission.contents", "Read-only"],
      ["agent_1.github.permission.issues", "Read and write"],
      ["agent_1.github.permission.pull_requests", "Read and write"],
      ["agent_1.github.permission.checks", "Read-only"],
      ["agent_1.github.permission.actions", "Read-only"],
      ["agent_1.github.permission.deployments", "Read-only"],
      ["agent_1.github.permission.workflows", "No access"],
      ["agent_1.github.scope", "acme/backend"],
    ]);
    expect(github.find((f) => f.path.endsWith(".workflows"))).toMatchObject({
      label: "QA's Workflows permission (@qa)",
      hint: "No access or Read and write",
    });
    expect(github.find((f) => f.path.endsWith(".contents"))?.hint).toBe(
      "No access, Read-only or Read and write",
    );
    expect(github.at(-1)).toMatchObject({
      label: "QA's GitHub repositories (@qa)",
      kind: "list",
      hint: "Repositories by name, any of: acme/backend, acme/frontend, acme/mobile-app. None is All installed repositories.",
    });

    const confluence = draftSnapshot(tab(confluenceAnswer()).spec).fields;
    expect(confluence.find((f) => f.path === "agent_1.confluence.levels")).toEqual({
      path: "agent_1.confluence.levels",
      label: "PM's Confluence permissions (@pm)",
      kind: "list",
      value: "View\nAdd pages",
      hint: "Any of: View, Add pages, Add blog posts, Add comments, Admin. None is Product access only; View comes with any other.",
      editable: true,
    });
    // A SCOPE ENTRY BY THE VENDOR'S NAME, where the stored id is not one.
    const scaleway = draftSnapshot(tab(scalewayAnswer()).spec).fields;
    expect(scaleway.find((f) => f.path === "agent_2.scaleway.scope")?.value).toBe("acme-prod");
  });

  // AN AGENT THAT LEFT HAS NO CONTROL, and a product managed outside Crewlet
  // has its switch alone, so neither has a field it does not draw. Its place
  // is still its own, so the agents after it keep theirs. Mutation: number
  // the agents that have fields, and SRE becomes agent_2.
  test("draws a field only for a control the row draws, each agent at its own place", () => {
    const answer = datadogAnswer();
    const left: AccessAnswer = {
      ...answer,
      seats: answer.seats.map((s) => (s.handle === "qa" ? { ...s, departed: true } : s)),
    };
    expect(draftSnapshot(tab(left).spec).fields.map((f) => f.path)).toEqual([
      "agent_1.datadog.enabled",
      "agent_1.datadog.level",
      "agent_3.datadog.enabled",
      "agent_3.datadog.level",
    ]);
    const outside: AccessAnswer = {
      ...answer,
      options: [{ ...answer.options[0]!, managed: "external" }],
    };
    expect(draftSnapshot(tab(outside).spec).fields.map((f) => f.path)).toEqual([
      "agent_1.datadog.enabled",
      "agent_2.datadog.enabled",
      "agent_3.datadog.enabled",
    ]);
  });

  // WHAT IS NOT SAVED IS THE LAST THING EITHER BOUND CUTS: forty fields at
  // most, then the page context's bytes. A company of thirty agents on both of
  // Atlassian's products holds a hundred and eighty fields; the two agents
  // turned on last on the tab are first in the draft and in the page context,
  // and draft.set, past the members one schema holds, declares nothing.
  // Mutation: order the rows as the tab draws them, and both are cut.
  test("keeps the rows holding a change inside the field bound and the page's budget", () => {
    const options = [jiraAnswer().options[0]!, confluenceAnswer().options[0]!];
    const seats = Array.from({ length: 30 }, (_, i) =>
      seat(`a${i + 1}`, `Agent ${i + 1}`, { jira: offLane(), confluence: offLane() }),
    );
    const answer: AccessAnswer = {
      kind: "atlassian",
      revision: "rev-7",
      syncing: false,
      options,
      seats,
    };
    const jira = lanesOf(answer)[0]!;
    const draft = withEnabled(withEnabled({}, seats[28]!, jira, true), seats[29]!, jira, true);
    const { spec } = tab(answer, draft);
    const snapshot = draftSnapshot(spec);
    const drafted = [
      "agent_29.jira.enabled",
      "agent_29.jira.level",
      "agent_29.jira.scope",
      "agent_30.jira.enabled",
      "agent_30.jira.level",
      "agent_30.jira.scope",
    ];
    expect(snapshot.fields.slice(0, 6).map((f) => f.path)).toEqual(drafted);
    expect(snapshot.fields).toHaveLength(40);
    expect(snapshot.more).toBe(180 - 40);
    const page: FramePage = {
      screen: "integration",
      title: "Settings › Integrations › Atlassian",
      path: ["company", "integrations", "atlassian"],
      params: {},
      query: {},
    };
    const action = { ...draftSetAction(() => spec), args: draftSetArgs(snapshot) };
    const context = buildPageContext(page, {
      draft: snapshot,
      actions: { "draft.set": action as PageActionSpec<unknown> },
    });
    expect(pageContextFits(context)).toBe(true);
    expect(context.accepts).toEqual(["draft.set"]);
    expect(context.action_args?.["draft.set"]).toBeUndefined();
    expect(context.draft?.fields.slice(0, 6).map((f) => f.path)).toEqual(drafted);
  });
});

describe("a change Ask Crewlet proposes to the Agents tab", () => {
  // EACH FIELD LANDS AS ITS CONTROL WOULD, through the update path the
  // control calls. Mutation: write the switch without the tab's update path
  // (keeping a lane that asks for what is saved), and turning SRE off and on
  // again leaves a lane in the draft.
  test("turns an agent on or off as its switch does", () => {
    const answer = datadogAnswer();
    const qa = at(answer, "qa", "datadog");
    const sre = at(answer, "sre", "datadog");
    expect(proposed(answer, { "agent_2.datadog.enabled": "On" })).toEqual(
      withEnabled({}, qa.seat, qa.lane, true),
    );
    expect(proposed(answer, { "agent_3.datadog.enabled": "off" })).toEqual(
      withEnabled({}, sre.seat, sre.lane, false),
    );
    expect(
      proposed(
        answer,
        { "agent_3.datadog.enabled": "On" },
        withEnabled({}, sre.seat, sre.lane, false),
      ),
    ).toEqual({});
  });

  // Mutation: take a level by its id, or skip the update path, and the
  // levels named here land as nothing the picker makes.
  test("chooses a level as the picker does, by the name its menu gives it", () => {
    const answer = datadogAnswer();
    const sre = at(answer, "sre", "datadog");
    expect(proposed(answer, { "agent_3.datadog.level": "Datadog Standard Role" })).toEqual(
      withLevel({}, sre.seat, sre.lane, "r-std"),
    );
    expect(proposed(answer, { "agent_3.datadog.level": "sre responder (custom)" })).toEqual(
      withLevel({}, sre.seat, sre.lane, "r-sre"),
    );
    const confluence = confluenceAnswer();
    const pm = at(confluence, "pm", "confluence");
    expect(
      proposed(confluence, { "agent_1.confluence.levels": ["View", "Add pages", "Add comments"] }),
    ).toEqual(
      withLevel(
        {},
        pm.seat,
        pm.lane,
        joinLevels(pm.lane, ["read:space", "create:page", "create:comment"]),
      ),
    );
    // VIEW COMES WITH ANY OTHER, as the picker joins it.
    expect(proposed(confluence, { "agent_1.confluence.levels": ["Add comments"] })).toEqual(
      withLevel({}, pm.seat, pm.lane, "create:comment read:space"),
    );
    // PRODUCT ACCESS ONLY TAKES NO SCOPE, so choosing it clears QA's, as the
    // picker's own change does.
    const jira = jiraAnswer();
    const qa = at(jira, "qa", "jira");
    expect(proposed(jira, { "agent_2.jira.level": "Product access only" })).toEqual({
      qa: { jira: { enabled: true, level: "", scope: [] } },
    });
    expect(withLevel({}, qa.seat, qa.lane, "")).toEqual({
      qa: { jira: { enabled: true, level: "", scope: [] } },
    });
  });

  // Mutation: keep the dialog's own merge out of the write, and setting one
  // permission drops the agent's others.
  test("sets a permission as the permissions dialog does", () => {
    const answer = githubAnswer();
    const qa = at(answer, "qa", "github");
    expect(proposed(answer, { "agent_1.github.permission.workflows": "Read and write" })).toEqual(
      withPermission({}, qa.seat, qa.lane, "workflows", "write"),
    );
    expect(proposed(answer, { "agent_1.github.permission.contents": "No access" })).toEqual(
      withPermission({}, qa.seat, qa.lane, "contents", "none"),
    );
  });

  // A SCOPE ENTRY BY THE NAME THE DIALOG SHOWS, or by its id as the dialog's
  // search finds it, and typed where the lane takes typed entries. Mutation:
  // store the name a proposal gave, and Scaleway's projects are stored as
  // names its policy cannot hold.
  test("chooses a scope as the scope dialog does", () => {
    const github = githubAnswer();
    const qa = at(github, "qa", "github");
    expect(proposed(github, { "agent_1.github.scope": ["acme/backend", "acme/frontend"] })).toEqual(
      withScope({}, qa.seat, qa.lane, ["acme/backend", "acme/frontend"]),
    );
    const scaleway = scalewayAnswer();
    const sre = at(scaleway, "sre", "scaleway");
    expect(proposed(scaleway, { "agent_2.scaleway.scope": ["acme-prod", SCW_STAGING] })).toEqual(
      withScope({}, sre.seat, sre.lane, [SCW_PROD, SCW_STAGING]),
    );
    const gitlab = gitlabAnswer();
    const dev = at(gitlab, "dev", "gitlab");
    expect(
      proposed(gitlab, { "agent_1.gitlab.scope": ["acme/backend", "acme/new-service"] }),
    ).toEqual(withScope({}, dev.seat, dev.lane, ["acme/backend", "acme/new-service"]));
  });

  // AN AGENT TURNED ON AND GIVEN ITS ACCESS IN ONE CHANGE, whichever the
  // change names first: the switch is its row's first field. Mutation: put
  // the switch after the level, and the change is refused as QA's level
  // while QA is off.
  test("turns an agent on and chooses its access in one change", () => {
    const answer = datadogAnswer();
    const qa = at(answer, "qa", "datadog");
    expect(
      proposed(answer, {
        "agent_2.datadog.level": "Datadog Admin Role",
        "agent_2.datadog.enabled": "On",
      }),
    ).toEqual(withLevel(withEnabled({}, qa.seat, qa.lane, true), qa.seat, qa.lane, "r-admin"));
  });

  // WHAT THE CONTROLS WOULD NOT TAKE, each in the sentence the card shows.
  // Mutation, one at a time: take any level named, take Read-only for
  // Workflows, take any scope entry typed on a lane that is not open, let an
  // agent that is off be given a level, take a level offered disabled, or
  // take any word for the switch, and its case here is set.
  test("refuses what the tab's controls do not offer, saying why", () => {
    expect(proposed(datadogAnswer(), { "agent_3.datadog.level": "Owner" })).toBe(
      "Datadog offers SRE no level called “Owner”. It offers Datadog Read Only Role, Datadog Standard Role, Datadog Admin Role or SRE Responder (custom).",
    );
    expect(proposed(githubAnswer(), { "agent_1.github.permission.workflows": "Read-only" })).toBe(
      "QA's Workflows permission is No access or Read and write.",
    );
    expect(
      proposed(githubAnswer(), { "agent_1.github.scope": ["acme/backend", "acme/secret"] }),
    ).toBe(
      "GitHub's connection reaches no repository called “acme/secret”. It reaches: acme/backend, acme/frontend, acme/mobile-app.",
    );
    expect(proposed(datadogAnswer(), { "agent_2.datadog.level": "Datadog Admin Role" })).toBe(
      "QA is off on Datadog, and its access is chosen once it is on. Turn it on in the same change.",
    );
    expect(proposed(cloudflareAnswer(), { "agent_2.cloudflare.level": "Secure" })).toBe(
      "The Secure level is unavailable until the provisioner token holds Zone WAF: Edit and Account Filter Lists: Edit",
    );
    expect(proposed(datadogAnswer(), { "agent_2.datadog.enabled": "Yes" })).toBe(
      "QA on Datadog is On or Off.",
    );
    expect(proposed(jiraAnswer(), { "agent_1.jira.scope": ["ENG"] })).toBe(
      "Product access only on Jira takes no projects. Choose PM's access first, in the same change.",
    );
  });

  // EVERY REFUSAL IS ONE SHORT LINE, as the page's report carries it back to
  // the model: whatever an agent, a level or a scope entry is called and
  // whatever the proposal typed, each sentence the tab can refuse in is within
  // the engine's line, and still says what the control offers, listed as far
  // as the line holds and then counted. Said whole, a refusal naming a dozen
  // repositories was past the line, and the report carrying it was refused.
  // Mutation: write a refusal as a plain template, or let `said` place a name
  // whole, and its case here is past the line.
  test("refuses in one short line, whatever the names and the value run to", () => {
    const long = "設".repeat(200);
    // EVERY NAME A PERSON OR A VENDOR WROTE, run long: each agent's, a level's
    // reason for being offered disabled, and a dozen more levels and scope
    // entries than the fixture lists, each named as a company names its own.
    const more = (prefix: string) => Array.from({ length: 12 }, (_, i) => `${prefix}-${i + 1}`);
    const stretched = (answer: AccessAnswer): AccessAnswer => ({
      ...answer,
      options: answer.options.map((o) => ({
        ...o,
        levels: [
          ...(o.levels ?? []).map((l) =>
            l.unavailable ? { ...l, unavailable: `${l.unavailable} ${long}` } : l,
          ),
          ...(o.levels?.length ? more("Custom role").map((name) => ({ id: name, name })) : []),
        ],
        ...(o.scope ? { scope: [...o.scope, ...more("acme/service")] } : {}),
      })),
      seats: answer.seats.map((s) => ({ ...s, name: `${s.name} ${long}` })),
    });
    // TWO OF A LIST'S ENTRIES CALLED ALIKE, and long: the first two levels,
    // neither of them one the agent holds, and two scope entries.
    const twins = (answer: AccessAnswer): AccessAnswer => ({
      ...answer,
      options: answer.options.map((o) => ({
        ...o,
        levels: (o.levels ?? []).map((l, i) => (i < 2 ? { ...l, name: `Twin ${long}` } : l)),
        ...(o.scope ? { scope: [`acme/${long}`, `acme/${long}`] } : {}),
      })),
    });
    const datadog = stretched(datadogAnswer());
    const github = stretched(githubAnswer());
    const confluence = stretched(confluenceAnswer());
    const typed = `Owner ${long}`;
    const cases: [string, AccessAnswer, Record<string, unknown>, RegExp][] = [
      ["a switch's word", datadog, { "agent_2.datadog.enabled": typed }, /is On or Off\.$/],
      [
        "a level for an agent that is off",
        datadog,
        { "agent_2.datadog.level": typed },
        /Turn it on in the same change\.$/,
      ],
      [
        "a level no menu offers",
        datadog,
        { "agent_3.datadog.level": typed },
        /”\. It offers .+ and \d+ more\.$/,
      ],
      [
        "a level two of the menu's share",
        twins(datadogAnswer()),
        { "agent_3.datadog.level": `Twin ${long}` },
        /: choose it on .+'s row\.$/,
      ],
      [
        "a level offered disabled",
        stretched(cloudflareAnswer()),
        { "agent_2.cloudflare.level": "Secure" },
        /^The Secure level is unavailable until .+…$/,
      ],
      [
        "a permission no picker offers",
        confluence,
        { "agent_1.confluence.levels": [typed] },
        /”\. It offers .+ and \d+ more\.$/,
      ],
      [
        "a GitHub permission's word",
        github,
        { "agent_1.github.permission.workflows": typed },
        /permission is No access or Read and write\.$/,
      ],
      [
        "a scope entry the connection does not reach",
        github,
        { "agent_1.github.scope": [typed] },
        /”\. It reaches: .+ and \d+ more\.$/,
      ],
      [
        "a scope entry two share",
        twins(githubAnswer()),
        { "agent_1.github.scope": [`acme/${long}`] },
        /on the tab\.$/,
      ],
      [
        "a scope on product access only",
        stretched(jiraAnswer()),
        { "agent_1.jira.scope": ["ENG"] },
        /, in the same change\.$/,
      ],
      [
        "a field the draft does not have",
        datadog,
        { [`agent_9.${"x".repeat(300)}`]: "On" },
        /”\.$/,
      ],
      [
        "a value of the wrong shape",
        datadog,
        { "agent_3.datadog.level": 7 },
        /is a line of text\.$/,
      ],
    ];
    for (const [what, answer, values, ends] of cases) {
      const reason = proposed(answer, values);
      expect(typeof reason, `${what} is taken`).toBe("string");
      expect(bytesOf(reason as string), `${what}: ${reason as string}`).toBeLessThanOrEqual(
        ASSISTANT_ACTION_REPORT_LINE_MAX_BYTES,
      );
      expect(reason, what).toMatch(ends);
    }
  });

  // A RUNNING SYNC HOLDS EVERY CONTROL, so the card's Approve waits with the
  // tab's sentence. Mutation: drop the gate, and a change lands mid-sync.
  test("waits while a sync runs", () => {
    expect(tab({ ...datadogAnswer(), syncing: true }).spec.blocked?.()).toBe(
      "A sync with Datadog is running. Access can be changed here once it ends.",
    );
    expect(tab(datadogAnswer()).spec.blocked?.()).toBeNull();
  });
});

describe("an agent's Access page, every tab one draft", () => {
  // THE PAGE CONTEXT CARRIES ONE DRAFT, so the agent's tabs are declared
  // together: each field read and set through its own tab, the rows holding
  // a change first across them, and a change applied to each tab it moves
  // and to no other. Mutation: apply the whole change to every tab, and the
  // Atlassian tab is handed a draft it did not change.
  test("reads and sets each tab's fields through that tab", () => {
    const datadog = datadogAnswer();
    const gitlab = gitlabAnswer();
    const only = (answer: AccessAnswer, handle: string): AccessAnswer => ({
      ...answer,
      seats: answer.seats.filter((s) => s.handle === handle),
    });
    const sre = at(datadog, "sre", "datadog");
    const first = tab(only(datadog, "sre"), withEnabled({}, sre.seat, sre.lane, false));
    const second = tab(only(gitlab, "dev"));
    const spec = accessDraftsOf([second.part, first.part], "SRE")!;
    expect(spec.scope).toBe("SRE's access (not saved)");
    expect(draftSnapshot(spec).fields.map((f) => [f.path, f.value])).toEqual([
      ["agent_1.datadog.enabled", "Off"],
      ["agent_1.datadog.level", "Datadog Admin Role"],
      ["agent_1.gitlab.enabled", "On"],
      ["agent_1.gitlab.level", "Developer"],
      ["agent_1.gitlab.scope", "acme/backend"],
    ]);
    const parsed = parseDraftChange(spec, { values: { "agent_1.gitlab.level": "Maintainer" } });
    if (!parsed.ok) throw new Error(parsed.reason);
    spec.apply?.(parsed.value.next);
    const dev = at(gitlab, "dev", "gitlab");
    expect(second.apply).toHaveBeenCalledWith(withLevel({}, dev.seat, dev.lane, "40"));
    expect(first.apply).not.toHaveBeenCalled();
  });
});
