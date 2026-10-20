/**
 * The integrations room and its forms answer as the engine's do: every form
 * derived from the document (the captures it was taken from reproduced
 * exactly), the public address moving every form and every registration
 * the engine compares with it, setup passes run and remembered, each agent's
 * own GitHub app judged on its two acts, deliveries counted from the event
 * log, and every card rolled up by the engine's own rule.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { IntegrationRow } from "~/contract/integrations.ts";
import type { SetupListing, SetupRun } from "~/protocol/types.ts";
import noAddress from "../fixtures/engine/setup.no-address.json";
import withAddress from "../fixtures/engine/setup.with-address.json";
import { fairsplit } from "../fixtures/fairsplit.ts";
import {
  addAgent,
  compose,
  connectIntegration,
  integrationStatus,
  integrationTraffic,
  seatApp,
  seatProvisioned,
  withPublicAddress,
  type DemoCompany,
} from "../fixtures/scenarios.ts";
import { documentOf } from "../harness/engine/company.ts";
import { DemoEngine } from "../harness/engine/engine.ts";
import type { RestReply } from "../harness/engine/rest.ts";
import { listingOf } from "../harness/engine/setup.ts";

let engine: DemoEngine;
let warned: unknown[][];

beforeEach(() => {
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
    body: body === undefined ? null : JSON.stringify(body),
  });
}

const listing = () => rest("GET", "/setup/integrations").body as SetupListing;
const form = (key: string) => listing().tools.find((t) => t.key === key)!;
const room = () => engine.answer("integrations", {});
const row = (key: string) => room().integrations.find((r) => r.key === key) as IntegrationRow;
const card = (key: string) => room().tools?.find((t) => t.key === key);

const MATTERMOST = {
  enabled: "true",
  url: "https://chat.fairsplit.example",
  team: "fairsplit",
  admin_token: "demo-admin-token",
};
const GITHUB = { enabled: "true", "provisioning.org": "fairsplit-co" };

/** The engine's capture, with the two things a capture cannot say: this company's revision, and the passes a running engine serves. */
function captured(capture: unknown, c: DemoCompany): unknown {
  const out = structuredClone(capture) as SetupListing & { revision_id: string };
  out.revision_id = c.config.revisions[c.config.revisions.length - 1]!.meta.revision_id;
  for (const tool of out.tools) {
    tool.can_provision = [
      "atlassian",
      "datadog",
      "jira",
      "confluence",
      "gitlab",
      "mattermost",
      "github",
    ].includes(tool.key);
  }
  return out;
}

describe("the forms", () => {
  test("are the engine's captures, field for field, for the node with and without an address", () => {
    const base = fairsplit();
    expect(JSON.parse(JSON.stringify(listingOf(base)))).toEqual(captured(withAddress, base));
    const unaddressed = withPublicAddress("")(base);
    expect(JSON.parse(JSON.stringify(listingOf(unaddressed)))).toEqual(
      captured(noAddress, unaddressed),
    );
  });

  test("list every agent on the chart, an agent added since included", () => {
    engine = new DemoEngine(
      addAgent({ name: "Crewlet-QA", handle: "qa", unit: "Product and Engineering" })(fairsplit()),
    );
    for (const tool of listing().tools) {
      expect(tool.seats?.map((s) => s.handle)).toEqual(["pm", "engineer", "qa"]);
    }
    expect(
      form("datadog")
        .requirements.find((r) => r.field === "route_to")
        ?.choices?.map((c) => c.value),
    ).toEqual(["none", "pm", "engineer", "qa"]);
  });

  test("name each agent's account the engine's way once its tool is connected", () => {
    engine = new DemoEngine(fairsplit());
    rest("POST", "/setup/integrations/mattermost/inputs", { values: MATTERMOST });
    const seat = form("mattermost").seats?.find((s) => s.handle === "pm");
    expect(seat?.detail).toBe(
      "not on Mattermost yet: name a ${VAR} under this seat's integrations.mattermost.bot_token and the next sync creates the account and seals it there",
    );
    rest("POST", "/setup/integrations/slack/inputs", { values: { typing_status: "addressed" } });
    rest("POST", "/setup/integrations/slack/inputs", {
      seat: "pm",
      values: { bot_token: "xoxb-1", signing_secret: "a-signing-secret" },
    });
    const slack = form("slack").seats?.find((s) => s.handle === "pm");
    expect(slack).toMatchObject({
      present: true,
      satisfied: true,
      detail: expect.stringMatching(/^App A0/),
    });
    expect(slack?.manage_url).toMatch(/^https:\/\/api\.slack\.com\/apps\/A0/);
  });
});

describe("the public address", () => {
  // TIER A, NOT THE DOCUMENT: the address is the node's `api.external_url`,
  // so it moves with the node restarted on a new one.
  const save = (value: string) => {
    engine = new DemoEngine(withPublicAddress(value)(engine.company));
  };

  test("set on the node moves every form", () => {
    engine = new DemoEngine(fairsplit());
    save("https://a.example");
    expect(listing().external_url).toEqual({
      value: "https://a.example",
      config_path: "api.external_url",
    });
    expect(form("github").public_url).toBe("https://a.example/webhooks/github");
    const pm = form("slack").seats?.find((s) => s.handle === "pm");
    expect(pm?.public_url).toBe("https://a.example/webhooks/slack/pm");
    expect(pm?.manifest).toContain('"request_url": "https://a.example/webhooks/slack/pm"');
    expect(pm?.manifest).toContain('"https://a.example/webhooks/slack-oauth"');

    save("");
    expect(listing().external_url).toEqual({ value: "", config_path: "api.external_url" });
    expect(form("github").public_url).toBeUndefined();
    expect(form("slack").seats?.[0]?.manifest_note).toMatch(/^no manifest yet/);
  });

  test("keeps what was connected, and a Slack app registered at the old address asks for action", () => {
    engine = new DemoEngine(
      compose(
        seatApp("github", "pm", "installed"),
        seatApp("github", "engineer", "installed"),
      )(fairsplit()),
    );
    save("https://a.example");
    rest("POST", "/setup/integrations/github/inputs", {
      values: GITHUB,
      generate: ["webhook_secret"],
    });
    rest("POST", "/setup/integrations/slack/inputs", { values: { typing_status: "always" } });
    rest("POST", "/setup/integrations/slack/inputs", {
      seat: "pm",
      values: { bot_token: "xoxb-1", signing_secret: "a-signing-secret" },
    });
    rest("POST", "/setup/integrations/mattermost/inputs", { values: MATTERMOST });
    expect(row("github")).toMatchObject({ endpoint: "https://a.example", endpoint_current: true });
    expect(row("slack")).toMatchObject({ endpoint: "https://a.example", endpoint_current: true });

    save("https://b.example");
    // The engine re-registers what it registered itself on the pass the apply brings forward.
    expect(row("github")).toMatchObject({ endpoint: "https://b.example", endpoint_current: true });
    expect(card("github")?.state).toBe("connected");
    expect(card("mattermost")?.state).toBe("connected");
    expect(form("mattermost").requirements.find((r) => r.field === "url")?.value).toBe(
      MATTERMOST.url,
    );
    // A Slack app's request URL is pasted by a person, so the room asks one to.
    expect(row("slack")).toMatchObject({ endpoint: "https://a.example", endpoint_current: false });
    expect(card("slack")).toMatchObject({
      state: "attention",
      label: "Action needed",
      reason:
        "Slack is registered at an address this deployment no longer answers on, so its deliveries go nowhere",
    });
    rest("POST", "/setup/integrations/slack/inputs", { values: { typing_status: "always" } });
    expect(card("slack")?.state).toBe("connected");
  });
});

describe("a setup pass", () => {
  test("Check now runs one, remembers it first and records it on the room's row", () => {
    engine = new DemoEngine(fairsplit());
    rest("POST", "/setup/integrations/mattermost/inputs", { values: MATTERMOST });
    const checked = rest("POST", "/setup/integrations/mattermost/check");
    expect(checked.status).toBe(200);
    const run = checked.body as SetupRun;
    expect(run).toMatchObject({ key: "mattermost", state: "done", report: { phase: "ready" } });
    expect(Date.parse(run.ended_at!) - Date.parse(run.started_at)).toBe(1_000);
    const runs = rest("GET", "/setup/integrations/mattermost/runs").body as { runs: SetupRun[] };
    expect(runs.runs[0]?.run_id).toBe(run.run_id);
    expect(rest("GET", `/setup/integrations/mattermost/runs/${run.run_id}`).body).toEqual(
      runs.runs[0],
    );
    expect(row("mattermost").reconcile).toMatchObject({
      last_attempt_at: run.ended_at,
      phase: "ready",
    });
  });

  test("reports what the tool says, and answers as the engine does where none can run", () => {
    engine = new DemoEngine(
      compose(
        connectIntegration("mattermost", [{ values: MATTERMOST }]),
        integrationStatus("mattermost", {
          phase: "awaiting_admin",
          actor: "admin",
          detail: "Approve the agents' bots.",
          findings: [{ kind: "approval_required", subject: "pm", detail: "Approve @pm." }],
        }),
      )(fairsplit()),
    );
    const run = rest("POST", "/setup/integrations/mattermost/check").body as SetupRun;
    expect(run.report).toEqual({
      phase: "awaiting_admin",
      actor: "admin",
      detail: "Approve the agents' bots.",
    });
    expect(run.findings?.[0]).toMatchObject({ kind: "approval_required", subject: "pm" });
    // A finding about one agent unsettles that agent's row.
    expect(form("mattermost").seats?.find((s) => s.handle === "pm")).toMatchObject({
      satisfied: false,
      detail: "Approve @pm.",
    });
    const unconfigured = rest("POST", "/setup/integrations/datadog/check");
    expect(unconfigured.status).toBe(502);
    expect(unconfigured.body).toMatchObject({
      error: "vendor_refused",
      run: { state: "failed", error: "integration: this surface is not configured" },
    });
    expect(rest("POST", "/setup/integrations/slack/check").body).toMatchObject({
      error: "not_provisionable",
    });
    expect(rest("POST", "/setup/integrations/teams/check").body).toMatchObject({
      error: "not_provisionable",
    });
  });
});

describe("an agent's own GitHub app", () => {
  test("is judged on its two acts, and a disconnect names its sealed key", () => {
    const base = connectIntegration("github", [{ values: GITHUB, generate: ["webhook_secret"] }])(
      fairsplit(),
    );
    const seat = (c: DemoCompany) => {
      engine = new DemoEngine(c);
      return form("github").seats?.find((s) => s.handle === "pm");
    };
    expect(seat(base)).toMatchObject({ step: "create_app", satisfied: false });
    expect(form("github").satisfied).toBe(false);

    const exists = seatApp("github", "pm", "install_app")(base);
    expect(seat(exists)).toMatchObject({
      present: true,
      enrolled: true,
      satisfied: false,
      step: "install_app",
      action_url:
        "https://github.com/organizations/fairsplit-co/settings/apps/fairsplit-pm/installations",
      manage_url:
        "https://github.com/organizations/fairsplit-co/settings/apps/fairsplit-pm/advanced",
      // The pass's finding about pm, folded into its row (`withLoopFindings`).
      detail:
        "pm's app exists and nothing has installed it, so it sees no repository: install it on fairsplit-co",
    });
    expect(form("github").satisfied).toBe(false);

    const installed = seatApp("github", "pm", "installed")(base);
    const done = seat(installed);
    expect(done).toMatchObject({ present: true, satisfied: true, detail: "fairsplit-pm" });
    expect(done?.step).toBeUndefined();
    expect(form("github").satisfied).toBe(true);
    const gone = rest("DELETE", "/setup/integrations/github", { remove_seats: false });
    expect((gone.body as { orphaned_secrets: string[] }).orphaned_secrets).toEqual(
      expect.arrayContaining(["GITHUB_APP_KEY_PM", "GITHUB_APP_WEBHOOK_SECRET_PM"]),
    );
  });
});

describe("an agent's provisioned account", () => {
  const DATADOG = {
    site: "datadoghq.com",
    api_key: "demo-datadog-api-key",
    app_key: "demo-datadog-application-key",
    enabled: "true",
  };
  const GITLAB = {
    enabled: "true",
    url: "https://gitlab.example.com",
    "provisioning.group": "fairsplit",
    admin_token: "glpat-demo-admin",
  };
  const ATLASSIAN = { deployment: "cloud", org_id: "org-1", api_key: "demo-atlassian-org-key" };
  const PRODUCT = { email: "agents@fairsplit.example.com", token: "demo-atlassian-token" };
  const connected = compose(
    connectIntegration("datadog", [{ values: DATADOG, generate: ["webhook_token"] }]),
    connectIntegration("gitlab", [{ values: GITLAB, generate: ["signing_secret"] }]),
    connectIntegration("mattermost", [{ values: MATTERMOST }]),
    connectIntegration("atlassian", [{ values: ATLASSIAN }]),
    connectIntegration("jira", [{ values: PRODUCT, generate: ["webhook_secret"] }]),
    connectIntegration("confluence", [{ values: PRODUCT, generate: ["webhook_token"] }]),
  );
  const seatOn = (key: string, handle = "pm") => form(key).seats?.find((s) => s.handle === handle);
  const roleOf = (c: DemoCompany, handle: string) =>
    documentOf(c).units?.[0]?.roles?.find((r) => r.handle === handle) as Record<string, unknown>;
  const sealedAs = (c: DemoCompany, name: string) => c.secrets.find((s) => s.name === name);

  test("reads ready under the account the engine names, once the pass sealed its credential", () => {
    const base = connected(fairsplit());
    engine = new DemoEngine(base);
    expect(seatOn("datadog")).toMatchObject({
      satisfied: false,
      detail:
        "not on Datadog yet: name a ${VAR} under this seat's mcp_env.datadog and the next sync creates the account and seals it there",
    });
    const c = compose(
      seatProvisioned("datadog", "pm"),
      seatProvisioned("gitlab", "pm"),
      seatProvisioned("mattermost", "pm"),
    )(base);
    engine = new DemoEngine(c);
    expect(seatOn("datadog")).toMatchObject({
      present: true,
      satisfied: true,
      detail: "crewlet-pm@agents.crewlet.invalid",
    });
    expect(seatOn("gitlab")).toMatchObject({ satisfied: true, detail: "crewlet-pm" });
    expect(seatOn("mattermost")).toMatchObject({ satisfied: true, detail: "@pm" });
    expect(seatOn("datadog", "engineer")?.satisfied).toBe(false);
    const pm = roleOf(c, "pm");
    expect(pm.mcp_env).toEqual({
      datadog: { DD_APP_KEY: "${DD_APP_KEY_PM}" },
      gitlab: { GITLAB_TOKEN: "${GITLAB_TOKEN_PM}" },
    });
    expect((pm.integrations as Record<string, unknown>).mattermost).toEqual({
      bot_token: "${MATTERMOST_TOKEN_PM}",
    });
    for (const name of ["DD_APP_KEY_PM", "GITLAB_TOKEN_PM", "MATTERMOST_TOKEN_PM"]) {
      expect(sealedAs(c, name)).toMatchObject({
        updated_by: "reconcile loop",
        source: "provision",
      });
    }
  });

  test("on Atlassian shows the address the pass recorded, on every product it serves", () => {
    const c = compose(
      seatProvisioned("atlassian", "pm"),
      seatProvisioned("atlassian", "engineer", {
        email: "swe-1a2b3c@serviceaccount.atlassian.com",
      }),
    )(connected(fairsplit()));
    engine = new DemoEngine(c);
    for (const key of ["atlassian", "jira", "confluence"]) {
      expect(seatOn(key)?.satisfied).toBe(true);
      expect(seatOn(key)?.detail).toMatch(/^pm-[0-9a-f]{6}@serviceaccount\.atlassian\.com$/);
      expect(seatOn(key, "engineer")?.detail).toBe("swe-1a2b3c@serviceaccount.atlassian.com");
    }
    expect(roleOf(c, "pm").mcp_env).toEqual({
      atlassian: { JIRA_API_TOKEN: "${PM_JIRA_TOKEN}", JIRA_USERNAME: "${PM_JIRA_USER}" },
    });
    expect(sealedAs(c, "PM_JIRA_USER")?.source).toBe("provision");
    // The address is read through the secret store, so a removed one is not shown.
    expect(rest("DELETE", "/secrets/PM_JIRA_USER").status).toBe(204);
    expect(seatOn("atlassian")).toMatchObject({
      satisfied: true,
      detail: "mcp_env.atlassian.JIRA_API_TOKEN",
    });
  });

  test("seals the variable an agent already names, and refuses a value it cannot write to", () => {
    engine = new DemoEngine(connected(fairsplit()));
    const put = (handle: string, change: (role: Record<string, unknown>) => void) => {
      const read = rest("GET", `/config/roles/${handle}`);
      const role = read.body as Record<string, unknown>;
      change(role);
      return rest("PUT", `/config/roles/${handle}`, role, { "If-Match": read.headers?.ETag ?? "" });
    };
    expect(
      put("pm", (role) => {
        role.mcp_env = { datadog: { DATADOG_APP_KEY: "${OUR_DD_KEY}" } };
      }).status,
    ).toBe(201);
    const named = engine.company;
    const after = seatProvisioned("datadog", "pm")(named);
    expect(after.config.revisions).toHaveLength(named.config.revisions.length);
    expect(sealedAs(after, "OUR_DD_KEY")).toMatchObject({ source: "provision" });
    expect(
      put("engineer", (role) => {
        role.mcp_env = { gitlab: { GITLAB_TOKEN: "glpat-written-by-hand" } };
      }).status,
    ).toBe(201);
    expect(() => seatProvisioned("gitlab", "engineer")(engine.company)).toThrow(
      "seatProvisioned: engineer's mcp_env.gitlab.GITLAB_TOKEN holds a value rather than a whole ${VAR} reference, so the GitLab pass has nowhere to write what it mints and leaves this agent to be managed by hand.",
    );
  });

  test("is refused for a tool that is not connected, or a seat that is no agent's", () => {
    expect(() => seatProvisioned("datadog", "pm")(fairsplit())).toThrow(
      "seatProvisioned: Datadog is not connected, so no pass runs for it: connect it first.",
    );
    expect(() => seatProvisioned("mattermost", "miles")(connected(fairsplit()))).toThrow(
      'seatProvisioned: There is no agent "miles"; a person has an account of their own.',
    );
  });
});

describe("GitHub's pass", () => {
  const connected = connectIntegration("github", [
    { values: GITHUB, generate: ["webhook_secret"] },
  ]);
  const reconcile = () => row("github").reconcile!;
  const NO_APP_REMEDY =
    "Create one per agent: GitHub offers no API for it, so a person clicking is the only thing that can.";

  test("names every agent with no app in one finding, as SeatsWithNoApp does", () => {
    engine = new DemoEngine(connected(fairsplit()));
    expect(reconcile()).toMatchObject({
      phase: "awaiting_admin",
      actor: "admin",
      outcome: "blocked",
      detail: "2 agents have no GitHub App of their own, so nothing they do on GitHub is theirs",
      action_url: "",
    });
    expect(reconcile().findings).toEqual([
      {
        kind: "approval_required",
        subject: "agents without an app",
        subjects: ["pm", "engineer"],
        detail: "2 agents have no GitHub App of their own, so nothing they do on GitHub is theirs",
        remedy: NO_APP_REMEDY,
        action_url: "",
        phase: "awaiting_admin",
        actor: "admin",
      },
    ]);
    expect(card("github")).toMatchObject({
      state: "attention",
      label: "Action needed",
      reason: "2 agents have no GitHub App of their own, so nothing they do on GitHub is theirs",
    });

    engine = new DemoEngine(seatApp("github", "pm", "installed")(connected(fairsplit())));
    expect(reconcile().detail).toBe(
      "engineer has no GitHub App of its own, so nothing it does on GitHub is its own",
    );
    expect(reconcile().findings).toMatchObject([
      { subject: "agents without an app", subjects: ["engineer"], remedy: NO_APP_REMEDY },
    ]);
  });

  test("asks for each app nothing installed, with GitHub's own install page", () => {
    engine = new DemoEngine(
      compose(
        connected,
        seatApp("github", "pm", "install_app"),
        seatApp("github", "engineer", "installed"),
      )(fairsplit()),
    );
    const install =
      "https://github.com/organizations/fairsplit-co/settings/apps/fairsplit-pm/installations";
    expect(reconcile()).toMatchObject({
      phase: "awaiting_admin",
      actor: "admin",
      detail:
        "pm's app exists and nothing has installed it, so it sees no repository: install it on fairsplit-co",
      action_url: install,
    });
    expect(reconcile().findings).toMatchObject([
      { kind: "approval_required", subject: "pm", action_url: install },
    ]);
  });

  test("counts its attempts, and starts the count again when the wait changes", () => {
    engine = new DemoEngine(
      compose(
        connected,
        seatApp("github", "pm", "install_app"),
        seatApp("github", "engineer", "installed"),
      )(fairsplit()),
    );
    rest("POST", "/setup/integrations/github/check");
    expect(reconcile()).toMatchObject({ actor: "admin", attempts: 4 });
    // From waiting on an administrator to waiting on the operator (`Observe`).
    engine.company.secrets = engine.company.secrets.filter((s) => s.name !== "GITHUB_APP_KEY_PM");
    rest("POST", "/setup/integrations/github/check");
    expect(reconcile()).toMatchObject({ actor: "operator", attempts: 1 });
  });

  test("is ready once every agent's app is installed, and refuses an app whose key is gone", () => {
    const both = compose(
      connected,
      seatApp("github", "pm", "installed"),
      seatApp("github", "engineer", "installed"),
    )(fairsplit());
    engine = new DemoEngine(both);
    expect(reconcile()).toMatchObject({ phase: "ready", actor: "", detail: "", findings: [] });
    expect(card("github")?.state).toBe("connected");

    const keyless = structuredClone(both);
    keyless.secrets = keyless.secrets.filter((s) => s.name !== "GITHUB_APP_KEY_PM");
    engine = new DemoEngine(keyless);
    expect(rest("POST", "/setup/integrations/github/check").status).toBe(200);
    expect(reconcile()).toMatchObject({
      phase: "unconfigured",
      actor: "operator",
      detail:
        "the private key for pm's app did not resolve, and GitHub issues it once: delete the app at GitHub and create it again",
    });
  });

  test("reports an agent with an app block and no app twice, as the engine's pass does today", () => {
    // seatsWithoutApps counts every agent whose block has no app id, and
    // ReconcileSeatApps walks every agent with a block, so the engine names
    // this agent in both. Reported to be fixed in the engine.
    engine = new DemoEngine(
      compose(
        connected,
        seatApp("github", "pm", "installed"),
        seatApp("github", "engineer", "create_app"),
      )(fairsplit()),
    );
    expect(reconcile().detail).toBe(
      "engineer has no GitHub App of its own, so nothing it does on GitHub is its own (and 1 more)",
    );
    expect(reconcile().findings?.map((f) => f.subject)).toEqual([
      "agents without an app",
      "engineer",
    ]);
  });

  test("hears what a scenario scripted instead, until a person writes its inputs", () => {
    engine = new DemoEngine(
      compose(connected, integrationStatus("github", { phase: "ready" }))(fairsplit()),
    );
    expect(rest("POST", "/setup/integrations/github/check").status).toBe(200);
    expect(reconcile()).toMatchObject({ phase: "ready", findings: [] });
    expect(
      rest("POST", "/setup/integrations/github/inputs", {
        values: { "provisioning.org": "fairsplit-labs" },
      }).status,
    ).toBe(201);
    expect(reconcile()).toMatchObject({
      phase: "awaiting_admin",
      detail: "2 agents have no GitHub App of their own, so nothing they do on GitHub is theirs",
    });
  });
});

describe("a write to a surface's inputs", () => {
  const DATADOG = {
    site: "datadoghq.com",
    api_key: "demo-datadog-api-key",
    app_key: "demo-datadog-application-key",
    enabled: "true",
  };
  const refused = compose(
    connectIntegration("datadog", [{ values: DATADOG, generate: ["webhook_token"] }]),
    integrationStatus("datadog", {
      phase: "unconfigured",
      actor: "operator",
      detail: "the third-party app refused this integration's credential",
      last_error: "datadog: GET /api/v1/validate: 403 Forbidden",
      findings: [{ kind: "credential_rejected" }],
    }),
  );

  test("asks the tool afresh, so a rotated key that was refused reads Connected", () => {
    engine = new DemoEngine(refused(fairsplit()));
    expect(card("datadog")?.state).toBe("attention");
    // Every pass hears the refusal again, as a tool refusing a key does.
    rest("POST", "/setup/integrations/datadog/check");
    expect(row("datadog").reconcile).toMatchObject({
      phase: "unconfigured",
      attempts: 2,
      last_error: "datadog: GET /api/v1/validate: 403 Forbidden",
    });
    // A refused write changes nothing at the tool.
    expect(
      rest("POST", "/setup/integrations/datadog/inputs", { values: { colour: "x" } }).status,
    ).toBe(500);
    expect(row("datadog").reconcile?.phase).toBe("unconfigured");

    const rotated = rest("POST", "/setup/integrations/datadog/inputs", {
      values: { api_key: "a-rotated-datadog-key" },
    });
    expect(rotated.status).toBe(201);
    expect((rotated.body as { reloaded: boolean }).reloaded).toBe(true);
    expect(row("datadog").reconcile).toMatchObject({
      phase: "ready",
      attempts: 0,
      last_error: "",
      findings: [],
    });
    expect(card("datadog")).toMatchObject({ state: "connected", label: "Connected" });
  });

  test("leaves another surface's script alone", () => {
    engine = new DemoEngine(
      compose(refused, connectIntegration("mattermost", [{ values: MATTERMOST }]))(fairsplit()),
    );
    rest("POST", "/setup/integrations/mattermost/inputs", { values: { team: "fairsplit-2" } });
    expect(row("datadog").reconcile?.phase).toBe("unconfigured");
  });
});

describe("traffic", () => {
  test("is counted from the delivery rows, which the tool's deliveries list reads", () => {
    engine = new DemoEngine(
      compose(
        connectIntegration("github", [{ values: GITHUB, generate: ["webhook_secret"] }]),
        integrationTraffic("github", {
          inbound: 12,
          skipped: 2,
          coalesced: 1,
          deliveries: [
            {
              type: "push",
              summary: "Pushed 3 commits to main",
              recipient: "pm",
              minutesAgo: 5,
              delivery_key: "d-1",
            },
            { type: "pull_request", summary: "Opened #12", minutesAgo: 40 },
          ],
        }),
      )(fairsplit()),
    );
    expect(row("github")).toMatchObject({ inbound: 12, skipped: 2, coalesced: 1 });
    expect(Date.parse(row("github").last_at as string)).toBe(
      Date.parse(engine.company.now) - 5 * 60_000,
    );
    const rows = engine.answer("events", {
      category: "webhook",
      source: "github",
      limit: 50,
    }).events;
    expect(rows).toHaveLength(12);
    expect(rows[0]).toMatchObject({
      type: "webhook:push",
      summary: "Pushed 3 commits to main",
      tags: { recipient: "pm", delivery_key: "d-1", route: "github" },
    });
  });
});

describe("the roll-up", () => {
  test("reads an expiring credential on a ready surface as needing attention", () => {
    engine = new DemoEngine(
      compose(
        connectIntegration("mattermost", [{ values: MATTERMOST }]),
        integrationStatus("mattermost", {
          phase: "ready",
          findings: [
            {
              kind: "credential_expiring",
              detail: "The administrator token expires on 2026-10-20.",
            },
          ],
        }),
      )(fairsplit()),
    );
    expect(card("mattermost")).toMatchObject({
      state: "attention",
      label: "Credential expiring",
      reason: "The administrator token expires on 2026-10-20.",
    });
  });
});

describe("verify, a proposal", () => {
  test("answers both ways by a rule a walk can drive", () => {
    engine = new DemoEngine(fairsplit());
    const verify = (body: unknown, kind = "mattermost") =>
      rest("POST", `/setup/integrations/${kind}/verify`, body);
    expect(verify({ field: "admin_token", value: "${ANTHROPIC_API_KEY}" }).body).toEqual({
      ok: true,
      detail: "Works.",
    });
    expect(verify({ field: "admin_token", value: "${NOPE}" }).body).toEqual({
      ok: false,
      detail: "There is no secret NOPE.",
    });
    expect(verify({ field: "admin_token", value: "a-bad-token-1" }).body).toEqual({
      ok: false,
      detail: "Mattermost refused this administrator token.",
    });
    expect(verify({ field: "admin_token", value: "short" }).body).toMatchObject({ ok: false });
    expect(verify({ field: "admin_token", value: "a-good-token" }).body).toEqual({
      ok: true,
      detail: "Works. Mattermost accepted it.",
    });
    expect(
      verify({ seat: "pm", field: "bot_token", value: "xoxb-a-good-one" }, "slack").body,
    ).toEqual({
      ok: true,
      detail: "Works. Slack accepted it.",
    });
    expect(verify({ field: "colour", value: "x" })).toMatchObject({
      status: 400,
      body: { error: "invalid_input" },
    });
    expect(verify({ field: "x", value: "y" }, "teams")).toMatchObject({
      status: 404,
      body: { error: "unknown_kind" },
    });
    expect(warned).toEqual([]);
  });
});
