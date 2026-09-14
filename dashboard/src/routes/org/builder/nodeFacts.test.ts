// @vitest-environment node
/**
 * What the editor and the dialogs read about a node.
 *
 * What these protect: a handle comes from the engine or the document, never a
 * derivation of the name; the provider an unpinned seat runs on follows the
 * engine's own fallback; a credential reaches a screen only as the name of a
 * sealed entry; and a seat counts as working while a turn or a coding run is
 * in flight.
 */

import { describe, expect, test } from "vitest";
import type { AgentRow, CompanyDocument, SandboxEntry } from "~/protocol/index.ts";
import { locate } from "./model/draft.ts";
import { builderReducer } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import {
  datadogFallback,
  derivedSeatOf,
  derivedUnitOf,
  gitLabAccessLevel,
  handleOf,
  hasGitLabProvisioning,
  isConnected,
  isWholeReference,
  isWorking,
  mattermostBotUsername,
  nameOfHandle,
  placementSummary,
  providerOrder,
  referenceNames,
  toolCredentialNames,
  unitsLedBy,
  unpinnedProvider,
  vendorIdentities,
} from "./nodeFacts.ts";
import { keyedState, recheck } from "./testState.ts";

describe("handles", () => {
  test("a seat of the base carries its handle in its key, and a new seat has one only once a check reports it", () => {
    const state = keyedState(fixtureCompany());
    expect(handleOf(state, "seat:dev")).toBe("dev");
    const added = builderReducer(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "Quality Lead" },
      },
    });
    expect(handleOf(added, "new:qa")).toBeUndefined();
    const checked = recheck(added, { seats: { "units[1].roles[0]": { handle: "qa-engine" } } });
    expect(handleOf(checked, "new:qa")).toBe("qa-engine");
    expect(nameOfHandle(checked, "qa-engine")).toBe("Quality Lead");
    expect(nameOfHandle(checked, "nobody")).toBe("nobody");
  });

  // A check of an older draft described a company the operator has since
  // changed, and the reducer records nothing against its handles either, so
  // a screen reading it would offer what the operation then refuses.
  test("a check of an older draft answers nothing about the draft as it stands", () => {
    const added = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "Quality Lead" },
      },
    });
    const checked = recheck(added, { seats: { "units[1].roles[0]": { handle: "qa-engine" } } });
    expect(derivedUnitOf(checked, "unit:Sales")).toBeDefined();
    const later = builderReducer(checked, {
      type: "record",
      intent: {
        type: "updateUnit",
        target: "unit:Sales",
        set: [{ path: ["purpose"], value: "Sell" }],
      },
    });
    expect(handleOf(later, "new:qa")).toBeUndefined();
    expect(derivedSeatOf(later, "new:qa")).toBeUndefined();
    expect(derivedUnitOf(later, "unit:Sales")).toBeUndefined();
    expect(nameOfHandle(later, "qa-engine")).toBe("qa-engine");
    // A seat of the base carries its handle in its key, which no check dates.
    expect(handleOf(later, "seat:dev")).toBe("dev");
  });

  test("a declared handle is the seat's own even before a check", () => {
    const state = keyedState(fixtureCompany());
    const added = builderReducer(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:ops",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "Ops", handle: "ops-lead" },
      },
    });
    expect(handleOf(added, "new:ops")).toBe("ops-lead");
  });

  test("the units a seat leads are the ones that declare it", () => {
    const state = keyedState(fixtureCompany());
    expect(unitsLedBy(state.draft, "VP Engineering").map((u) => u.data.name)).toEqual([
      "Engineering",
    ]);
    expect(unitsLedBy(state.draft, "SRE")).toEqual([]);
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
    expect(datadogFallback(company)).toBe("sre");
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

  // An account exists at a provisioning vendor only for a seat enrolled with
  // it, by a credential for the vendor's mcp_env server, its own or its home
  // unit's; and nothing exists anywhere for a seat this draft created.
  test("a vendor account is named only for an enrolled, saved seat", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      atlassian: { org_id: "acme" },
      datadog: { route_to: "sre", provisioning: { site: "datadoghq.eu" } },
    };
    doc.units![0]!.roles![1]!.mcp_env = {
      confluence: { CONFLUENCE_API_TOKEN: "${DEV_WIKI}" },
      datadog: { DD_APP_KEY: "${DEV_DD}" },
    };
    doc.units![1]!.mcp_env = { gitlab: { GITLAB_TOKEN: "${SALES_GITLAB}" } };
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
    // Through the unit's mcp_env, which every direct member receives.
    expect(vendorIdentities(state, seat("seat:account-executive"))).toEqual([
      "its GitLab service account",
    ]);
    // Connected tools, no enrolment.
    expect(vendorIdentities(state, seat("seat:sre"))).toEqual([]);

    const added = builderReducer(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "QA", integrations: { slack: { channel: "C9" } } },
      },
    });
    const qa = locate(added.draft, "new:qa");
    expect(qa?.kind === "seat" && vendorIdentities(added, qa.node)).toEqual([]);
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
  const agent = (state: string): AgentRow => ({ id: "1", role: "Dev", handle: "dev", state });
  const run: SandboxEntry = {
    turn_id: "t",
    role: "Dev",
    agent_handle: "dev",
    agent_id: "a",
    coding_agent: "claude",
    sandbox_id: "s",
    task: "Fix",
    status: "running",
    started_at: "2026-09-13T00:00:00Z",
  };

  test("a seat is working while a turn runs or a coding run waits, and not while idle", () => {
    expect(isWorking("dev", [agent("working")], [])).toBe(true);
    expect(isWorking("dev", [agent("idle")], [run])).toBe(true);
    // A coding run is work in flight even when no live row names the seat.
    expect(isWorking("dev", [], [run])).toBe(true);
    expect(isWorking("dev", [agent("idle")], [])).toBe(false);
    expect(isWorking("sre", [agent("working")], [])).toBe(false);
    expect(isWorking(undefined, [agent("working")], [run])).toBe(false);
  });
});
