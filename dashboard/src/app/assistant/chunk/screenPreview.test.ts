// @vitest-environment node

/**
 * A page proposal's card is the screen's own account of it while it waits:
 * the screen's parser runs FIRST over the payload the model wrote, and only a
 * value it took reaches the screen's preview and gate. Anything else keeps
 * the engine's rows.
 */

import { afterEach, describe, expect, test } from "vitest";
import type { AssistantProposalCard } from "~/contract/assistant.ts";
import {
  registerPage,
  resetPageContextForTest,
  setFramePage,
  type PageActionSpec,
} from "../context.ts";
import { screenPreviewOf } from "./blocks.tsx";

afterEach(resetPageContextForTest);

const seen: unknown[] = [];

/** The work list's filter, as a screen declares it: it takes `{status}` and refuses `assignee`. */
const spec: PageActionSpec<{ status?: string; assignee?: string }> = {
  scope: "Worklet list filters",
  args: { type: "object" },
  parse: (raw) => {
    seen.push(raw);
    return raw && typeof raw === "object" && !Array.isArray(raw)
      ? { ok: true, value: raw as { status?: string; assignee?: string } }
      : { ok: false, reason: "Not a set of filters." };
  },
  preview: (v) => [{ field: "Status", next: v.status ?? "Cleared", current: "Open" }],
  blocked: (v) => (v.assignee !== undefined ? "This list is one person's work." : null),
  apply: () => {},
};

function card(over: Partial<AssistantProposalCard> = {}): AssistantProposalCard {
  return {
    proposal_id: "p",
    scope: "page",
    tool: "propose_page_change",
    title: "Filter this list",
    changes: [{ kind: "changed", field: "status", label: "status", from: "", to: "blocked" }],
    digest: `sha256:${"a".repeat(64)}`,
    expires_at: "2026-10-24T10:41:00Z",
    state: "proposed",
    approve_label: "Apply to this list",
    page_action: { name: "work.filter.apply", payload: { status: "Blocked" } },
    ...over,
  };
}

function mounted() {
  setFramePage({
    screen: "worklet-tasks",
    title: "Worklet › Tasks",
    path: ["worklet", "tasks"],
    params: {},
    query: {},
  });
  registerPage({ actions: { "work.filter.apply": spec as PageActionSpec<unknown> } });
}

describe("a page proposal's card", () => {
  test("draws the screen's own rows, from a payload the screen parsed first", () => {
    mounted();
    seen.length = 0;
    expect(screenPreviewOf(card())).toEqual({
      rows: [{ field: "Status", next: "Blocked", current: "Open" }],
      blocked: null,
    });
    expect(seen).toEqual([{ status: "Blocked" }]);
  });

  test("says why the screen's gate refuses it, before the person presses anything", () => {
    mounted();
    const preview = screenPreviewOf(
      card({ page_action: { name: "work.filter.apply", payload: { assignee: null } } }),
    );
    expect(preview?.blocked).toBe("This list is one person's work.");
  });

  test("keeps the engine's rows where the screen's parser refuses the payload", () => {
    mounted();
    expect(
      screenPreviewOf(card({ page_action: { name: "work.filter.apply", payload: "status=x" } })),
    ).toBeNull();
  });

  test("keeps the engine's rows where no screen here declares it, or it is decided", () => {
    mounted();
    expect(
      screenPreviewOf(card({ page_action: { name: "spend.range.set", payload: {} } })),
    ).toBeNull();
    expect(screenPreviewOf(card({ page_action: { name: "made.up", payload: {} } }))).toBeNull();
    expect(screenPreviewOf(card({ state: "applied" }))).toBeNull();
    expect(screenPreviewOf(card({ scope: "server" }))).toBeNull();
    const { page_action: _, ...bare } = card();
    expect(screenPreviewOf(bare)).toBeNull();
  });
});
