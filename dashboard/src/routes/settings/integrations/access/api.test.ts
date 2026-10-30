/**
 * The access routes as the tab calls them: a save is conditional on the
 * revision it edited, sent as the entity tag the read's ETag carries, and each
 * refusal the routes answer reads in the reader's words.
 */

import { afterEach, expect, test, vi } from "vitest";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { RestError } from "~/protocol/rest.ts";
import { accessApi, refusalWords } from "./api.ts";
import { datadogAnswer } from "./fixtures.ts";

afterEach(() => {
  vi.unstubAllGlobals();
});

function stub(status: number, payload: unknown) {
  const calls: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(JSON.stringify(payload), {
        status,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
  return calls;
}

test("a save sends the draft alone, conditional on the revision as an entity tag", async () => {
  const calls = stub(200, datadogAnswer());
  await accessApi.save("datadog", { qa: { datadog: { enabled: true } } }, "rev-7");
  const call = calls.at(-1)!;
  expect(call.url).toContain("/setup/integrations/datadog/access");
  expect(call.init.method).toBe("PUT");
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"rev-7"');
  expect(JSON.parse(String(call.init.body))).toEqual({
    seats: { qa: { datadog: { enabled: true } } },
  });
});

test("an install asks for a fresh link and answers it", async () => {
  const calls = stub(200, { url: "https://slack.com/oauth/v2/authorize?state=one-use" });
  const link = await accessApi.install("slack", "pm", "slack");
  expect(calls.at(-1)!.url).toContain("/setup/integrations/slack/access/seats/pm/slack/install");
  expect(link.url).toBe("https://slack.com/oauth/v2/authorize?state=one-use");
});

test("each refusal the access routes answer reads in the reader's words", async () => {
  for (const [code, words] of [
    ["revision_advanced", /Somebody changed these agents/],
    ["pass_in_flight", /A sync is running/],
    ["seat_still_on", /Turn this agent off and save/],
    ["credential_external", /Rotate it where it was issued/],
    ["config_managed", /managed by the operator through each agent's Seat object/],
  ] as const) {
    stub(409, { error: code, message: "refused" });
    const err = await accessApi.act("datadog", "sre", "datadog", "remove").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestError);
    expect(refusalWords(err)).toMatch(words);
  }
});

test("a refusal says the vendor's reason, then its remedy", async () => {
  stub(409, {
    error: "install_refused",
    message: "Slack refused the install",
    detail: "Slack refused the install",
    hint: "approve the app in Slack's admin settings",
  });
  const err = await accessApi.install("slack", "pm", "slack").catch((e: unknown) => e);
  expect(refusalWords(err)).toMatch(/\. Approve the app in Slack's admin settings\.$/);
});
