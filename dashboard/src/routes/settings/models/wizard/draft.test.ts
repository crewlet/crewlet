/**
 * What the wizard's draft writes: the entry, the order, the uses, and where
 * each problem is said.
 *
 * The invariants: a pasted value never lands in the document; an edit keeps
 * every field it does not hold; a new model goes last in the company's order
 * whatever its name; and every problem is said once, in words, on the step
 * that fixes it.
 */

import { describe, expect, test } from "vitest";
import { noUses, usesForIntent } from "~/lib/modelUses.ts";
import type { CompanyDocument, ConfigProblem } from "~/protocol/types.ts";
import type { ModelView } from "../data.ts";
import {
  baseOf,
  distinctProblems,
  draftOf,
  draftProblems,
  emptyDraft,
  entityOf,
  messageOf,
  nextDocument,
  stepOf,
  suggestedVariable,
  type Draft,
} from "./draft.ts";

const draft = (patch: Partial<Draft> = {}): Draft => ({
  ...emptyDraft(),
  key: "sonnet-key",
  model: "sonnet",
  variable: "ANTHROPIC_API_KEY_SONNET_KEY",
  credential: "sk-ant-api03-secret",
  ...patch,
});

function view(patch: Partial<ModelView> & { raw: Record<string, unknown> }): ModelView {
  return {
    key: "sonnet",
    agent: "claude-code",
    model: "sonnet",
    executor: true,
    entry: {
      key: "sonnet",
      agent: "claude-code",
      model: "sonnet",
      health: "ready",
      sign_in: { route: "api-key", resolves: true },
      sitting_out: null,
      last_success: null,
      waiting: null,
      used_by: [],
    },
    admin: null,
    uses: noUses(),
    runIn: null,
    timeoutSeconds: null,
    maxConcurrent: null,
    webDenied: false,
    ...patch,
  };
}

describe("the entry", () => {
  test("names the secret and never carries the pasted value", () => {
    const entity = entityOf(draft());
    expect(entity.auth).toEqual({ mode: "api-key", api_key: "${ANTHROPIC_API_KEY_SONNET_KEY}" });
    expect(JSON.stringify(entity)).not.toContain("sk-ant-api03-secret");
  });

  test("an edit keeps the fields the wizard does not hold, and its other overrides", () => {
    const raw = {
      agent: "claude-code",
      model: "sonnet",
      state_dir: "/var/crewlet/sonnet",
      overrides: { web_tools: "denied", api_key_env: "MY_KEY" },
      env: { HTTPS_PROXY: "http://proxy.example.com" },
      auth: { mode: "api-key", api_key: "${OLD}", credential_bundle: "${BUNDLE}" },
    };
    const model = view({ raw });
    const d = { ...draftOf(model), webDenied: false, timeout: "120" };
    const entity = entityOf(d, baseOf(model));
    expect(entity.state_dir).toBe("/var/crewlet/sonnet");
    expect(entity.overrides).toEqual({ api_key_env: "MY_KEY" });
    expect(entity.env).toEqual({ HTTPS_PROXY: "http://proxy.example.com" });
    expect(entity.timeout_seconds).toBe(120);
    // The same method keeps its bundle; the secret it reads stays the one written.
    expect(entity.auth).toEqual({
      mode: "api-key",
      api_key: "${OLD}",
      credential_bundle: "${BUNDLE}",
    });
  });

  test("a cloud provider signs in through env, its secret by reference", () => {
    const entity = entityOf(
      draft({
        method: "cloud",
        cloud: "bedrock",
        cloudFields: { AWS_REGION: "us-west-2", AWS_BEARER_TOKEN_BEDROCK: "ABSK-secret" },
      }),
    );
    expect(entity.auth).toEqual({ mode: "env" });
    expect(entity.env).toEqual({
      CLAUDE_CODE_USE_BEDROCK: "1",
      AWS_REGION: "us-west-2",
      AWS_BEARER_TOKEN_BEDROCK: "${AWS_BEARER_TOKEN_BEDROCK_SONNET_KEY}",
    });
  });

  test("OpenCode reads its provider's key from env and names its provider in the model", () => {
    const entity = entityOf(
      draft({ agent: "opencode", provider: "openrouter", model: "qwen-coder", variable: "OR_KEY" }),
    );
    expect(entity.model).toBe("openrouter/qwen-coder");
    expect(entity.auth).toBeUndefined();
    expect(entity.env).toEqual({ OPENROUTER_API_KEY: "${OR_KEY}" });
  });

  test("an edit reads the method and the provider back from what is written", () => {
    const model = view({
      key: "open",
      agent: "opencode",
      model: "openrouter/qwen-coder",
      raw: {
        agent: "opencode",
        model: "openrouter/qwen-coder",
        env: { OPENROUTER_API_KEY: "${OR_KEY}" },
      },
    });
    const d = draftOf(model);
    expect(d.method).toBe("api-key");
    expect(d.provider).toBe("openrouter");
    expect(d.model).toBe("qwen-coder");
    expect(d.variable).toBe("OR_KEY");
  });
});

describe("the secret's name", () => {
  test("is the CLI's own variable while no model reads it", () => {
    expect(suggestedVariable(draft(), [])).toBe("ANTHROPIC_API_KEY");
  });

  test("takes the model's name when it is taken, without saying the vendor twice", () => {
    expect(suggestedVariable(draft({ key: "anthropic-opus" }), ["ANTHROPIC_API_KEY"])).toBe(
      "ANTHROPIC_API_KEY_OPUS",
    );
    expect(suggestedVariable(draft({ key: "backup" }), ["ANTHROPIC_API_KEY"])).toBe(
      "ANTHROPIC_API_KEY_BACKUP",
    );
  });
});

describe("the next document", () => {
  const doc = {
    providers: { llm: { default: { agent: "claude-code" }, zeta: { agent: "codex" } } },
    roles: [
      { handle: "qa", name: "QA", llm_review: ["default", "zeta"] },
      { handle: "pm", name: "PM" },
    ],
  } as unknown as CompanyDocument;

  test("a new model goes last in the company's order, whatever its name sorts as", () => {
    const next = nextDocument(doc, draft({ key: "aaa" })).doc as Record<string, any>;
    expect(next.providers.llm_order).toEqual(["default", "zeta", "aaa"]);
    expect(next.providers.llm.aaa.model).toBe("sonnet");
  });

  test("a use is written by applyUses, and the fallback it drops is returned", () => {
    const uses = usesForIntent("reviews", ["qa"], false);
    const next = nextDocument(doc, draft({ uses, intent: "reviews" }));
    const qa = (next.doc as Record<string, any>).roles[0];
    expect(qa.llm_review).toBe("sonnet-key");
    expect(next.dropped).toContainEqual({ seat: "qa", job: "review", keys: ["default", "zeta"] });
  });
});

describe("problems", () => {
  test("each step's own problems, with the credential missing unless it is kept", () => {
    const d = draft({ key: "Backup Key", credential: "", model: "", timeout: "0" });
    const steps = draftProblems(d, []).map((p) => stepOf(p, d.key));
    expect(steps).toEqual(["cli", "signin", "model", "limits"]);
    const kept = draftProblems(draft({ credential: "" }), [], { credential: true, cloud: {} });
    expect(kept).toEqual([]);
  });

  test("a name past the wizard's cap, or one already used, is refused on the first step", () => {
    expect(draftProblems(draft({ key: "a".repeat(41) }), [])[0]?.kind).toBe("out_of_range");
    expect(draftProblems(draft(), ["sonnet-key"])[0]?.message).toBe(
      "There is already a model called sonnet-key.",
    );
  });

  test("the engine's path is not read out, and its problem on a field the form flags is said once", () => {
    const engine: ConfigProblem = {
      path: "providers.llm.sonnet-key.model",
      segments: ["providers", "llm", "sonnet-key", "model"],
      kind: "missing",
      message: "providers.llm.sonnet-key.model: name the model the CLI runs",
    };
    expect(messageOf(engine)).toBe("Name the model the CLI runs");
    const local = draftProblems(draft({ model: "" }), []);
    const once = distinctProblems(local, [engine, engine], "sonnet-key");
    expect(once).toHaveLength(1);
    expect(distinctProblems([], [engine, engine], "sonnet-key")).toHaveLength(1);
  });

  test("a tested value's refusal is the Method's, a seat's is Use it for", () => {
    expect(stepOf({ segments: ["values", "ANTHROPIC_API_KEY"] }, "k")).toBe("signin");
    expect(stepOf({ segments: ["roles", 0, "llm"] }, "k")).toBe("assign");
    expect(stepOf({ segments: ["providers", "llm", "k", "run_in"] }, "k")).toBe("runs");
  });
});
