/**
 * A decision on a proposal card is one operation to the engine: the approve
 * and decline routes require an `Idempotency-Key` (`opkey.Require`), so each
 * press carries one, and a retry of the same decision carries the same one,
 * which the engine collapses rather than deciding twice.
 */

import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { AssistantProposalCard } from "~/contract/assistant.ts";
import { approve, decline, resetSessionForTest } from "./session.ts";

beforeEach(() => {
  resetSessionForTest();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const card: AssistantProposalCard = {
  proposal_id: "8a1d5c7e-3f0b-4c9a-9e2d-6b7f1a0c4d21",
  scope: "server",
  tool: "update_task",
  title: "Rename the task",
  changes: [],
  digest: `sha256:${"b".repeat(64)}`,
  expires_at: "2026-10-24T10:41:00Z",
  state: "proposed",
  approve_label: "Rename",
};

/** The engine, answering every decision with `status`, and the keys it was sent. */
function engine(status: number): { path: string; key: string | null }[] {
  const sent: { path: string; key: string | null }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({
        path: new URL(url).pathname,
        key: new Headers(init.headers).get("Idempotency-Key"),
      });
      const body =
        status < 300
          ? { proposal: { id: card.proposal_id, state: "declined" }, turn_id: "t", after_seq: 0 }
          : { error: "internal_error" };
      return new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

test("a decline is sent under an operation key, and its retry under the same one", async () => {
  const sent = engine(500);
  await decline(card);
  await decline(card);
  const declines = sent.filter((r) => r.path.endsWith("/decline"));
  expect(declines).toHaveLength(2);
  expect(declines[0]!.key).toMatch(/^[0-9a-f-]{36}$/);
  expect(declines[1]!.key).toBe(declines[0]!.key);
});

test("an approval and a decline of one proposal are two operations", async () => {
  const sent = engine(500);
  await approve(card);
  await decline(card);
  const approval = sent.find((r) => r.path.endsWith("/approve"))!;
  const refusal = sent.find((r) => r.path.endsWith("/decline"))!;
  expect(approval.key).toMatch(/^[0-9a-f-]{36}$/);
  expect(refusal.key).toMatch(/^[0-9a-f-]{36}$/);
  expect(refusal.key).not.toBe(approval.key);
});
