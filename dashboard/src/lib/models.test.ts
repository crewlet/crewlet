import { describe, expect, test } from "vitest";

import {
  authWords,
  credentialField,
  credentialOf,
  editSummary,
  formChanged,
  formErrors,
  formOf,
  modelEntity,
  modelEntries,
  modelEntry,
  modelLine,
  problemsByField,
  providerOrder,
  roleWords,
  signInSource,
  usesOf,
} from "./models.ts";
import type { Seat } from "./seats.ts";
import {
  DEFAULT_CLI_AGENT,
  DEFAULT_CLI_AUTH_MODE,
  DEFAULT_CLI_MAX_CONCURRENT,
  DEFAULT_CLI_TIMEOUT_SECONDS,
  EXECUTOR_CLI_AGENTS,
} from "~/contract/models.ts";
import { REDACTED } from "~/lib/format.ts";
import type { CompanyDocument } from "~/protocol/types.ts";

const seat = (name: string, llm?: Record<string, string[]>) =>
  ({
    key: name,
    name,
    handle: name.toLowerCase(),
    kind: "agent",
    raw: { name, llm },
  }) as unknown as Seat;

describe("which seats run on a model", () => {
  // THE ENGINE'S CHAIN, read: first is what the phase runs on, later a fallback.
  test("is the chain each seat resolved to, first versus later", () => {
    const uses = usesOf(
      [
        seat("CEO", { execute: ["smart", "fast"], review: ["smart"] }),
        seat("SWE", { execute: ["fast"] }),
        seat("Ana"),
      ],
      "fast",
    );
    expect(uses.map((u) => [u.seat.name, u.runs, u.fallback, u.every])).toEqual([
      ["CEO", [], ["executor"], false],
      ["SWE", ["executor"], [], true],
    ]);
  });

  // THE EXECUTOR FIRST, whatever order the projection's keys arrive in.
  test("lists phases in the engine's order, not the answer's", () => {
    const [use] = usesOf([seat("CEO", { review: ["m"], judge: ["x"], execute: ["m"] })], "m");
    expect(use?.runs).toEqual(["executor", "reviewer"]);
    expect(use?.every).toBe(false);
  });
});

describe("an entry, read off the document", () => {
  // WHAT AN UNSET FIELD MEANS IS THE ENGINE'S. An entry naming only its model
  // still drives a CLI — one that runs executors — on a sign-in, under two caps,
  // and each is said as the default the engine applies, marked as one.
  test("an entry naming only its model runs on the engine's defaults, each marked", () => {
    const entry = modelEntry("default", { model: "sonnet", auth: {} });
    expect(entry).toMatchObject({
      key: "default",
      model: "sonnet",
      agent: DEFAULT_CLI_AGENT,
      agentDefault: true,
      executor: true,
      runIn: "",
      authMode: DEFAULT_CLI_AUTH_MODE,
      authDefault: true,
      timeoutSeconds: DEFAULT_CLI_TIMEOUT_SECONDS,
      timeoutSet: false,
      maxConcurrent: DEFAULT_CLI_MAX_CONCURRENT,
      concurrencySet: false,
      token: null,
      apiKey: null,
      bundle: null,
      env: [],
    });
  });

  test("an entry that names each field is read as written", () => {
    const entry = modelEntry("coder", {
      agent: "opencode",
      model: "anthropic/claude-sonnet-5",
      run_in: "e2b",
      timeout_seconds: 900,
      max_concurrent: 2,
      state_dir: "/var/lib/crewlet/opencode",
      auth: { mode: "api-key", api_key: "${OPENAI_KEY}" },
    });
    expect(entry).toMatchObject({
      agent: "opencode",
      agentDefault: false,
      executor: true,
      runIn: "e2b",
      timeoutSeconds: 900,
      timeoutSet: true,
      maxConcurrent: 2,
      concurrencySet: true,
      stateDir: "/var/lib/crewlet/opencode",
      authMode: "api-key",
      authDefault: false,
      apiKey: { kind: "reference", name: "OPENAI_KEY" },
    });
  });

  // NAMES, NEVER VALUES. A credential field shows one whole reference or that
  // something is written inline: the mask, and a literal an engine let
  // through unmasked, alike — `Bearer ${X}` is a literal around a reference.
  test("a credential is the variable it names, or inline — never a value", () => {
    expect(credentialOf("${CLAUDE_TOKEN}")).toEqual({ kind: "reference", name: "CLAUDE_TOKEN" });
    expect(credentialOf(REDACTED)).toEqual({ kind: "inline" });
    expect(credentialOf("sk-live-123")).toEqual({ kind: "inline" });
    expect(credentialOf("Bearer ${X}")).toEqual({ kind: "inline" });
    expect(credentialOf("")).toBeNull();
    expect(credentialOf(undefined)).toBeNull();
    const env = modelEntry("o", {
      model: "anthropic/claude-sonnet-5",
      env: { OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}", A_LITERAL: "sk-live-123" },
    }).env;
    // By name, sorted, each value a credential.
    expect(env).toEqual([
      { name: "A_LITERAL", value: { kind: "inline" } },
      { name: "OPENROUTER_API_KEY", value: { kind: "reference", name: "OPENROUTER_API_KEY" } },
    ]);
    expect(JSON.stringify(env)).not.toContain("sk-live-123");
  });

  // THE DECLARATION ORDER IS WHAT A SEAT THAT NAMES NO MODEL FALLS BACK
  // THROUGH, and a JSON object's keys come back sorted, so it rides in
  // `llm_order` — reconciled against the map, as the engine reconciles it.
  test("models come in the document's declared order, reconciled against the map", () => {
    const llm = { alpha: {}, bravo: {}, charlie: {} };
    expect(providerOrder(llm, ["charlie", "gone", "charlie", "alpha"])).toEqual([
      "charlie",
      "alpha",
      "bravo",
    ]);
    // No order recorded: sorted, arbitrary and stable.
    expect(providerOrder({ zulu: {}, alpha: {} }, undefined)).toEqual(["alpha", "zulu"]);
    const doc: CompanyDocument = {
      providers: {
        llm: { opus: { model: "opus" }, sonnet: { model: "sonnet" } },
        llm_order: ["sonnet", "opus"],
      },
    };
    expect(modelEntries(doc).map((e) => e.key)).toEqual(["sonnet", "opus"]);
    // No company configured is no model, not a failure.
    expect(modelEntries(null)).toEqual([]);
  });
});

describe("how a model signs in, in words", () => {
  const entry = (raw: Record<string, unknown>) => modelEntry("m", { model: "sonnet", ...raw });

  test("each sign-in says where its credential comes from, and only ever a name", () => {
    expect(signInSource(entry({ auth: { token: "${CLAUDE_TOKEN}" } }))).toBe("CLAUDE_TOKEN");
    expect(signInSource(entry({}))).toBe("CLI login");
    expect(signInSource(entry({ auth: { credential_bundle: "${BUNDLE}" } }))).toBe(
      "CLI login, from a bundle",
    );
    expect(signInSource(entry({ auth: { token: REDACTED } }))).toBe("token written inline");
    expect(signInSource(entry({ auth: { mode: "api-key", api_key: "${KEY_A}" } }))).toBe("KEY_A");
    expect(signInSource(entry({ auth: { mode: "api-key", api_key: "sk-live-123" } }))).toBe(
      "key written inline",
    );
    expect(signInSource(entry({ auth: { mode: "api-key" } }))).toBe("the CLI’s own key variable");
    expect(signInSource(entry({ auth: { mode: "inherit-env" } }))).toBe(
      "the engine’s own variables",
    );
  });

  // ONLY A CLI WITH AN IN-BOX RUNNER RUNS AN EXECUTOR: an entry on any other
  // serves the text phases, and an executor chain skips it at launch — so the
  // screen must never say Codex runs a seat's work.
  test("only an executor CLI is said to run a seat's executor", () => {
    for (const agent of EXECUTOR_CLI_AGENTS) {
      const entry = modelEntry("m", { agent, model: "x" });
      expect(entry.executor).toBe(true);
      expect(roleWords(entry).label).toBe("Executor");
    }
    for (const agent of ["codex", "gemini-cli", "custom"]) {
      const entry = modelEntry("m", { agent, model: "x", run_in: "e2b" });
      expect(entry.executor).toBe(false);
      expect(roleWords(entry).label).toBe("Text phases");
    }
  });

  // A value a newer engine sends that this build has no words for is drawn as
  // itself, never as nothing and never as a sign-in it is not.
  test("a sign-in this build does not know is said as itself", () => {
    expect(authWords("api-key").label).toBe("API key");
    expect(authWords("passkey").label).toBe("passkey");
  });

  test("a row's second line is the CLI and the model it runs", () => {
    expect(modelLine({ agent: "opencode", model: "anthropic/claude-sonnet-5" })).toBe(
      "opencode · anthropic/claude-sonnet-5",
    );
    expect(modelLine({ agent: "claude-code", model: "" })).toBe("claude-code");
  });
});

describe("the edit", () => {
  const entity = {
    agent: "codex",
    model: "gpt-5",
    run_in: "e2b",
    state_dir: "/var/lib/crewlet/codex",
    env: { HTTPS_PROXY: "${PROXY}" },
    overrides: { binary: "/opt/codex" },
    timeout_seconds: 900,
    auth: { mode: "api-key", api_key: "${OPENAI_KEY}", credential_bundle: "${BUNDLE}" },
  };

  // ONLY WHAT THE FORM SHOWS CHANGES. The CLI, where it runs, its
  // home, its environment, its overrides and the bundle are the entry's
  // identity, and go back exactly as read.
  test("sends back the entity it read, with only the edited fields replaced", () => {
    const read = formOf(entity);
    expect(read).toEqual({
      model: "gpt-5",
      credential: "${OPENAI_KEY}",
      timeout: "900",
      concurrency: "",
    });
    const sent = modelEntity(entity, { ...read, model: "gpt-5-codex", concurrency: "2" });
    expect(sent).toEqual({ ...entity, model: "gpt-5-codex", max_concurrent: 2 });
    // An emptied cap is the default, so the field goes rather than a 0.
    expect(modelEntity(entity, { ...read, timeout: "" })).not.toHaveProperty("timeout_seconds");
  });

  // ONE CREDENTIAL, THE ONE THE SIGN-IN READS: the engine refuses one written
  // under a mode that does not read it, so the form never edits it there.
  test("edits the credential its sign-in reads, and no other", () => {
    expect(credentialField(entity)).toBe("api_key");
    const keyed = modelEntity(entity, { ...formOf(entity), credential: "${OPENAI_KEY_B}" });
    expect(keyed.auth).toEqual({ ...entity.auth, api_key: "${OPENAI_KEY_B}" });

    const subscription = { model: "sonnet", auth: { token: "${T}" } };
    expect(credentialField(subscription)).toBe("token");
    expect(formOf(subscription).credential).toBe("${T}");
    const cleared = modelEntity(subscription, { ...formOf(subscription), credential: "" });
    // EMPTIED, it is removed rather than sent as an empty value, which the
    // engine would read as a credential that resolves to nothing.
    expect(cleared.auth).toEqual({});

    const inherited = { model: "sonnet", auth: { mode: "inherit-env" } };
    expect(credentialField(inherited)).toBeNull();
    expect(formOf(inherited).credential).toBe("");
    expect(modelEntity(inherited, { ...formOf(inherited), credential: "${IGNORED}" }).auth).toEqual(
      inherited.auth,
    );
  });

  // AN INLINE CREDENTIAL'S VALUE NEVER REACHED THIS PAGE: it goes back as the
  // mask, which the engine restores from the revision it was read from.
  test("an inline credential goes back as the mask", () => {
    const inline = { model: "sonnet", auth: { token: REDACTED } };
    const read = formOf(inline);
    expect(read.credential).toBe(REDACTED);
    expect(modelEntity(inline, { ...read, model: "opus" }).auth).toEqual({ token: REDACTED });
  });

  test("a form the engine would refuse is stopped before asking, and an empty cap is the default", () => {
    const ok = { model: "m", credential: "", timeout: "", concurrency: "" };
    expect(formErrors(ok)).toEqual({});
    expect(formErrors({ ...ok, model: " " })).toHaveProperty("model");
    for (const timeout of ["0", "-5", "soon"]) {
      expect(formErrors({ ...ok, timeout })).toHaveProperty("timeout");
    }
    expect(formErrors({ ...ok, timeout: "30.5" })).toEqual({});
    for (const concurrency of ["0", "1.5", "many"]) {
      expect(formErrors({ ...ok, concurrency })).toHaveProperty("concurrency");
    }
    expect(formErrors({ ...ok, concurrency: "2" })).toEqual({});
  });

  // THE AUDIT SUMMARY NAMES A CREDENTIAL BY ITS VARIABLE, never by a value: a
  // literal typed into the field is "a value", and an inline one "inline".
  test("the summary says what moved without carrying a credential", () => {
    const before = formOf(entity);
    const summary = editSummary("coder", entity, before, {
      ...before,
      model: "gpt-5-codex",
      credential: "sk-live-123",
      timeout: "",
    });
    expect(summary).toBe(
      "Edit the model coder: model gpt-5 → gpt-5-codex; API key OPENAI_KEY → a value; timeout 900 s → default",
    );
    expect(summary).not.toContain("sk-live-123");
    expect(
      editSummary(
        "x",
        { model: "m", auth: { token: REDACTED } },
        formOf({ model: "m", auth: { token: REDACTED } }),
        { ...formOf({ model: "m", auth: { token: REDACTED } }), credential: "${T}" },
      ),
    ).toBe("Edit the model x: token inline → T");
    expect(formChanged(before, { ...before, model: " gpt-5 " })).toBe(false);
  });

  // PLACED BY THE PROBLEM'S SEGMENTS, never its message — and only beside the
  // field the form shows: a fault in another auth field is about something
  // this form cannot change, so it is said above it.
  test("the engine's problems sit beside the field they are about", () => {
    const problem = (...seg: (string | number)[]) => ({
      path: seg.join("."),
      segments: seg,
      kind: "conflict",
      message: `about ${seg.slice(3).join(".")}`,
    });
    const { fields, rest } = problemsByField(
      [
        problem("providers", "llm", "coder", "auth", "api_key"),
        problem("providers", "llm", "coder", "auth", "token"),
        problem("providers", "llm", "coder", "timeout_seconds"),
        problem("providers", "llm", "coder", "max_concurrent"),
        problem("providers", "llm", "coder", "model"),
        problem("providers", "llm", "other", "model"),
      ],
      "coder",
      entity,
    );
    expect(fields).toEqual({
      credential: "about auth.api_key",
      timeout: "about timeout_seconds",
      concurrency: "about max_concurrent",
      model: "about model",
    });
    expect(rest).toEqual(["about auth.token", "about model"]);
  });
});
