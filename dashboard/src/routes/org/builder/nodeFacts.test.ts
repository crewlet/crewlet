// @vitest-environment node
/**
 * What the editor and the dialogs read about a node.
 *
 * What these protect: the engine's answers about a node are the SAVED chart's,
 * read only while the draft's chart is still the saved one; what the rows
 * state (a seat's home unit, the units it leads, the handle it runs under) is
 * read off the draft and the base; the provider an unpinned seat runs on
 * follows the engine's own fallback; a credential reaches a screen only as the
 * name of a sealed entry, the mask never; and a seat counts as working while a
 * turn or a coding run is in flight.
 */

import { describe, expect, test } from "vitest";
import type { AgentRow, CompanyDocument, SandboxEntry } from "~/protocol/index.ts";
import { locate } from "./model/draft.ts";
import { fixtureChart, fixtureSettings } from "./model/testkit.ts";
import {
  derivedSeatOf,
  gitLabAccessLevel,
  hasGitLabProvisioning,
  homeUnitOf,
  isConnected,
  isWholeReference,
  isWorking,
  mattermostBotUsername,
  nameOfHandle,
  placementSummary,
  providerOrder,
  referenceNames,
  savedDerivation,
  savedHandleOf,
  toolCredentialNames,
  toolServersOf,
  unitsLedBy,
  unpinnedProvider,
  vendorIdentities,
} from "./nodeFacts.ts";
import { checkedEdit, loadedState, record } from "./testState.ts";

const seatOf = (state: ReturnType<typeof loadedState>, key: string) => {
  const found = locate(state.draft, key);
  if (found?.kind !== "seat") throw new Error(key);
  return found.node;
};

describe("the engine's answers about a node", () => {
  test("are the saved chart's, read only while the draft's chart is still the saved one", () => {
    const state = checkedEdit(fixtureChart(), { seats: { dev: { manager: "vp-engineering" } } });
    expect(derivedSeatOf(state, "seat:dev")?.manager).toBe("vp-engineering");
    expect(savedDerivation(state)).toBeDefined();
    const edited = record(state, {
      type: "updateSeat",
      target: "seat:ceo",
      set: [{ path: ["goal"], value: "Lead well" }],
    });
    expect(derivedSeatOf(edited, "seat:dev")).toBeUndefined();
    expect(savedDerivation(edited)).toBeUndefined();
    // Control: undoing the edit makes the draft the saved chart again.
    const undone = record(edited, {
      type: "updateSeat",
      target: "seat:ceo",
      set: [{ path: ["goal"], value: "Lead" }],
    });
    expect(derivedSeatOf(undone, "seat:dev")?.manager).toBe("vp-engineering");
  });

  test("a derived handle is named as the draft names the seat now, else as the derivation did", () => {
    const state = checkedEdit();
    const renamed = record(state, { type: "renameSeat", target: "seat:dev", name: "Developer" });
    expect(nameOfHandle(renamed, "dev")).toBe("Developer");
    const removed = record(state, { type: "remove", target: "seat:dev" });
    expect(nameOfHandle(removed, "dev")).toBe("Dev");
    expect(nameOfHandle(state, "nobody")).toBe("nobody");
  });

  test("what the rows state is read off the draft: a seat's home unit, the units it leads, the handle it runs under", () => {
    const state = loadedState();
    expect(homeUnitOf(state.draft, "seat:dev")?.data.key).toBe("engineering");
    expect(homeUnitOf(state.draft, "seat:ceo")).toBeUndefined();
    expect(unitsLedBy(state.draft, "vp-engineering").map((u) => u.data.key)).toEqual([
      "engineering",
    ]);
    // Only a DECLARED lead: Platform inherits the VP and declares nobody.
    expect(unitsLedBy(state.draft, "vp-engineering")).toHaveLength(1);
    const renamed = record(state, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["handle"], value: "zed" }],
    });
    // The running seat answers to the saved handle until the save.
    expect(savedHandleOf(renamed, "seat:dev")).toBe("dev");
    const added = record(state, {
      type: "addSeat",
      key: "new:q",
      placement: { parent: "unit:sales" },
      data: { handle: "qa", name: "QA" },
    });
    expect(savedHandleOf(added, "new:q")).toBeUndefined();
  });
});

describe("integrations", () => {
  test("a tool is connected when its block is present, whatever it holds", () => {
    const company = fixtureSettings();
    expect(isConnected(company, "datadog")).toBe(true);
    expect(isConnected(company, "slack")).toBe(false);
    expect(isConnected({ integrations: { jira: { enabled: false } } }, "jira")).toBe(true);
    expect(hasGitLabProvisioning(company)).toBe(true);
    expect(hasGitLabProvisioning({ integrations: { gitlab: {} } })).toBe(false);
    expect(gitLabAccessLevel(company, "sre")).toBe("maintainer");
    expect(gitLabAccessLevel(company, "ceo")).toBe("");
    expect(gitLabAccessLevel(company, undefined)).toBe("");
  });

  // A provisioner acts only where the chart names a secret store entry, and
  // it creates the bot under a name a screen must not guess: the prefix and
  // the handle the seat was created under, lowercased.
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

  test("tool credentials are server and variable names only, from the runtime half", () => {
    expect(
      toolCredentialNames({
        handle: "dev",
        name: "Dev",
        runtime: { mcp_env: { tracker: { TOKEN: "__redacted__", URL: "${TRACKER_URL}" } } },
      }),
    ).toEqual([{ server: "tracker", variables: ["TOKEN", "URL"] }]);
    // No runtime half, no names: a caller that must tell "none" from "not
    // shown" asks the base (`BaseCompany.runtimeVisible`).
    expect(toolCredentialNames({ handle: "dev", name: "Dev" })).toEqual([]);
    // A seat receives its home unit's servers.
    const state = loadedState();
    const dev = seatOf(state, "seat:dev");
    expect([...toolServersOf(dev.data, homeUnitOf(state.draft, "seat:dev"))]).toEqual(["tracker"]);
  });

  // An account exists at a provisioning vendor only for a seat enrolled with
  // it, by a credential for the vendor's mcp_env server, its own or its home
  // unit's; and nothing exists anywhere for a seat this draft created.
  test("a vendor account is named only for an enrolled, saved seat", () => {
    const settings = fixtureSettings();
    settings.integrations = {
      ...settings.integrations,
      atlassian: { org_id: "acme" },
      datadog: { route_to: "sre", provisioning: { site: "datadoghq.eu" } },
    };
    const chart = fixtureChart();
    const dev = chart.seats.find((s) => s.handle === "dev")!;
    dev.runtime = {
      ...dev.runtime,
      mcp_env: {
        confluence: { CONFLUENCE_API_TOKEN: "${DEV_WIKI}" },
        datadog: { DD_APP_KEY: "__redacted__" },
      },
      slack: { bot_token: "__redacted__" },
    };
    const sales = chart.units.find((u) => u.key === "sales")!;
    sales.runtime = { mcp_env: { gitlab: { GITLAB_TOKEN: "${SALES_GITLAB}" } } };
    const state = loadedState(chart, settings);
    expect(vendorIdentities(state, seatOf(state, "seat:dev"))).toEqual([
      "its Slack app",
      "its Datadog service account",
      "its Atlassian account",
    ]);
    // Through the unit's mcp_env, which every direct member receives.
    expect(vendorIdentities(state, seatOf(state, "seat:account-executive"))).toEqual([
      "its GitLab service account",
    ]);
    // Connected tools, no enrolment.
    expect(vendorIdentities(state, seatOf(state, "seat:sre"))).toEqual([]);
    const added = record(state, {
      type: "addSeat",
      key: "new:qa",
      placement: { parent: "unit:sales" },
      data: { handle: "qa", name: "QA", runtime: { slack: { bot_token: "${QA_BOT}" } } },
    });
    expect(vendorIdentities(added, seatOf(added, "new:qa"))).toEqual([]);
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
});

describe("live state", () => {
  const agent = (state: string): AgentRow => ({
    id: "1",
    agent_id: "id-1",
    role: "Dev",
    handle: "dev",
    state,
  });
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
    expect(isWorking("dev", [], [run])).toBe(true);
    expect(isWorking("dev", [agent("idle")], [])).toBe(false);
    expect(isWorking("sre", [agent("working")], [])).toBe(false);
    expect(isWorking(undefined, [agent("working")], [run])).toBe(false);
  });
});
