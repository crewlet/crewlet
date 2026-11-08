// @vitest-environment node
/**
 * What the editor and the dialogs read about a node.
 *
 * What these protect: the engine's answers about a node are read only from a
 * check of the draft as it stands; the provider an unpinned seat runs on follows the
 * engine's own fallback; a credential reaches a screen only as the name of a
 * sealed entry; and a seat counts as working while a turn or its executor is
 * in flight.
 */

import { describe, expect, test } from "vitest";
import type { AgentRow, CompanyDocument, OrgProjection, ExecutorEntry } from "~/protocol/index.ts";
import type { SeatActivity } from "~/contract/wire.ts";
import { isWholeReference } from "~/lib/references.ts";
import { locate } from "./model/draft.ts";
import { builderReducer } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import {
  companyIdentities,
  derivedSeatOf,
  derivedUnitOf,
  gitLabAccessLevel,
  hasGitLabProvisioning,
  homeUnitOf,
  identityProblemOf,
  isConnected,
  isWorking,
  mattermostBotUsername,
  nameOfHandle,
  placementSummary,
  providerOrder,
  readsCodeAccess,
  referenceNames,
  seatLabels,
  takenIdentities,
  TOOL_NAMES,
  toolCredentialNames,
  unitsLedBy,
  unpinnedProvider,
  vendorIdentities,
} from "./nodeFacts.ts";
import { keyedState, recheck } from "./testState.ts";

describe("the engine's answers about a node", () => {
  test("a seat of the base, and a new seat once a check describes it, are read from that check", () => {
    const state = keyedState(fixtureCompany());
    expect(derivedSeatOf(state, "seat:dev")?.handle).toBe("dev");
    const added = builderReducer(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:sales", after: null },
        data: { name: "Quality Lead", handle: "qa" },
      },
    });
    expect(derivedSeatOf(added, "new:qa")).toBeUndefined();
    const checked = recheck(added);
    expect(derivedSeatOf(checked, "new:qa")?.handle).toBe("qa");
    expect(nameOfHandle(checked, "qa")).toBe("Quality Lead");
    expect(nameOfHandle(checked, "nobody")).toBe("nobody");
  });

  // A check of an older draft described a company the operator has since
  // changed, so a dialog reading its manager or lead would state an old
  // answer as the consequence of the next change. A seat's handle is another
  // matter: the draft holds it, so nothing waits on a check to know it.
  test("a check of an older draft answers nothing about the draft as it stands", () => {
    const added = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:sales", after: null },
        data: { name: "Quality Lead", handle: "qa" },
      },
    });
    const checked = recheck(added);
    expect(derivedUnitOf(checked, "unit:sales")).toBeDefined();
    const later = builderReducer(checked, {
      type: "record",
      intent: {
        type: "updateUnit",
        target: "unit:sales",
        set: [{ path: ["purpose"], value: "Sell" }],
      },
    });
    expect(derivedSeatOf(later, "new:qa")).toBeUndefined();
    expect(derivedUnitOf(later, "unit:sales")).toBeUndefined();
    expect(homeUnitOf(later, "new:qa")).toBeUndefined();
  });

  test("the units a seat leads are the ones that declare its handle", () => {
    const state = keyedState(fixtureCompany());
    expect(unitsLedBy(state.draft, "vp-engineering").map((u) => u.data.name)).toEqual([
      "Engineering",
    ]);
    expect(unitsLedBy(state.draft, "VP Engineering")).toEqual([]);
    expect(unitsLedBy(state.draft, "sre")).toEqual([]);
  });

  // A pick is written as a handle, so two seats sharing a name must read
  // apart in the list they are picked from, and a seat alone with its name
  // reads as the name.
  test("a seat is offered by its name, with its handle beside it only where the name is shared", () => {
    const doc = fixtureCompany();
    doc.roles!.push({ name: "CEO", handle: "ceo-2" });
    const labels = seatLabels(keyedState(doc).draft);
    expect(labels.get("ceo")).toBe("CEO (@ceo)");
    expect(labels.get("ceo-2")).toBe("CEO (@ceo-2)");
    expect(labels.get("designer")).toBe("Designer");
  });

  // A removed node's identity still answers for its memory and mailbox, so a
  // created node may not take it until a save has made the removal the base.
  test("an identity held by the draft or by the saved company is refused, except the node's own", () => {
    const removed = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: { type: "remove", target: "seat:sre" },
    });
    const taken = takenIdentities(removed, new Set());
    expect(identityProblemOf(taken, "seat", "sre", "")).toMatch(/already names/);
    expect(identityProblemOf(taken, "seat", "engineering", "")).toMatch(/already names/);
    expect(identityProblemOf(taken, "seat", "ceo", "ceo")).toBeNull();
    expect(identityProblemOf(taken, "seat", "quality", "")).toBeNull();
    expect(identityProblemOf(taken, "unit", "Ops", "")).toMatch(/lowercase/);
    expect(identityProblemOf(taken, "unit", "ops", "")).toBeNull();
  });

  // A LEAD'S DRAFT HOLDS ONE UNIT, and the engine counts every seat and unit
  // of the company: what the org push says the running company holds is
  // taken too, so a lead is never offered a handle another team answers to.
  test("the running company's seats and units are taken beside the draft's", () => {
    const company = companyIdentities({
      name: "Acme",
      derived: {
        seats: [{ handle: "engineer", name: "Engineer", kind: "agent" }],
        units: [{ id: "platform", name: "Platform" }],
      },
    } as OrgProjection);
    expect([...company].sort()).toEqual(["engineer", "platform"]);
    const taken = takenIdentities(keyedState(fixtureCompany()), company);
    expect(identityProblemOf(taken, "seat", "engineer", "")).toMatch(/already names/);
    expect(identityProblemOf(taken, "unit", "platform", "")).toMatch(/already names/);
    expect(companyIdentities(null).size).toBe(0);
  });
});

describe("integrations", () => {
  test("a tool is connected when its block is present, whatever it holds", () => {
    const company = fixtureCompany();
    expect(isConnected(company, "datadog")).toBe(true);
    expect(isConnected(company, "slack")).toBe(false);
    expect(isConnected({ integrations: { jira: { enabled: false } } }, "jira")).toBe(true);
    expect(hasGitLabProvisioning(company)).toBe(true);
    expect(hasGitLabProvisioning({ integrations: { gitlab: {} } })).toBe(false);
    expect(gitLabAccessLevel(company, "sre")).toBe("maintainer");
    expect(gitLabAccessLevel(company, "ceo")).toBe("");
    expect(gitLabAccessLevel(company, undefined)).toBe("");
  });

  // GITLET ALONE READS A SEAT'S CODE ACCESS. GitHub registers a seat's app
  // with the seat's own permissions (`integrations.github.permissions`), so a
  // company connected to GitHub that runs no Gitlet has nothing a tier sets.
  test("a seat's code access is read only where the company runs Gitlet", () => {
    const github: CompanyDocument = { integrations: { github: { enabled: true } } };
    expect(isConnected(github, "github")).toBe(true);
    expect(readsCodeAccess(github)).toBe(false);
    expect(readsCodeAccess({ ...github, code: { backend: "gitlet" } })).toBe(true);
    expect(readsCodeAccess({ code: { backend: "gitlet" } })).toBe(true);
    expect(readsCodeAccess({ code: { backend: "none" } })).toBe(false);
  });

  // A provisioner acts only where the document names a secret store entry, and
  // it creates the bot under a name a screen must not guess: the prefix and
  // the handle, lowercased, because Mattermost usernames are.
  test("only a whole reference marks a provisioned credential, and the bot's default name carries the prefix", () => {
    expect(isWholeReference("${DEV_MM_TOKEN}")).toBe(true);
    expect(isWholeReference(" ${DEV_MM_TOKEN} ")).toBe(true);
    expect(isWholeReference("__redacted__")).toBe(false);
    expect(isWholeReference("Bearer ${DEV_MM_TOKEN}")).toBe(false);
    expect(isWholeReference(undefined)).toBe(false);

    const prefixed: CompanyDocument = {
      integrations: { mattermost: { provisioning: { username_prefix: "Agent-" } } },
    };
    expect(mattermostBotUsername(prefixed, "Dev")).toBe("agent-dev");
    expect(mattermostBotUsername({ integrations: { mattermost: {} } }, "Dev")).toBe("dev");
  });
});

describe("models", () => {
  const company = (llm: Record<string, unknown>, order?: string[]): CompanyDocument => ({
    providers: { llm, ...(order ? { llm_order: order } : {}) },
  });

  test("providers are offered in the engine's order: the declared order, then the rest sorted", () => {
    expect(
      providerOrder(company({ zeta: {}, alpha: {}, beta: {} }, ["beta", "gone", "beta"])),
    ).toEqual(["beta", "alpha", "zeta"]);
    expect(providerOrder({})).toEqual([]);
  });

  test("a seat that names no model runs on the provider keyed default, else the first in order", () => {
    expect(unpinnedProvider(company({ fast: {}, default: {} }, ["fast", "default"]))).toBe(
      "default",
    );
    expect(unpinnedProvider(company({ fast: {}, smart: {} }, ["smart", "fast"]))).toBe("smart");
    expect(unpinnedProvider({})).toBeUndefined();
  });
});

describe("credentials", () => {
  test("only a whole reference is a name; a mask and a partial reference are not", () => {
    expect(
      referenceNames({
        bot_token: "${SLACK_BOT}",
        signing_secret: "__redacted__",
        header: "Bearer ${PARTIAL}",
        nested: [{ key: " ${PADDED} " }, "${SLACK_BOT}"],
      }),
    ).toEqual(["PADDED", "SLACK_BOT"]);
  });

  // An account exists at a provisioning vendor only for a seat that is on
  // there in its own `roles[].integrations` block, at a company that
  // provisions it; a credential in `mcp_env` enrols nobody. A Cloudflare
  // token is deleted with its seat, and a seat this draft created has nothing.
  test("a vendor account is named only for an enrolled, saved seat", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      atlassian: { org_id: "acme" },
      datadog: { route_to: "sre", provisioning: { site: "datadoghq.eu" } },
      cloudflare: { enabled: true, account_id: "acct", api_token: "${CLOUDFLARE_API_TOKEN}" },
    };
    doc.units![0]!.roles![1]!.integrations = {
      confluence: { enabled: true },
      datadog: { enabled: true, role: "r-ro" },
      cloudflare: { enabled: true, level: "operate" },
    };
    doc.units![1]!.roles![0]!.integrations = { gitlab: { enabled: true, access_level: 30 } };
    // The engine refuses a seat key in mcp_env, so it names no account.
    doc.units![0]!.children![0]!.roles![0]!.mcp_env = { datadog: { DD_APP_KEY: "${SRE_DD}" } };
    doc.units![0]!.children![0]!.roles![0]!.integrations = { jira: { enabled: false } };
    const state = keyedState(doc);
    const seat = (key: string) => {
      const found = locate(state.draft, key);
      if (found?.kind !== "seat") throw new Error(key);
      return found.node;
    };
    expect(vendorIdentities(state, seat("seat:dev"))).toEqual([
      "its Datadog service account",
      "its Atlassian account",
    ]);
    expect(vendorIdentities(state, seat("seat:account-executive"))).toEqual([
      "its GitLab service account",
    ]);
    // Connected tools, a block that is off and a stale credential: no enrolment.
    expect(vendorIdentities(state, seat("seat:sre"))).toEqual([]);

    const added = builderReducer(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:sales", after: null },
        data: {
          name: "QA",
          handle: "qa",
          integrations: { slack: { bot_token: "${QA_SLACK_BOT}" } },
        },
      },
    });
    const qa = locate(added.draft, "new:qa");
    expect(qa?.kind === "seat" && vendorIdentities(added, qa.node)).toEqual([]);
  });

  // AN EXPO ROBOT STAYS AT EXPO until a person deletes it there, so deleting a
  // seat that is on at Expo names it; a seat that is off there has none.
  // Mutation: drop Expo from the vendor lanes and the robot goes unnamed.
  test("an Expo seat names the robot a person deletes at Expo", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      expo: { enabled: true, provisioning: { account_name: "acme" } },
    };
    doc.units![0]!.children![0]!.roles![0]!.integrations = {
      expo: { enabled: true, level: "developer" },
    };
    const state = keyedState(doc);
    const sre = locate(state.draft, "seat:sre");
    if (sre?.kind !== "seat") throw new Error("seat:sre");
    expect(isConnected(doc, "expo")).toBe(true);
    expect(TOOL_NAMES.expo).toBe("Expo");
    expect(vendorIdentities(state, sre.node)).toEqual(["its Expo robot"]);
    doc.units![0]!.children![0]!.roles![0]!.integrations = { expo: { enabled: false } };
    const off = keyedState(doc);
    const offSeat = locate(off.draft, "seat:sre");
    if (offSeat?.kind !== "seat") throw new Error("seat:sre");
    expect(vendorIdentities(off, offSeat.node)).toEqual([]);
  });

  // A DIGITALOCEAN TOKEN LEAVES WITH ITS SEAT: Crewlet revokes it itself once
  // the seat is off or gone, so a seat on at DigitalOcean leaves nothing there.
  test("a DigitalOcean seat names no account to decommission", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      digitalocean: { enabled: true, provisioning: { team: "acme" } },
    };
    doc.units![0]!.children![0]!.roles![0]!.integrations = {
      digitalocean: { enabled: true, level: "operate" },
    };
    const state = keyedState(doc);
    const sre = locate(state.draft, "seat:sre");
    if (sre?.kind !== "seat") throw new Error("seat:sre");
    expect(isConnected(doc, "digitalocean")).toBe(true);
    expect(TOOL_NAMES.digitalocean).toBe("DigitalOcean");
    expect(vendorIdentities(state, sre.node)).toEqual([]);
  });

  // A SCALEWAY APPLICATION LEAVES WITH ITS SEAT: the engine deletes the seat's
  // key, policy and IAM application once it is off or gone, so a seat that is
  // on at Scaleway has nothing there to decommission.
  test("a Scaleway seat names no account to decommission", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      scaleway: {
        enabled: true,
        provisioning: {
          organization_id: "33333333-3333-4333-8333-333333333333",
          access_key: "SCWXXXXXXXXXXXXXXXXX",
          secret_key: "${SCALEWAY_SECRET_KEY}",
        },
      },
    };
    doc.units![0]!.children![0]!.roles![0]!.integrations = {
      scaleway: {
        enabled: true,
        level: "operate",
        projects: ["11111111-1111-4111-8111-111111111111"],
      },
    };
    const state = keyedState(doc);
    const sre = locate(state.draft, "seat:sre");
    if (sre?.kind !== "seat") throw new Error("seat:sre");
    expect(isConnected(doc, "scaleway")).toBe(true);
    expect(TOOL_NAMES.scaleway).toBe("Scaleway");
    expect(vendorIdentities(state, sre.node)).toEqual([]);
  });

  // AN AWS ROLE OUTLIVES ITS SEAT: the engine denies it everything when the
  // seat goes and deletes it only when somebody removes it, so a seat enrolled
  // at AWS names it, and one not enrolled names nothing.
  test("an AWS seat names its IAM role to decommission", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      aws: { enabled: true, provisioning: { region: "us-east-2" } },
    };
    const role = doc.units![0]!.children![0]!.roles![0]!;
    role.integrations = { aws: { enabled: true, level: "read" } };
    const on = keyedState(doc);
    const sre = locate(on.draft, "seat:sre");
    if (sre?.kind !== "seat") throw new Error("seat:sre");
    expect(TOOL_NAMES.aws).toBe("AWS");
    expect(vendorIdentities(on, sre.node)).toEqual(["its AWS IAM role"]);
    role.integrations = { aws: { enabled: false, level: "read" } };
    const off = keyedState(doc);
    const offSeat = locate(off.draft, "seat:sre");
    if (offSeat?.kind !== "seat") throw new Error("seat:sre");
    expect(vendorIdentities(off, offSeat.node)).toEqual([]);
  });

  // A GOOGLE CLOUD SERVICE ACCOUNT OUTLIVES ITS SEAT: the engine disables it
  // when the seat goes and deletes it only when somebody removes it, so a seat
  // enrolled at Google Cloud names it. One not enrolled names nothing, by the
  // rule GitLab's and Datadog's accounts follow.
  test("a Google Cloud seat names its service account to decommission", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      google_cloud: {
        enabled: true,
        provisioning: {
          provisioner: "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com",
          provider:
            "projects/123456789/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine",
        },
      },
    };
    const role = doc.units![0]!.children![0]!.roles![0]!;
    role.integrations = {
      google_cloud: { enabled: true, level: "viewer", projects: ["acme-prod"] },
    };
    const on = keyedState(doc);
    const sre = locate(on.draft, "seat:sre");
    if (sre?.kind !== "seat") throw new Error("seat:sre");
    expect(TOOL_NAMES.google_cloud).toBe("Google Cloud");
    expect(vendorIdentities(on, sre.node)).toEqual(["its Google Cloud service account"]);
    role.integrations = { google_cloud: { enabled: false, level: "viewer", projects: [] } };
    const off = keyedState(doc);
    const offSeat = locate(off.draft, "seat:sre");
    if (offSeat?.kind !== "seat") throw new Error("seat:sre");
    expect(vendorIdentities(off, offSeat.node)).toEqual([]);
  });

  // The labels are a map, which the fact once printed as "[object Object]".
  test("a placement reads as the node it pins and the labels a node must carry", () => {
    expect(placementSummary({ node: "node-a" })).toBe("Pinned to node node-a");
    expect(placementSummary({ labels: { region: "eu", gpu: "true" } })).toBe(
      "Nodes labelled region=eu, gpu=true",
    );
    expect(placementSummary({ node: "node-a", labels: { region: "eu" } })).toBe(
      "Pinned to node node-a, which must be labelled region=eu",
    );
    expect(placementSummary({})).toBe("Any node that runs seats");
  });

  test("tool credentials are server and variable names only", () => {
    expect(
      toolCredentialNames({
        name: "Dev",
        mcp_env: { tracker: { TOKEN: "__redacted__", URL: "${TRACKER_URL}" } },
      }),
    ).toEqual([{ server: "tracker", variables: ["TOKEN", "URL"] }]);
  });
});

describe("live state", () => {
  const agent = (activity: SeatActivity): AgentRow => ({
    id: "1",
    agent_id: "1",
    role: "Dev",
    handle: "dev",
    activity,
  });
  const run: ExecutorEntry = {
    turn_id: "t",
    role: "Dev",
    agent_handle: "dev",
    agent_id: "a",
    harness: "claude",
    sandbox_id: "s",
    task: "Fix",
    status: "running",
    started_at: "2026-09-13T00:00:00Z",
    question_context: "",
  };

  test("a seat is working while a turn runs or its executor waits, and not while idle", () => {
    expect(isWorking("dev", [agent("working")], [])).toBe(true);
    expect(isWorking("dev", [agent("idle")], [run])).toBe(true);
    // An executor in its box is work in flight even when no live row names the seat.
    expect(isWorking("dev", [], [run])).toBe(true);
    expect(isWorking("dev", [agent("idle")], [])).toBe(false);
    // A seat parked on a question still has its work in flight.
    expect(isWorking("dev", [agent("needs")], [])).toBe(true);
    // A stopped seat is running nothing.
    expect(isWorking("dev", [agent("stopped")], [])).toBe(false);
    expect(isWorking("sre", [agent("working")], [])).toBe(false);
    expect(isWorking(undefined, [agent("working")], [run])).toBe(false);
  });
});
