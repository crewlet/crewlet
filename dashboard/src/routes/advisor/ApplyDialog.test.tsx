/**
 * Apply never sends a change the engine has not just re-derived: every
 * outcome of the re-check, drawn and acted on, over the engine's routes
 * stubbed at `fetch`.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { ApplyDialog } from "./ApplyDialog.tsx";
import type { AdvisorKeptFinding, AdvisorRecheck } from "~/protocol/advisorFindings.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const ID = "1f2e3d4c-5b6a-4978-8877-665544332211";

const FINDING: AdvisorKeptFinding = {
  id: ID,
  key: "capacity.add_seat:group:eng/backend",
  class: "capacity.add_seat",
  source: "rule",
  actor: "owner",
  area: "capacity",
  severity: "costs",
  subject: {
    kind: "group",
    id: "eng/backend",
    name: "Backend Engineer",
    seat: "backend",
    unit: "eng",
    schedule: "",
  },
  title: "Work waits for Backend Engineer every day",
  why: "Ready work waited 5 item-hours a day while the seat was busy 85% of the time.",
  item: "",
  names: [],
  path: "",
  entries: null,
  explanation: null,
  state: "open",
  close_reason: null,
  ineffective: null,
  snoozed_until: null,
  opened_at: "2026-11-12T08:00:00Z",
  last_confirmed_at: "2026-11-19T01:00:00Z",
  closed_at: null,
  effect_due_at: null,
  confirmed: "fresh",
  revision: 1,
  seen_before: 0,
  impact: { hours_per_week: 35, tokens_per_week: null },
  evidence: {
    window: { from: "2026-11-12T00:00:00Z", to: "2026-11-19T00:00:00Z" },
    facts: [{ key: "wait_h", label: "Waiting a day", value: 5, unit: "hours" }],
    records: [],
  },
  fix: {
    kind: "config_change",
    sentence: "Add a second Backend Engineer, cloned from the first.",
    options: [],
  },
};

const SEND = {
  method: "PUT" as const,
  path: "/config/units/eng",
  body: { roles: ["backend", "backend-2"] },
  headers: { "If-Match": '"rev-41"' },
  dry_run_path: "/config/units/eng?dry_run=true",
};

const later = (minutes: number) => new Date(Date.now() + minutes * 60_000).toISOString();

function answer(patch: Partial<AdvisorRecheck> = {}): AdvisorRecheck {
  return {
    outcome: "holds",
    finding: FINDING,
    moved: [],
    previous_fix: null,
    preview: {
      rows: [{ field: "roles", label: "Seats in Engineering", before: "1", after: "2" }],
      notes: [],
    },
    send: SEND,
    digest: "sha256:aaa",
    checked_at: new Date().toISOString(),
    expires_at: later(10),
    refusal: null,
    ...patch,
  };
}

const json = (body: unknown, status = 200, etag?: string) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...(etag ? { ETag: etag } : {}) },
  });

/** The engine: each re-check answered from `checks` in turn, and every request recorded. */
function engine(
  checks: AdvisorRecheck[],
  { sendStatus = 200, appliedStatus = 200 }: { sendStatus?: number; appliedStatus?: number } = {},
) {
  const sent: { method: string; path: string; init: RequestInit }[] = [];
  let next = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init: RequestInit = {}) => {
      const url = new URL(input);
      const method = init.method ?? "GET";
      sent.push({ method, path: url.pathname + url.search, init });
      if (url.pathname === `/advisor/findings/${ID}/recheck`) {
        return json(checks[Math.min(next++, checks.length - 1)]);
      }
      if (url.pathname === `/advisor/findings/${ID}/applied`) {
        return appliedStatus === 200
          ? json({ ...FINDING, state: "applied", effect_due_at: "2026-11-22T09:00:00Z" })
          : json({ outcome: "recheck" }, appliedStatus);
      }
      if (url.pathname === "/config/units/eng") {
        if (url.search.includes("dry_run")) return json({ result: "valid" });
        return sendStatus === 200
          ? json({ result: "saved" }, 200, '"rev-42"')
          : json({ error: "precondition_failed", message: "It moved." }, sendStatus);
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  return sent;
}

function mount(onSettled = vi.fn()) {
  render(
    <LayerHost>
      <ApplyDialog finding={FINDING} onClose={() => {}} onSettled={onSettled} />
    </LayerHost>,
  );
  return onSettled;
}

const writes = (sent: { method: string; path: string; init: RequestInit }[]) =>
  sent.filter((r) => r.path.startsWith("/config/"));

test("a finding that still holds shows the change and sends it under the revision the re-check read", async () => {
  const sent = engine([answer()]);
  mount();
  expect(screen.getByText("Checking it still holds")).toBeDefined();
  await screen.findByText("It still holds");
  expect(screen.getByText("Waiting a day")).toBeDefined();
  expect(screen.getByText("Seats in Engineering")).toBeDefined();
  expect(writes(sent)).toEqual([]);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }));
  });
  await screen.findByText("The change was applied");
  const [dry, real] = writes(sent);
  expect(dry!.path).toBe("/config/units/eng?dry_run=true");
  expect(real!.path).toBe("/config/units/eng");
  expect(new Headers(real!.init.headers).get("If-Match")).toBe('"rev-41"');
  // THE RE-CHECK NAMED THE FIX'S OPTION, and the report names the digest the
  // change was sent under, for the engine to read the target itself.
  const checked = sent.find((r) => r.path === `/advisor/findings/${ID}/recheck`)!;
  expect(JSON.parse(String(checked.init.body))).toEqual({ option: 0 });
  const told = sent.find((r) => r.path === `/advisor/findings/${ID}/applied`)!;
  expect(JSON.parse(String(told.init.body))).toEqual({ digest: "sha256:aaa" });
});

test("a finding that changed since it was found shows what moved, and sends nothing until confirmed", async () => {
  const sent = engine([
    answer({
      outcome: "changed",
      digest: "sha256:bbb",
      finding: {
        ...FINDING,
        revision: 2,
        evidence: {
          ...FINDING.evidence,
          facts: [{ key: "wait_h", label: "Waiting a day", value: 9, unit: "hours" }],
        },
      },
      moved: [{ key: "wait_h", label: "Waiting a day", was: "5 h", now: "9 h" }],
    }),
  ]);
  const settled = mount();
  await screen.findByText("It changed since it was found");
  expect(screen.getByText("Since this was found")).toBeDefined();
  const moved = screen.getByRole("region", { name: "What moved since it was found" });
  expect(within(moved).getByText("5 h")).toBeDefined();
  expect(within(moved).getByText("9 h")).toBeDefined();
  const now = screen.getByRole("region", { name: "As checked now" });
  expect(within(now).getByText("9 h")).toBeDefined();
  expect(screen.getByRole("button", { name: "Apply the updated change" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Keep it open" })).toBeDefined();
  expect(writes(sent)).toEqual([]);
  // THE PAGE IS TOLD: the finding was revised.
  expect(settled).toHaveBeenCalledTimes(1);
});

test("a finding that no longer holds is closed, and nothing is sent", async () => {
  const sent = engine([
    answer({
      outcome: "resolved",
      finding: { ...FINDING, state: "resolved", close_reason: "condition_cleared" },
      send: null,
      preview: null,
      moved: [{ key: "wait_h", label: "Waiting a day", was: "5 h", now: "0 h" }],
    }),
  ]);
  mount();
  await screen.findByText("No longer needed");
  expect(screen.getByText(/The condition cleared/)).toBeDefined();
  expect(screen.queryByRole("button", { name: /Apply/ })).toBeNull();
  expect(writes(sent)).toEqual([]);
});

test("a finding whose subject is gone is retired, and nothing is sent", async () => {
  const sent = engine([
    answer({
      outcome: "not_applicable",
      finding: { ...FINDING, state: "retired", close_reason: "subject_gone" },
      send: null,
      preview: null,
    }),
  ]);
  mount();
  await screen.findByText("This no longer applies");
  expect(screen.getByText(/What it was about is gone/)).toBeDefined();
  expect(screen.queryByRole("button", { name: /Apply/ })).toBeNull();
  expect(writes(sent)).toEqual([]);
});

test("a reader the change's route refuses is told who can apply it, and the finding stays open", async () => {
  const sent = engine([
    answer({
      refusal: {
        reason: "Changing a unit needs admin.",
        who_can: "An administrator can apply it.",
      },
    }),
  ]);
  const settled = mount();
  await screen.findByText("You can't apply this one");
  expect(screen.getByText(/An administrator can apply it/)).toBeDefined();
  expect(screen.queryByRole("button", { name: /Apply/ })).toBeNull();
  expect(writes(sent)).toEqual([]);
  expect(settled).not.toHaveBeenCalled();
});

test("a target that moved between the re-check and the send is re-checked, and the new answer shown", async () => {
  const sent = engine(
    [
      answer(),
      answer({
        outcome: "changed",
        digest: "sha256:ccc",
        moved: [{ key: "seats", label: "Seats in the group", was: "1", now: "2" }],
      }),
    ],
    { sendStatus: 412 },
  );
  mount();
  await screen.findByText("It still holds");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }));
  });
  await screen.findByText("It changed since it was found");
  expect(sent.filter((r) => r.path.endsWith("/recheck"))).toHaveLength(2);
  expect(sent.some((r) => r.path.endsWith("/applied"))).toBe(false);
  // ONE SEND, refused: nothing is sent again until the person presses again.
  expect(writes(sent).filter((r) => !r.path.includes("dry_run"))).toHaveLength(1);
});

test("an answer older than ten minutes is asked again, and sent only if the engine answers the same change", async () => {
  const stale = answer({ expires_at: new Date(Date.now() - 1000).toISOString() });
  const moved = answer({ digest: "sha256:ddd", outcome: "changed" });
  const sent = engine([stale, moved]);
  mount();
  await screen.findByText("It still holds");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }));
  });
  await screen.findByText("It changed since it was found");
  expect(writes(sent)).toEqual([]);

  cleanup();
  const again = engine([stale, answer({ digest: "sha256:aaa" })]);
  mount();
  await screen.findByText("It still holds");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }));
  });
  await screen.findByText("The change was applied");
  expect(again.filter((r) => r.path.endsWith("/recheck"))).toHaveLength(2);
});

test("an engine holding a newer revision of the finding answers the send with a re-check", async () => {
  const sent = engine([answer(), answer({ outcome: "changed", digest: "sha256:eee" })], {
    appliedStatus: 409,
  });
  mount();
  await screen.findByText("It still holds");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }));
  });
  await waitFor(() => expect(screen.getByText("It changed since it was found")).toBeDefined());
  expect(sent.filter((r) => r.path.endsWith("/recheck"))).toHaveLength(2);
});

// A FIX WITH SEVERAL CHANGES IS CHOSEN FROM, and each choice is re-checked
// before it can be sent: the engine builds and checks the change picked.
test("a fix with several options re-checks the one the person picks", async () => {
  const sent = engine([answer(), answer({ digest: "sha256:opt1" })]);
  render(
    <LayerHost>
      <ApplyDialog
        finding={{
          ...FINDING,
          fix: {
            ...FINDING.fix,
            options: [
              {
                label: "Add one seat",
                change: { entity: "units", id: "eng", set: {}, summary: "a" },
              },
              {
                label: "Raise the round cap",
                change: { entity: "roles", id: "backend", set: {}, summary: "b" },
              },
            ],
          },
        }}
        onClose={() => {}}
        onSettled={() => {}}
      />
    </LayerHost>,
  );
  await screen.findByText("It still holds");
  await act(async () => {
    fireEvent.click(screen.getByRole("radio", { name: /Raise the round cap/ }));
  });
  await waitFor(() => expect(sent.filter((r) => r.path.endsWith("/recheck"))).toHaveLength(2));
  const bodies = sent
    .filter((r) => r.path.endsWith("/recheck"))
    .map((r) => JSON.parse(String(r.init.body)));
  expect(bodies).toEqual([{ option: 0 }, { option: 1 }]);
  expect(writes(sent)).toEqual([]);
});
