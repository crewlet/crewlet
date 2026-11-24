/**
 * Under the models: the Fleets card, then Who runs on which model, and the
 * confirms they open. The invariants: every fleet with its status, strategy
 * and members, the default fleet listed and never deletable; every picker
 * lists Fleets then Models, an empty fleet an ordinary choice; a seat opens to
 * the engine's own jobs; a pick asks once and writes the document or the
 * helper's setting; a fleet change is one `llm-fleets` write; a member reads
 * everything and meets no write control.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Announcer, LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { SEAT_JOBS } from "~/lib/modelUses.ts";
import { SEAT_JOB_WORDS } from "~/lib/seatJobs.ts";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import type { Fleet, FleetAssignment, FleetMember } from "~/protocol/fleets.ts";
import {
  ASSISTANT_CHAT,
  DEFAULT_FLEET_EMPTY,
  MODELS_NOW,
  at,
  fleetsFixture,
  modelEntry,
  modelsData,
  modelsDoc,
  runs,
  signIn,
} from "~/routes/settings/models/fixtures.ts";
import { WhoRuns } from "./SeatJobs.tsx";
import type { ModelsData } from "./data.ts";

const writes = vi.hoisted(() => ({
  docs: [] as { doc: Record<string, any>; dryRun: boolean; summary: string }[],
  patches: [] as unknown[],
  fleets: [] as { method: string; key: string; body: Record<string, unknown> }[],
  current: {} as Record<string, unknown>,
}));
vi.mock("./api.ts", async (actual) => ({
  ...(await actual<typeof import("./api.ts")>()),
  writeDocument: async (doc: Record<string, any>, o: { dryRun: boolean; summary: string }) => {
    writes.docs.push({ doc, dryRun: o.dryRun, summary: o.summary });
    return o.dryRun
      ? { kind: "valid", baseRevisionId: "r1", warnings: [], derived: null }
      : { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
}));
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: async () => ({
    kind: "document",
    document: structuredClone(writes.current),
    etag: '"r1"',
  }),
  dryRunPatch: async () => ({ kind: "valid", baseRevisionId: "r1", warnings: [], derived: null }),
  savePatch: async (patch: unknown) => {
    writes.patches.push(patch);
    return { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
  // EVERY LISTED FLEET READS WITH ITS TAG, the engine-made default fleet too.
  getEntity: async (_kind: string, key: string) => ({
    kind: "entity",
    entity: {},
    etag: `"${key}"`,
  }),
  putEntity: async (_kind: string, key: string, body: Record<string, unknown>) => {
    writes.fleets.push({ method: "PUT", key, body });
    return { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
  createEntity: async (_kind: string, key: string, body: Record<string, unknown>) => {
    writes.fleets.push({ method: "CREATE", key, body });
    return { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
}));

const member = (entry: string, over: Partial<FleetMember> = {}): FleetMember => ({
  entry,
  kind: "plan",
  state: "available",
  until: null,
  why: "",
  share: 0.3,
  overflow: false,
  last_served_at: at(-3),
  serving: [],
  ...over,
});

/** Claude Opus, Balanced: claude-max serves Backend Engineer and the Advisor, claude-team is near its limit, claude-api is overflow. */
const OPUS: Fleet = {
  key: "claude-opus",
  name: "Claude Opus",
  is_default: false,
  class: "Claude Opus",
  harness: "claude-code",
  strategy: "balanced",
  threshold_points: 5,
  spread: 3,
  members: [
    member("claude-max", { serving: ["backend-engineer", "advisor"] }),
    member("claude-team", { state: "near", share: 0.85 }),
    member("claude-api", { kind: "api", share: 0, overflow: true }),
  ],
};
/** Opus in order: claude-max, then claude-api. */
const ORDERED: Fleet = {
  ...OPUS,
  key: "opus-in-order",
  name: "Opus in order",
  strategy: "sequential",
  spread: null,
  members: [member("claude-max"), member("claude-api", { kind: "api" })],
};

const ASSIGNMENTS: FleetAssignment[] = [
  {
    subject: "assistant",
    kind: "helper",
    name: "Ask Crewlet",
    target: { pinned: "claude-sonnet" },
    now: "claude-sonnet",
    waiting_until: null,
  },
  {
    subject: "advisor",
    kind: "helper",
    name: "Advisor",
    target: { fleet: "claude-opus" },
    now: "claude-max",
    waiting_until: null,
  },
  {
    subject: "backend-engineer",
    kind: "seat",
    name: "Backend Engineer",
    target: { fleet: "claude-opus" },
    now: "claude-max",
    waiting_until: null,
  },
  {
    subject: "pm",
    kind: "seat",
    name: "Product Manager",
    target: { fleet: "default-fleet" },
    now: null,
    waiting_until: null,
  },
  {
    subject: "qa",
    kind: "seat",
    name: "Quality Lead",
    target: { pinned: "" },
    now: null,
    waiting_until: null,
    jobs: SEAT_JOBS.map((job) =>
      job === "executor"
        ? { job, target: { fleet: "claude-opus" }, now: "claude-team", waiting_until: null }
        : { job, target: { pinned: "claude-sonnet" }, now: "claude-sonnet", waiting_until: null },
    ),
  },
];

function company({
  admin = true,
  fleets = [DEFAULT_FLEET_EMPTY, OPUS, ORDERED],
  assignments = ASSIGNMENTS,
}: { admin?: boolean; fleets?: Fleet[]; assignments?: FleetAssignment[] } = {}): ModelsData {
  const opus = { model: "claude-opus-5" };
  const entries = [
    modelEntry({ key: "claude-max", ...opus }),
    modelEntry({ key: "claude-team", ...opus }),
    modelEntry({
      key: "claude-api",
      ...opus,
      sign_in: signIn({ route: "api-key", mode: "api-key", variable: "ANTHROPIC_API_KEY" }),
    }),
    modelEntry({ key: "claude-sonnet", used_by: [ASSISTANT_CHAT, ...runs("qa", "review")] }),
    modelEntry({
      key: "codex-review",
      harness: "codex",
      model: "gpt-6.1",
      helper_problem: "This model's tool keeps a tool Crewlet cannot turn off.",
    }),
  ];
  return modelsData({
    admin,
    entries,
    doc: {
      ...modelsDoc({
        "claude-max": { model: "claude-opus-5" },
        "claude-team": { model: "claude-opus-5" },
        "claude-api": { model: "claude-opus-5", auth: { mode: "api-key" } },
        "claude-sonnet": {},
        "codex-review": { harness: "codex" },
      }),
      roles: [
        { handle: "backend-engineer", llm: "fleet:claude-opus" },
        { handle: "pm", llm: "fleet:default-fleet" },
        { handle: "qa", llm: ["fleet:claude-opus"], llm_review: ["claude-sonnet", "claude-max"] },
      ],
      assistant: { model: "claude-sonnet" },
      advisor: { model: "fleet:claude-opus" },
    } as unknown as ModelsData["doc"] & object,
    fleets: fleetsFixture({ entries, admin, fleets, assignments }),
  });
}

function mount(data: ModelsData = company()) {
  writes.current = (data.doc as Record<string, unknown> | null) ?? {};
  writes.docs = [];
  writes.patches = [];
  writes.fleets = [];
  render(
    <TestClient>
      <LayerHost>
        <Announcer />
        <Router>
          <WhoRuns data={data} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
}

const fleetsTable = () => screen.getByRole("treegrid", { name: "Fleets and their members" });
const whoTable = () => screen.getByRole("treegrid", { name: "Consumers and what each runs on" });
const rows = (table: HTMLElement, level: string) =>
  within(table)
    .getAllByRole("row")
    .filter((r) => r.getAttribute("aria-level") === level);
const rowOf = (table: HTMLElement, text: string, level = "1") =>
  rows(table, level).find((r) => r.textContent?.includes(text))!;
const optionsOf = (trigger: HTMLElement) => {
  fireEvent.click(trigger);
  return screen.getAllByRole("option");
};
const expand = (table: HTMLElement, name: string) => {
  const row = rowOf(table, name);
  if (row.getAttribute("aria-expanded") === "false") {
    row.focus();
    fireEvent.keyDown(row, { key: "ArrowRight" });
  }
};

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("the fleets and who runs are two titled cards, their tables headed as the page names them", () => {
  mount();
  const fleets = screen.getByRole("heading", { name: "Fleets" }).closest("section")!;
  expect(fleets.textContent).toContain("Seats on a fleet run on whichever member has room.");
  const who = screen.getByRole("heading", { name: "Who runs on which model" }).closest("section")!;
  expect(who.textContent).toContain(
    "Choose the model or fleet each helper and seat runs on. New ones start on the default fleet, default-fleet.",
  );
  expect(
    within(fleetsTable())
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Name", "Status", "Utilization strategy", "Usage limits", "Serving now", "Actions"]);
  expect(
    within(whoTable())
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Consumers", "Runs on", "Status"]);
});

test("the default fleet is listed Empty with its No members yet row, and cannot be deleted", () => {
  mount();
  const row = rowOf(fleetsTable(), "default-fleet");
  expect(row.textContent).toContain("Empty");
  expect(row.textContent).not.toContain("Default");
  expect(rowOf(fleetsTable(), "No members yet", "2")).toBeDefined();
  fireEvent.click(within(row).getByRole("button", { name: "Actions for default-fleet" }));
  const remove = screen.getByRole("menuitem", { name: /Delete fleet/ });
  expect(remove.getAttribute("aria-disabled")).toBe("true");
  expect(remove.textContent).toContain("The default fleet can't be deleted");
});

test("a fleet's status counts its ready members, and its strategy says how it spreads", () => {
  mount();
  const opus = rowOf(fleetsTable(), "Claude Opus");
  expect(opus.textContent).toContain("3/3 ready");
  expect(opus.textContent).toContain("Balanced");
  expect(opus.textContent).toContain("Within 3 points");
  const ordered = rowOf(fleetsTable(), "Opus in order");
  expect(ordered.textContent).toContain("Sequential");
  expect(ordered.textContent).toContain("In order");
});

test("a member reads as its model, with who it serves by name", () => {
  mount();
  const max = rowOf(fleetsTable(), "claude-max", "2");
  expect(max.textContent).toContain("Ready");
  expect(within(max).getByLabelText("Serving Backend Engineer and Advisor")).toBeDefined();
  expect(rowOf(fleetsTable(), "claude-team", "2").textContent).toContain("Nobody");
});

test("Create fleet, the (+) and Settings open the fleet flow by its address", () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Create fleet" }));
  expect(window.location.hash).toBe("#/settings/models?fleet=new");
  fireEvent.click(screen.getByRole("button", { name: "Add members to Claude Opus" }));
  expect(window.location.hash).toBe("#/settings/models?fleet=claude-opus&step=members");
  fireEvent.click(screen.getByRole("button", { name: "Actions for Opus in order" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "Settings" }));
  expect(window.location.hash).toBe("#/settings/models?fleet=opus-in-order&step=members");
});

test("helpers and seats are listed together by name, and a seat opens to the engine's own jobs", () => {
  mount();
  const nameOf = (r: HTMLElement) => r.querySelector(".crewlet-org-label__name")?.textContent;
  expect(rows(whoTable(), "1").map(nameOf)).toEqual([
    "Advisor",
    "Ask Crewlet",
    "Backend Engineer",
    "Product Manager",
    "Quality Lead",
  ]);
  expand(whoTable(), "Quality Lead");
  expect(rows(whoTable(), "2").map(nameOf)).toEqual(SEAT_JOBS.map((j) => SEAT_JOB_WORDS[j].label));
});

test("every picker lists the fleets, then the models; an empty fleet is an ordinary choice", () => {
  mount();
  const options = optionsOf(screen.getByRole("combobox", { name: "Backend Engineer's model" }));
  const labels = options.map((o) => o.textContent ?? "");
  expect(labels.slice(0, 3)).toEqual([
    expect.stringContaining("Fleet · default-fleet"),
    expect.stringContaining("Fleet · Claude Opus"),
    expect.stringContaining("Fleet · Opus in order"),
  ]);
  const empty = options[0]!;
  expect(empty.textContent).toContain("Default · No members yet");
  expect(empty.getAttribute("aria-disabled")).not.toBe("true");
  expect(options[1]!.textContent).toContain("3 of 3 members have room");
  // CODING RUNS ONLY ON A HARNESS THAT RUNS A SEAT'S EXECUTOR.
  expect(labels.some((l) => l.includes("codex-review"))).toBe(false);
});

test("a seat listing every job on one fleet reads that fleet, as the engine lists a seat's jobs", () => {
  const listed = ASSIGNMENTS.map((a) =>
    a.subject === "backend-engineer"
      ? {
          ...a,
          jobs: SEAT_JOBS.map((job) => ({
            job,
            target: { fleet: "claude-opus" },
            now: "claude-max",
            waiting_until: null,
          })),
        }
      : a,
  );
  mount(company({ assignments: listed }));
  const be = screen.getByRole("combobox", { name: "Backend Engineer's model" });
  expect(be.textContent).toContain("Fleet · Claude Opus");
  expect(be.textContent).not.toContain("Mixed");
});

test("a seat whose jobs differ reads Mixed, and its status is its worst job's", () => {
  mount();
  const qa = screen.getByRole("combobox", { name: "Quality Lead's model" });
  expect(qa.textContent).toContain("Mixed");
  expect(rowOf(whoTable(), "Quality Lead").textContent).toContain("Near its limit");
});

test("a seat on a fleet with no members reads No members yet, in no alarm's tone", () => {
  mount();
  const pm = rowOf(whoTable(), "Product Manager");
  expect(pm.textContent).toContain("No members yet");
  expect(pm.querySelector("[class*='danger']")).toBeNull();
  expect(screen.queryByText(/Every member of default-fleet/)).toBeNull();
});

test("a seat's pick asks once, then writes llm alone and clears the job fields", async () => {
  mount();
  fireEvent.click(screen.getByRole("combobox", { name: "Quality Lead's model" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: /Fleet · Claude Opus/ }));
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("Move Quality Lead to Fleet · Claude Opus?")).toBeDefined();
  expect(dialog.textContent).toContain(
    "It moves at its next turn; a running turn finishes where it is.",
  );
  expect(dialog.textContent).toContain("All jobs");
  // THE FALLBACK THE WRITE DROPS IS SAID.
  expect(dialog.textContent).toContain("claude-max");
  expect(rowOf(whoTable(), "Quality Lead").textContent).toContain("Was ");
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Move" }));
  });
  const saved = writes.docs.find((d) => !d.dryRun)!;
  const qa = saved.doc.roles.find((r: { handle: string }) => r.handle === "qa");
  expect(qa).toEqual({ handle: "qa", llm: "fleet:claude-opus" });
});

test("a job's pick writes that job's own field and nothing else", async () => {
  mount();
  expand(whoTable(), "Backend Engineer");
  fireEvent.click(
    screen.getByRole("combobox", { name: `${SEAT_JOB_WORDS.review.label} for Backend Engineer` }),
  );
  fireEvent.mouseDown(screen.getByRole("option", { name: /claude-sonnet/ }));
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain(SEAT_JOB_WORDS.review.label);
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Move" }));
  });
  const saved = writes.docs.find((d) => !d.dryRun)!;
  const be = saved.doc.roles.find((r: { handle: string }) => r.handle === "backend-engineer");
  expect(be).toEqual({
    handle: "backend-engineer",
    llm: "fleet:claude-opus",
    llm_review: "claude-sonnet",
  });
});

test("a helper's pick asks with the move's words, then writes its one setting", async () => {
  mount();
  fireEvent.click(screen.getByRole("combobox", { name: "Ask Crewlet's model" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: /Fleet · Claude Opus/ }));
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("Move Ask Crewlet to Fleet · Claude Opus?")).toBeDefined();
  expect(dialog.textContent).toContain("Model");
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Move" }));
  });
  await act(afterMicrotasks);
  await act(afterMicrotasks);
  expect(writes.patches).toHaveLength(1);
  expect(writes.patches[0]).toEqual({ assistant: { model: "fleet:claude-opus" } });
});

test("a helper's picker holds a model that cannot run a helper, with the engine's reason", () => {
  mount();
  const codex = optionsOf(screen.getByRole("combobox", { name: "Advisor's model" })).find((o) =>
    o.textContent?.includes("codex-review"),
  )!;
  expect(codex.getAttribute("aria-disabled")).toBe("true");
  expect(codex.textContent).toContain("cannot turn off");
});

test("a fleet with no room raises one alarm naming who waits and the first room", () => {
  const full: Fleet = {
    ...OPUS,
    members: OPUS.members.map((m, i) => ({ ...m, state: "limit", until: at(30 + i * 30) })),
  };
  const waiting = ASSIGNMENTS.map((a) =>
    a.subject === "backend-engineer" ? { ...a, now: null, waiting_until: at(30) } : a,
  );
  mount(company({ fleets: [DEFAULT_FLEET_EMPTY, full], assignments: waiting }));
  const title = screen.getByText("Every member of Claude Opus is at its limit");
  const alarm = title.closest("[role='alert'], [role='status'], .crewlet-callout")!;
  expect(alarm.textContent).toContain("Backend Engineer waits.");
  expect(alarm.textContent).toContain("(claude-max)");
  expect(alarm.textContent).toContain("Add a member, or move them to a weaker model on purpose.");
});

test("removing a member says who moves and who stays pinned, and writes the rest", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Remove claude-max from Claude Opus" }));
  const dialog = screen.getByRole("alertdialog");
  expect(dialog.textContent).toContain(
    "Advisor and Backend Engineer move to the other members at their next turn; a running turn finishes first.",
  );
  expect(dialog.textContent).toContain(
    "claude-max stays under Models with its sign-in, and nothing else changes.",
  );
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove from fleet" }));
  });
  expect(writes.fleets).toEqual([
    {
      method: "PUT",
      key: "claude-opus",
      body: {
        name: "Claude Opus",
        strategy: "balanced",
        members: ["claude-team", "claude-api"],
        threshold_points: 5,
      },
    },
  ]);
});

test("only a seat pinned to the member stays on it; one served through another fleet is neither", () => {
  const served: Fleet = {
    ...OPUS,
    members: [
      member("claude-max", { serving: ["backend-engineer", "pm", "release"] }),
      ...OPUS.members.slice(1),
    ],
  };
  const assignments: FleetAssignment[] = [
    ...ASSIGNMENTS.filter((a) => a.subject !== "pm"),
    {
      subject: "pm",
      kind: "seat",
      name: "Product Manager",
      target: { pinned: "claude-max" },
      now: "claude-max",
      waiting_until: null,
    },
    {
      subject: "release",
      kind: "seat",
      name: "Release Lead",
      target: { fleet: "opus-in-order" },
      now: "claude-max",
      waiting_until: null,
    },
  ];
  mount(company({ fleets: [DEFAULT_FLEET_EMPTY, served, ORDERED], assignments }));
  fireEvent.click(screen.getByRole("button", { name: "Remove claude-max from Claude Opus" }));
  const text = screen.getByRole("alertdialog").textContent ?? "";
  // ON THE MEMBER IS ITS WORK OR ITS NEXT TURN THERE, not its faces alone.
  expect(text).toContain(
    "Advisor and Backend Engineer move to the other members at their next turn",
  );
  expect(text).toContain("Product Manager is pinned to claude-max and stays on it.");
  expect(text).not.toContain("Release Lead");
});

test("a helper's picker holds a fleet with a member that cannot run a helper, naming it", () => {
  const mixed: Fleet = {
    ...ORDERED,
    key: "mixed",
    name: "Mixed",
    members: [member("codex-review")],
  };
  mount(company({ fleets: [DEFAULT_FLEET_EMPTY, OPUS, mixed] }));
  const option = optionsOf(screen.getByRole("combobox", { name: "Ask Crewlet's model" })).find(
    (o) => o.textContent?.includes("Fleet · Mixed"),
  )!;
  expect(option.getAttribute("aria-disabled")).toBe("true");
  expect(option.textContent).toContain(
    "codex-review: This model's tool keeps a tool Crewlet cannot turn off.",
  );
});

test("removing a fleet's last member names who waits until one joins", () => {
  const one: Fleet = {
    ...OPUS,
    members: [member("claude-max", { serving: ["backend-engineer"] })],
  };
  mount(company({ fleets: [DEFAULT_FLEET_EMPTY, one] }));
  fireEvent.click(screen.getByRole("button", { name: "Remove claude-max from Claude Opus" }));
  expect(screen.getByRole("alertdialog").textContent).toContain(
    "Advisor, Backend Engineer, and Quality Lead wait until a member joins; a running turn finishes first.",
  );
});

test("deleting a fleet moves who is on it to its class's fleets or models, or the default fleet", () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Actions for Claude Opus" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "Delete fleet" }));
  const dialog = screen.getByRole("alertdialog");
  expect(dialog.textContent).toContain(
    "Advisor, Backend Engineer, and Quality Lead run on this fleet.",
  );
  const labels = optionsOf(within(dialog).getByRole("combobox", { name: "Move them to" })).map(
    (o) => o.textContent,
  );
  expect(labels).toEqual([
    "Fleet · Opus in order",
    "claude-max",
    "claude-team",
    "claude-api",
    "Fleet · default-fleet",
  ]);
  expect(dialog.textContent).not.toContain("Leave unset");
});

test("Alt and Down moves a Sequential fleet's member, and writes the new order", async () => {
  mount();
  const ordered = rows(fleetsTable(), "2").filter((r) => r.textContent?.includes("claude-"));
  // OPUS IN ORDER'S FIRST MEMBER: the rows after Claude Opus's three.
  const first = ordered[3]!;
  await act(async () => {
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowDown", altKey: true });
  });
  expect(writes.fleets.at(-1)).toMatchObject({
    key: "opus-in-order",
    body: { members: ["claude-api", "claude-max"], strategy: "sequential" },
  });
});

test("a member reads every fleet and pick, with no write control", () => {
  mount(company({ admin: false }));
  expect(screen.queryByRole("button", { name: "Create fleet" })).toBeNull();
  expect(screen.queryByRole("button", { name: /Add members to/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /Remove .* from/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /Actions for/ })).toBeNull();
  expect(
    screen.getByRole("heading", { name: "Who runs on which model" }).closest("section")!
      .textContent,
  ).toContain("The model or fleet each helper and seat runs on.");
  for (const picker of within(whoTable()).getAllByRole("combobox"))
    expect(picker.getAttribute("aria-disabled") === "true" || picker.hasAttribute("disabled")).toBe(
      true,
    );
});

test("a status says Running only for work in flight, and Idle with where the next turn goes", () => {
  const assignments: FleetAssignment[] = ASSIGNMENTS.map((a) =>
    a.subject === "backend-engineer"
      ? { ...a, now_kind: "running" as const }
      : a.subject === "advisor"
        ? { ...a, now_kind: "next" as const }
        : a,
  );
  mount(company({ assignments }));
  const be = rowOf(whoTable(), "Backend Engineer");
  expect(be.textContent).toContain("Running");
  expect(within(be).getByLabelText("Now on claude-max, in Claude Opus")).toBeTruthy();
  const advisor = rowOf(whoTable(), "Advisor");
  expect(advisor.textContent).toContain("Idle");
  expect(advisor.textContent).not.toContain("Running");
  expect(
    within(advisor).getByLabelText("Idle: its next turn goes to claude-max, in Claude Opus"),
  ).toBeTruthy();
  // A PINNED HELPER WITH NOTHING IN FLIGHT IS IDLE ON ITS MODEL.
  expect(
    within(rowOf(whoTable(), "Ask Crewlet")).getByLabelText(
      "Idle: its next turn runs on claude-sonnet",
    ),
  ).toBeTruthy();
});

test("a seat whose jobs differ reads Running while any job's work is in flight", () => {
  const assignments: FleetAssignment[] = ASSIGNMENTS.map((a) =>
    a.subject === "qa"
      ? {
          ...a,
          jobs: SEAT_JOBS.map((job) =>
            job === "review"
              ? {
                  job,
                  target: { pinned: "claude-sonnet" },
                  now: "claude-sonnet",
                  now_kind: "running" as const,
                  waiting_until: null,
                }
              : {
                  job,
                  target: { pinned: "claude-api" },
                  now: "claude-api",
                  now_kind: "next" as const,
                  waiting_until: null,
                },
          ),
        }
      : a,
  );
  mount(company({ assignments }));
  const qa = rowOf(whoTable(), "Quality Lead");
  expect(qa.textContent).toContain("Running");
  expect(qa.textContent).not.toContain("Idle");
});

test("Serving now shows only the faces of work in flight, and Nobody while nothing runs", () => {
  const idle: Fleet = { ...OPUS, members: OPUS.members.map((m) => ({ ...m, serving: [] })) };
  mount(company({ fleets: [DEFAULT_FLEET_EMPTY, idle, ORDERED] }));
  // EVERY ASSIGNMENT NAMES A NEXT MEMBER, AND STILL NOBODY IS SERVED.
  expect(rowOf(fleetsTable(), "Claude Opus").textContent).toContain("Nobody");
  expect(rowOf(fleetsTable(), "claude-max", "2").textContent).toContain("Nobody");
});
