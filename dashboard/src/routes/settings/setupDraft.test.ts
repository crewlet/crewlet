// @vitest-environment node

/**
 * An integration's settings form as the assistant sees it: every field the
 * form shows, a credential never, and a choice only among its own values.
 */

import { describe, expect, test } from "vitest";
import { draftSnapshot, parseDraftChange } from "~/app/assistant/draft.ts";
import type { SetupRequirement } from "~/protocol/index.ts";
import { setupDraftFields, setupPath } from "./setupDraft.ts";

const req = (over: Partial<SetupRequirement>): SetupRequirement =>
  ({
    field: "url",
    label: "Site address",
    kind: "url",
    config_path: "jira.url",
    required: true,
    present: false,
    ...over,
  }) as SetupRequirement;

const values = {
  "jira:url": "https://example.atlassian.net",
  "jira:api_key": "sk-typed",
  "jira:mode": "cloud",
};

const spec = (apply = (_: Readonly<Record<string, string>>) => {}) => ({
  kind: "client" as const,
  scope: "Jira settings (not saved)",
  value: values,
  fields: setupDraftFields([
    { key: "jira:url", r: req({}) },
    { key: "jira:api_key", r: req({ field: "api_key", label: "API key", kind: "secret" }) },
    {
      key: "jira:mode",
      r: req({
        field: "mode",
        label: "Deployment",
        kind: "choice",
        choices: [
          { value: "cloud", label: "Cloud" },
          { value: "server", label: "Server" },
        ],
      }),
    },
  ]),
  apply,
});

describe("the integration settings draft", () => {
  test("names each field by a path in the grammar, and never sends a credential", () => {
    expect(setupPath("atlassian/Alice:Bot Token")).toBe("atlassian.alice.bot_token");
    expect(draftSnapshot(spec()).fields).toEqual([
      {
        path: "jira.url",
        label: "Site address",
        kind: "text",
        value: values["jira:url"],
        editable: true,
      },
      { path: "jira.api_key", label: "API key", kind: "text", value: "", redacted: true },
      {
        path: "jira.mode",
        label: "Deployment",
        kind: "text",
        value: "cloud",
        hint: "One of cloud, server",
        editable: true,
      },
    ]);
  });

  test("sets a text or a choice through the form's own values, and refuses a credential or a stray choice", () => {
    expect(parseDraftChange(spec(), { values: { "jira.mode": "server" } })).toMatchObject({
      ok: true,
      value: { next: { ...values, "jira:mode": "server" } },
    });
    expect(parseDraftChange(spec(), { values: { "jira.api_key": "x" } }).ok).toBe(false);
    expect(parseDraftChange(spec(), { values: { "jira.mode": "desktop" } })).toEqual({
      ok: false,
      reason: "Deployment is one of cloud, server.",
    });
  });
});
