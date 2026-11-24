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
import { HARNESS_FACTS } from "~/contract/harnesses.ts";
import {
  HARNESSES,
  baseOf,
  catalogueModel,
  credentialValues,
  effortLevels,
  factsOf,
  methodOf,
  methodsOf,
  providersFor,
  distinctProblems,
  draftOf,
  draftProblems,
  emptyDraft,
  entityOf,
  keptOf,
  messageOf,
  modelId,
  nextDocument,
  stepOf,
  stepsFor,
  suggestedVariable,
  withChoices,
  type Draft,
} from "./draft.ts";
import { credentialExample, modelExample } from "./examples.ts";
import { EXECUTORS } from "../fixtures.ts";
import { droppedWords } from "./steps/Review.tsx";
import { fixable, hasSandbox } from "./draft.ts";

const draft = (patch: Partial<Draft> = {}): Draft => ({
  ...emptyDraft(EXECUTORS),
  key: "sonnet-key",
  model: "sonnet",
  variable: "ANTHROPIC_API_KEY_SONNET_KEY",
  credential: "sk-ant-api03-secret",
  ...patch,
});

function view(patch: Partial<ModelView> & { raw: Record<string, unknown> }): ModelView {
  return {
    key: "sonnet",
    harness: "claude-code",
    model: "sonnet",
    modelName: "sonnet",
    executor: true,
    entry: {
      key: "sonnet",
      harness: "claude-code",
      model: "sonnet",
      health: "ready",
      sign_in: { route: "api-key", resolves: true },
      sitting_out: null,
      last_success: null,
      waiting: null,
      used_by: [],
      limits: [],
    },
    admin: null,
    uses: noUses(),
    runIn: null,
    timeoutSeconds: null,
    maxConcurrent: null,
    effort: null,
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
      harness: "claude-code",
      model: "sonnet",
      state_dir: "/var/crewlet/sonnet",
      overrides: { web_tools: "denied", api_key_env: "MY_KEY" },
      env: { HTTPS_PROXY: "http://proxy.example.com" },
      auth: { mode: "api-key", api_key: "${OLD}", credential_bundle: "${BUNDLE}" },
    };
    const model = view({ raw });
    const d = { ...draftOf(model, EXECUTORS), webDenied: false, timeout: "120" };
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

  test("Gemini CLI's cloud is Vertex AI, switched on as Gemini CLI reads it, its key by reference", () => {
    const entity = entityOf(
      draft({
        harness: "gemini-cli",
        method: "cloud",
        cloud: "gemini-vertex",
        cloudFields: {
          GOOGLE_CLOUD_PROJECT: "acme",
          GOOGLE_CLOUD_LOCATION: "us-central1",
          GOOGLE_API_KEY: "AIza-secret",
        },
      }),
    );
    expect(entity.auth).toEqual({ mode: "env" });
    expect(entity.env).toEqual({
      GOOGLE_GENAI_USE_VERTEXAI: "true",
      GOOGLE_CLOUD_PROJECT: "acme",
      GOOGLE_CLOUD_LOCATION: "us-central1",
      GOOGLE_API_KEY: "${GOOGLE_API_KEY_SONNET_KEY}",
    });
    // READ BACK AS ITS OWN CLOUD, never Claude Code's gateway.
    const raw = {
      harness: "gemini-cli",
      model: "gemini-2.5-pro",
      auth: entity.auth,
      env: entity.env,
    };
    const back = draftOf(view({ harness: "gemini-cli", model: "gemini-2.5-pro", raw }), EXECUTORS);
    expect([back.method, back.cloud]).toEqual(["cloud", "gemini-vertex"]);
  });

  test("a harness's clouds are its own: moving to Gemini CLI leaves Claude Code's behind", () => {
    const fresh = { original: null, taken: [], named: false };
    const bedrock = draft({
      method: "cloud",
      cloud: "bedrock",
      cloudFields: { AWS_REGION: "us-west-2" },
    });
    const gemini = withChoices(bedrock, { harness: "gemini-cli" }, fresh).draft;
    expect([gemini.cloud, gemini.cloudFields]).toEqual(["gemini-vertex", {}]);
    // AN EDIT ON ANOTHER METHOD that turns to its cloud takes Gemini CLI's too.
    const keyed = draftOf(
      view({ harness: "gemini-cli", model: "gemini-2.5-pro", raw: { harness: "gemini-cli" } }),
      EXECUTORS,
    );
    expect(withChoices(keyed, { method: "cloud" }, fresh).draft.cloud).toBe("gemini-vertex");
  });

  test("OpenCode reads its provider's key from env and names its provider in the model", () => {
    const entity = entityOf(
      draft({
        harness: "opencode",
        provider: "openrouter",
        model: "qwen-coder",
        variable: "OR_KEY",
      }),
    );
    expect(entity.model).toBe("openrouter/qwen-coder");
    expect(entity.auth).toBeUndefined();
    expect(entity.env).toEqual({ OPENROUTER_API_KEY: "${OR_KEY}" });
  });

  test("an edit reads the method and the provider back from what is written", () => {
    const model = view({
      key: "open",
      harness: "opencode",
      model: "openrouter/qwen-coder",
      raw: {
        harness: "opencode",
        model: "openrouter/qwen-coder",
        env: { OPENROUTER_API_KEY: "${OR_KEY}" },
      },
    });
    const d = draftOf(model, EXECUTORS);
    expect(d.method).toBe("api-key");
    expect(d.provider).toBe("openrouter");
    expect(d.model).toBe("qwen-coder");
    expect(d.variable).toBe("OR_KEY");
  });
});

/**
 * A CHOICE TAKES WHAT BELONGS TO IT: a model and its effort are named by the
 * vendor the harness, provider or cloud sends them to, and a pasted credential
 * and its secret's name belong to the provider they sign in to. A changed
 * choice never carries one vendor's value to another; an edit back on its own
 * choice gets the model's values back.
 */
describe("a changed choice", () => {
  const adding = { original: null, taken: ["ANTHROPIC_API_KEY"], named: false };
  const onAnthropic = draft({
    key: "oc",
    harness: "opencode",
    provider: "anthropic",
    model: "claude-sonnet-5-5",
    effort: "high",
    variable: "ANTHROPIC_API_KEY_OC",
  });

  test("of provider clears the model, its effort and the key typed for the last one", () => {
    const { draft: next, named } = withChoices(onAnthropic, { provider: "openai" }, adding);
    expect(next.model).toBe("");
    expect(next.effort).toBe("");
    expect(next.credential).toBe("");
    expect(next.variable).toBe("OPENAI_API_KEY");
    expect(named).toBe(false);
    expect(modelId({ ...next, model: "gpt-5.5" })).toBe("openai/gpt-5.5");
  });

  test("of provider replaces a secret name typed for the last provider, and keeps one typed after", () => {
    const typed = withChoices(onAnthropic, { variable: "MY_KEY" }, adding);
    expect(typed.named).toBe(true);
    const moved = withChoices(typed.draft, { provider: "openai" }, { ...adding, named: true });
    expect(moved.draft.variable).toBe("OPENAI_API_KEY");
    const kept = withChoices(moved.draft, { variable: "MY_OPENAI_KEY" }, adding);
    expect(
      withChoices(kept.draft, { key: "renamed" }, { ...adding, named: kept.named }).draft.variable,
    ).toBe("MY_OPENAI_KEY");
  });

  test("of harness or of cloud clears the model; another way into the same vendor keeps it", () => {
    const claude = draft({ model: "claude-opus-5-5", effort: "max" });
    expect(withChoices(claude, { harness: "codex" }, adding).draft.model).toBe("");
    expect(withChoices(claude, { method: "cloud" }, adding).draft.model).toBe("");
    const bedrock = { ...claude, method: "cloud" as const, cloud: "bedrock" as const };
    expect(withChoices(bedrock, { cloud: "vertex" }, adding).draft.model).toBe("");
    const token = withChoices(claude, { method: "token" }, adding).draft;
    expect([token.model, token.effort]).toEqual(["claude-opus-5-5", "max"]);
    expect(token.credential).toBe("");
    expect(token.variable).toBe("CLAUDE_CODE_OAUTH_TOKEN");
  });

  test("on an edit keeps the stored key only for its own provider, and gives it back there", () => {
    const original = { ...onAnthropic, credential: "", variable: "ANTHROPIC_API_KEY" };
    const editing = { original, taken: [], named: true };
    expect(keptOf(original, original, undefined, false).credential).toBe(true);
    const away = withChoices(original, { provider: "openai" }, editing);
    expect(away.draft.variable).toBe("OPENAI_API_KEY");
    expect(away.draft.model).toBe("");
    expect(keptOf(away.draft, original, undefined, false).credential).toBe(false);
    expect(entityOf({ ...away.draft, model: "gpt-5.5" }).env).toEqual({
      OPENAI_API_KEY: "${OPENAI_API_KEY}",
    });
    const back = withChoices(
      away.draft,
      { provider: "anthropic" },
      { ...editing, named: away.named },
    );
    expect([back.draft.variable, back.draft.model, back.draft.effort]).toEqual([
      "ANTHROPIC_API_KEY",
      "claude-sonnet-5-5",
      "high",
    ]);
    expect(keptOf(back.draft, original, undefined, false).credential).toBe(true);
  });

  test("of harness back to the edited model's gives its uses and place to run back", () => {
    const original = draft({
      intent: "assistant",
      uses: { ...noUses(), assistant: true, advisor: true },
      runIn: "direct",
    });
    const editing = { original, taken: [], named: true };
    const away = withChoices(original, { harness: "codex" }, editing).draft;
    expect([away.uses.assistant, away.runIn]).toEqual([false, ""]);
    const back = withChoices(away, { harness: "claude-code" }, editing).draft;
    expect([back.uses, back.intent, back.runIn]).toEqual([original.uses, "assistant", "direct"]);
  });

  test("of harness drops the uses the new harness cannot run", () => {
    const helper = draft({
      intent: "assistant",
      uses: { ...noUses(), assistant: true, advisor: true },
    });
    const codex = withChoices(helper, { harness: "codex" }, adding).draft;
    expect([codex.uses.assistant, codex.uses.advisor, codex.intent]).toEqual([
      false,
      false,
      "none",
    ]);
    const coder = draft({
      intent: "all",
      uses: usesForIntent("all", ["pm"], false),
      runIn: "direct",
    });
    const gemini = withChoices(coder, { harness: "gemini-cli" }, adding).draft;
    expect([gemini.intent, gemini.runIn, Object.values(gemini.uses.jobs).flat()]).toEqual([
      "none",
      "",
      [],
    ]);
  });
});

// WHICH HARNESS RUNS CODING IS THE ENGINE'S ANSWER (`executor_harnesses`), never
// a list compiled into the page: a runner a build adds is offered with no
// dashboard rebuild. Mutation: read a compiled list and pi stays host-only.
describe("the harnesses that run coding", () => {
  test("are the ones the engine serves, whatever the page was built with", () => {
    const pi = { ...draft({ harness: "pi" }) };
    expect(stepsFor(pi)).not.toContain("runs");
    const served = { ...pi, executors: [...EXECUTORS, "pi"] };
    expect(stepsFor(served)).toContain("runs");
    expect(stepsFor({ ...draft(), executors: [] })).not.toContain("runs");
  });
});

// WHO RUNS ON A MODEL IS CHOSEN UNDER WHO RUNS ON WHICH MODEL, never in the
// wizard. Mutation: put "assign" back in WIZARD_STEPS and it is offered again.
test("no harness's wizard asks what the model is used for", () => {
  for (const harness of [...EXECUTORS, "pi", "gemini-cli"])
    expect(stepsFor(draft({ harness }))).toEqual(expect.not.arrayContaining(["assign" as never]));
  expect(stepsFor(draft())).toEqual(["harness", "signin", "model", "runs", "limits", "review"]);
});

describe("a model id on a harness that names its provider", () => {
  test("always names the chosen provider before the provider's own id, a slash or its name included", () => {
    const oc = { harness: "opencode", provider: "openrouter" };
    expect(modelId({ ...oc, model: "qwen/qwen3-coder" })).toBe("openrouter/qwen/qwen3-coder");
    expect(modelId({ ...oc, model: "openrouter/auto" })).toBe("openrouter/openrouter/auto");
    expect(modelId({ ...oc, provider: "groq", model: "openai/gpt-oss-120b" })).toBe(
      "groq/openai/gpt-oss-120b",
    );
  });

  test("a catalogue id is held as the provider's own id", () => {
    const oc = { harness: "opencode", provider: "openrouter" };
    expect(catalogueModel(oc, "openrouter/qwen/qwen3-coder")).toBe("qwen/qwen3-coder");
    expect(catalogueModel(oc, "openrouter/openrouter/auto")).toBe("openrouter/auto");
    expect(
      catalogueModel({ harness: "claude-code", provider: "anthropic" }, "claude-opus-5-5"),
    ).toBe("claude-opus-5-5");
  });

  test.each(["openrouter/qwen/qwen3-coder", "openrouter/openrouter/auto", "groq/groq/compound"])(
    "an edit of %s saved as it opened writes the model it read",
    (id) => {
      const provider = id.split("/")[0]!;
      const variable = provider === "groq" ? "GROQ_API_KEY" : "OPENROUTER_API_KEY";
      const raw = { harness: "opencode", model: id, env: { [variable]: "${KEY}" } };
      const model = view({ key: "open", harness: "opencode", model: id, raw });
      expect(entityOf(draftOf(model, EXECUTORS), baseOf(model)).model).toBe(id);
    },
  );
});

/** ONE TABLE FOR EVERY EXAMPLE: the placeholder, the key's prefix and the hint name one vendor. */
describe("the examples", () => {
  const ex = (patch: Partial<Draft>) => {
    const d = { ...emptyDraft(EXECUTORS), ...patch };
    return [modelExample(d).placeholder, modelExample(d).naming, credentialExample(d)];
  };

  test.each([
    [{ harness: "opencode", provider: "openai" }, "gpt-5.5", "Sent as openai/<model>.", "sk-…"],
    [
      { harness: "opencode", provider: "anthropic" },
      "claude-sonnet-5-5",
      "Sent as anthropic/<model>.",
      "sk-ant-api03-…",
    ],
    [
      { harness: "opencode", provider: "openrouter" },
      "qwen/qwen3-coder",
      "Sent as openrouter/<model>.",
      "sk-or-v1-…",
    ],
    [
      { harness: "opencode", provider: "groq" },
      "The model's id at Groq",
      "Sent as groq/<model>.",
      "gsk_…",
    ],
    [
      { harness: "claude-code" },
      "claude-sonnet-5-5",
      "In the harness's own naming.",
      "sk-ant-api03-…",
    ],
    [
      { harness: "claude-code", method: "token" as const },
      "claude-sonnet-5-5",
      "In the harness's own naming.",
      "sk-ant-oat01-…",
    ],
    [
      { harness: "claude-code", method: "cloud" as const, cloud: "vertex" as const },
      "The model's id at Google Vertex AI",
      "In the harness's own naming.",
      "Paste the key",
    ],
    [{ harness: "codex" }, "gpt-5.5", "In the harness's own naming.", "sk-…"],
    [
      { harness: "cursor-agent" },
      "The model's id",
      "In the harness's own naming.",
      "Paste the key",
    ],
  ])("follow %o", (patch, placeholder, naming, key) => {
    expect(ex(patch)).toEqual([placeholder, naming, key]);
  });
});

describe("the secret's name", () => {
  test("is the harness's own variable while no model reads it", () => {
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
    providers: { llm: { default: { harness: "claude-code" }, zeta: { harness: "codex" } } },
    roles: [
      { handle: "qa", name: "QA", llm_review: ["default", "zeta"] },
      { handle: "pm", name: "PM" },
    ],
  } as unknown as CompanyDocument;

  // THE FLEETS ARE NOT THE WIZARD'S: a model added or edited leaves every fleet as written.
  test("a document's fleets are kept whole through a model's write", () => {
    const fleets = {
      "claude-opus": { name: "Claude Opus", strategy: "sequential", members: ["default", "zeta"] },
    };
    const withFleets = {
      ...doc,
      providers: { ...(doc.providers as object), llm_fleets: fleets },
    } as unknown as CompanyDocument;
    const next = nextDocument(withFleets, draft({ key: "aaa" })).doc as Record<string, any>;
    expect(next.providers.llm_fleets).toEqual(fleets);
  });

  // A MODEL JOINS THE DEFAULT FLEET in the revision that adds it, written out
  // where the engine only made it; a first model's seats and helpers go there too.
  test("joining writes the default fleet with the model, and moves only the seats and helpers asked", () => {
    const join = { seats: ["qa", "pm"], assistant: true, advisor: false };
    const next = nextDocument(doc, draft({ key: "aaa" }), undefined, join).doc as Record<
      string,
      any
    >;
    expect(next.providers.llm_fleets).toEqual({
      "default-fleet": { name: "default-fleet", strategy: "balanced", members: ["aaa"] },
    });
    // QA's review on two models is on no fleet: every job moves, the review with it.
    expect(next.roles).toEqual([
      { handle: "qa", name: "QA", llm: "fleet:default-fleet" },
      { handle: "pm", name: "PM", llm: "fleet:default-fleet" },
    ]);
    expect(next.assistant).toEqual({ model: "fleet:default-fleet" });
    expect(next.advisor).toBeUndefined();
  });

  test("joining keeps the default fleet's members and settings, and moves nobody unless asked", () => {
    const written = {
      ...doc,
      providers: {
        ...(doc.providers as object),
        llm_fleets: {
          "default-fleet": { name: "default-fleet", strategy: "sequential", members: ["zeta"] },
        },
      },
    } as unknown as CompanyDocument;
    const join = { seats: [], assistant: false, advisor: false };
    const next = nextDocument(written, draft({ key: "aaa" }), undefined, join).doc as Record<
      string,
      any
    >;
    expect(next.providers.llm_fleets["default-fleet"]).toEqual({
      name: "default-fleet",
      strategy: "sequential",
      members: ["zeta", "aaa"],
    });
    expect(next.roles).toEqual((doc as Record<string, any>).roles);
  });

  test("a helper already on a fleet keeps it when a first model joins", () => {
    const onFleet = { ...doc, assistant: { model: "fleet:codex" } } as unknown as CompanyDocument;
    const join = { seats: [], assistant: true, advisor: true };
    const next = nextDocument(onFleet, draft({ key: "aaa" }), undefined, join).doc as Record<
      string,
      any
    >;
    expect(next.assistant).toEqual({ model: "fleet:codex" });
    expect(next.advisor).toEqual({ model: "fleet:default-fleet" });
  });

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
    expect(next.dropped).toContainEqual({ seat: "qa", job: "review", keys: ["zeta"] });
    // ONLY THE FALLBACK IS SAID AS ONE: `default`, which QA reviewed on first, is the change.
    expect(droppedWords({ seat: "qa", job: "review", keys: ["zeta"] }, () => "QA")).toBe(
      "QA's reviewing also named zeta as a fallback; this removes it.",
    );
  });
});

describe("problems", () => {
  test("each step's own problems, with the credential missing unless it is kept", () => {
    const d = draft({ key: "Backup Key", credential: "", model: "", timeout: "0" });
    const steps = draftProblems(d, []).map((p) => stepOf(p, d.key));
    expect(steps).toEqual(["harness", "signin", "model", "limits"]);
    const kept = draftProblems(draft({ credential: "" }), [], { credential: true, cloud: {} });
    expect(kept).toEqual([]);
  });

  test("a name past the wizard's cap, or one already used, is refused on the first step", () => {
    expect(draftProblems(draft({ key: "a".repeat(41) }), [])[0]?.kind).toBe("out_of_range");
    expect(draftProblems(draft(), ["sonnet-key"])[0]?.message).toBe(
      "There is already a model called sonnet-key.",
    );
  });

  // THE ENGINE'S GRAMMARS, held as `MODEL_KEY_PATTERN` and `REFERENCE_NAME`:
  // a model's name as `config.ProviderKeyPattern` takes one, and its secret's
  // as the secret store does, which takes lowercase and a leading underscore.
  test("a model's name and its secret's are held to the engine's grammars, and no narrower", () => {
    const named = (key: string) =>
      draftProblems(draft({ key }), []).filter((p) => p.path === "key");
    for (const key of ["sonnet-key", "9b", "gpt-5.1_mini"]) expect(named(key), key).toEqual([]);
    for (const key of ["Sonnet", "-sonnet", ".hidden", "son net", "son/net"]) {
      expect(named(key)[0]?.message, key).toBe(
        "Lowercase letters, digits, dots, hyphens and underscores, up to 40 characters. It starts with a letter or a digit.",
      );
    }
    const secret = (variable: string) =>
      draftProblems(draft({ variable }), []).filter((p) => p.path === "variable");
    for (const variable of ["ANTHROPIC_API_KEY", "anthropic_key", "_KEY2"]) {
      expect(secret(variable), variable).toEqual([]);
    }
    for (const variable of ["2KEY", "MY-KEY", "MY KEY", ""]) {
      expect(secret(variable)[0]?.message, variable).toBe(
        "Name the secret with letters, digits and underscores, starting with a letter or an underscore.",
      );
    }
  });

  test("the engine's path is not read out, and its problem on a field the form flags is said once", () => {
    const engine: ConfigProblem = {
      path: "providers.llm.sonnet-key.model",
      segments: ["providers", "llm", "sonnet-key", "model"],
      kind: "missing",
      message: "providers.llm.sonnet-key.model: name the model the harness runs",
    };
    expect(messageOf(engine)).toBe("Name the model the harness runs");
    const local = draftProblems(draft({ model: "" }), []);
    const once = distinctProblems(local, [engine, engine], "sonnet-key");
    expect(once).toHaveLength(1);
    expect(distinctProblems([], [engine, engine], "sonnet-key")).toHaveLength(1);
  });

  test("a tested value's refusal is the Method's, a seat's is Review's, web tools are Limits'", () => {
    expect(stepOf({ segments: ["values", "ANTHROPIC_API_KEY"] }, "k")).toBe("signin");
    expect(stepOf({ segments: ["roles", 0, "llm"] }, "k")).toBe("review");
    expect(stepOf({ segments: ["assistant", "model"] }, "k")).toBe("review");
    expect(stepOf({ segments: ["providers", "llm", "k", "overrides"] }, "k")).toBe("limits");
    expect(stepOf({ segments: ["providers", "llm", "k", "run_in"] }, "k")).toBe("runs");
  });
});

describe("a pasted credential", () => {
  // A terminal that wraps `claude setup-token` output turns the break into
  // spaces when it is copied, and Claude Code then ignores the whole token.
  test("is sent without the whitespace and invisible characters a copy adds", () => {
    expect(credentialValues(draft({ credential: " sk-ant-api03-sec  ret\n\u200b-AA\t " }))).toEqual(
      { ANTHROPIC_API_KEY_SONNET_KEY: "sk-ant-api03-secret-AA" },
    );
  });

  test("of only whitespace keeps what is stored", () => {
    expect(credentialValues(draft({ credential: " \n " }))).toEqual({});
  });
});

// EVERY BUILT-IN HARNESS IS OFFERED, with the sign-ins the engine runs for it, and
// each writes the entry shape internal/config's gate holds the engine to.
describe("every harness", () => {
  test("is offered, and none the engine does not ship", () => {
    expect([...HARNESSES].sort()).toEqual(Object.keys(HARNESS_FACTS).sort());
  });

  test.each([...HARNESSES])("%s writes each of its sign-ins as the engine reads it", (harness) => {
    for (const method of methodsOf(harness)) {
      const provider = providersFor(harness)[0]?.value ?? "anthropic";
      const effort = factsOf(harness).efforts[0] ?? "";
      const d = draft({ harness, method, provider, variable: "MODEL_KEY", model: "m", effort });
      const entity = entityOf(d);
      expect(entity.harness).toBe(harness);
      expect(entity.effort).toBe(effort || undefined);
      const auth = entity.auth as Record<string, unknown> | undefined;
      const env = (entity.env ?? {}) as Record<string, string>;
      if (method === "api-key" && factsOf(harness).providerKeys) {
        expect(auth).toBeUndefined();
        expect(env[providersFor(harness)[0]!.variable]).toBe("${MODEL_KEY}");
      } else if (method === "api-key") {
        expect(auth).toEqual({ mode: "api-key", api_key: "${MODEL_KEY}" });
      } else if (method === "token") {
        expect(auth).toEqual({ mode: "subscription", token: "${MODEL_KEY}" });
      } else if (method === "cloud") {
        expect(auth).toEqual({ mode: "env" });
      } else {
        expect(auth).toEqual({ mode: "subscription" });
      }
    }
  });

  test("a provider key on Hermes reads back as a key, its provider kept", () => {
    const d = draft({
      harness: "hermes",
      method: "api-key",
      provider: "xai",
      variable: "XAI_API_KEY",
    });
    const raw = entityOf(d);
    const back = draftOf(view({ harness: "hermes", model: "m", raw }), EXECUTORS);
    expect(methodOf(view({ harness: "hermes", model: "m", raw }))).toBe("api-key");
    expect(back.provider).toBe("xai");
  });

  test("Qwen Code's endpoint is written to its variable and read back", () => {
    const d = draft({
      harness: "qwen-code",
      method: "api-key",
      variable: "QWEN_API_KEY",
      endpoint: "https://example.com/v1",
    });
    const raw = entityOf(d);
    expect(raw.env).toEqual({ OPENAI_BASE_URL: "https://example.com/v1" });
    expect(draftOf(view({ harness: "qwen-code", model: "m", raw }), EXECUTORS).endpoint).toBe(
      "https://example.com/v1",
    );
  });

  test("an effort the model does not take is a problem on the Model step", () => {
    expect(effortLevels(draft({ model: "claude-opus-4-5" }))).toEqual(["low", "medium", "high"]);
    const problems = draftProblems(draft({ model: "claude-opus-4-5", effort: "max" }), []);
    const effort = problems.find((p) => p.segments?.at(-1) === "effort");
    expect(effort && stepOf(effort, "sonnet-key")).toBe("model");
    expect(
      draftProblems(draft({ model: "opus", effort: "max" }), []).some((p) =>
        p.segments?.includes("effort"),
      ),
    ).toBe(false);
  });
});

// M-001: AN EDIT'S NAME IS NOT ASKED AGAIN. The engine takes 63 characters and
// the edit shows the name read-only, so the wizard's own cap of 40 for a new
// name refused every edit of a longer one.
test("an edit's name is never held to the cap a new name has", () => {
  const long = "claude-sonnet-for-long-running-code-reviews-and-design-docs";
  expect(long.length).toBeGreaterThan(40);
  const keyed = (editing: boolean) =>
    draftProblems(draft({ key: long }), [], undefined, editing).filter((p) => p.path === "key");
  expect(keyed(false)[0]?.kind).toBe("out_of_range");
  expect(keyed(true)).toEqual([]);
});

// M-002: A FIRST CODING MODEL GETS THIS HOST. The engine refuses any agent
// seat's executor with no providers.sandbox, and nothing on the page set one
// up; the save writes the engine's own remedy, and only where none exists.
test("a coding model on a company with no sandbox block writes this host as the place to run", () => {
  const doc = { name: "Acme", roles: [{ handle: "pm", name: "PM" }] } as never;
  expect(hasSandbox(doc)).toBe(false);
  const next = nextDocument(doc, draft()).doc as Record<string, any>;
  expect(next.providers.sandbox).toEqual({ local: {}, default_run_in: "direct" });
  // A COMPANY THAT HAS ONE keeps it as it is.
  const own = { name: "Acme", providers: { sandbox: { e2b: {}, default_run_in: "e2b" } } } as never;
  expect((nextDocument(own, draft()).doc as Record<string, any>).providers.sandbox).toEqual({
    e2b: {},
    default_run_in: "e2b",
  });
  // A HARNESS THAT RUNS NO CODING needs none.
  const helperOnly = nextDocument(doc, draft({ harness: "gemini-cli" })).doc as Record<string, any>;
  expect(helperOnly.providers.sandbox).toBeUndefined();
  // ITS REFUSAL IS WHERE IT RUNS', not Use it for.
  expect(stepOf({ segments: ["providers", "sandbox"] }, "sonnet-key")).toBe("runs");
});

// M-004: A PROGRAM MISSING ON THE ENGINE HOST is said plainly, with no step to
// fix it in: no step of the wizard installs one.
test("a harness program missing on the host is said plainly and offers no step", () => {
  const missing = {
    path: "providers.llm.sonnet-key.harness",
    segments: ["providers", "llm", "sonnet-key", "harness"],
    kind: "invalid" as const,
    message:
      'not on PATH "claude" is not on this host\'s PATH \u2014 the harness runs on the ENGINE host, so it must be installed here; set overrides.binary to an absolute path if it lives somewhere unusual',
  };
  expect(messageOf(missing)).toBe("Install claude on the engine host: the harness runs there.");
  expect(fixable(missing)).toBe(false);
  expect(fixable({ message: "Name the model." })).toBe(true);
});
