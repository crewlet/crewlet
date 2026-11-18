/**
 * Settings › Advisor. The invariants worth breaking a build over: the owner's
 * findings are listed and the engine's never read as the owner's to-do; a change
 * is previewed from the engine's own dry run of exactly the entity it writes,
 * with only the warnings the change adds; nothing is written until Apply, and
 * Apply writes that entity under its own tag with the change's summary; a
 * refused write is said, never swallowed; and a reader who may not write
 * cannot apply.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { overrideChunkForTest } from "~/app/lazyScreen.ts";
import { Router } from "~/app/router.tsx";
import type { AdvisorAnswer, AdvisorFinding } from "~/protocol/advisor.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Advisor } from "./Advisor.tsx";
import { AssistantControlContext, type AssistantControl } from "~/app/assistant/control.ts";

/** An assistant the reader may ask, recording what it was asked. */
const asked = vi.hoisted(() => vi.fn());
const ASSISTANT: AssistantControl = {
  open: false,
  unavailable: null,
  toggle: () => {},
  ask: (question) => asked(question),
};
const NO_ASSISTANT: AssistantControl = {
  ...ASSISTANT,
  unavailable: { reason: "checking", short: "Checking whether you can ask.", title: "", body: "" },
};

const getEntity = vi.hoisted(() => vi.fn());
const dryRunEntity = vi.hoisted(() => vi.fn());
const putEntity = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getEntity,
  dryRunEntity,
  putEntity,
}));

const access = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: access,
}));

const HEALTH: AdvisorFinding = {
  class: "grant.health_unheld",
  severity: "costs",
  actor: "owner",
  source: "static",
  area: "access",
  subject: { kind: "company" },
  title: "No agent watches the engine's own alarms.",
  why: "An alarm that fires on a server reaches nobody.",
  path: "roles[0].health_tools",
  evidence: [],
  fix: {
    kind: "config_change",
    sentence: "Let one agent watch the engine's health and wake on its alarms.",
    unchanged: "No agent gets any other new access.",
    options: [
      {
        label: "Let PM watch the engine's health",
        change: { entity: "roles", id: "pm", set: { health_tools: true }, summary: "Let pm watch" },
      },
      {
        label: "Let SRE watch the engine's health",
        change: {
          entity: "roles",
          id: "sre",
          set: { health_tools: true },
          summary: "Let sre watch the engine's health",
        },
      },
    ],
  },
};

const ENGINE: AdvisorFinding = {
  ...HEALTH,
  class: "wake.noop_share",
  actor: "engine",
  source: "evidence",
  title: "Many turns changed nothing.",
  fix: { kind: "none", sentence: "The engine is reducing this.", options: [] },
};

/** The analyst, off: what a company that names no model for it reads. */
const ANALYST_OFF = {
  on: false,
  cadence: "daily",
  hour: 6,
  ceiling: 200000,
  stale: false,
  findings: [],
  set_aside: 0,
};

const SRE = { name: "SRE", handle: "sre", llm: "default" };
const VALID = { kind: "valid", baseRevisionId: "r1", warnings: [], derived: null };

/** The go-live review: one agent, connected to GitHub, whose box a run has read. */
const REVIEW = {
  agents: [
    {
      seat: "sre",
      name: "SRE",
      access: {
        integrations: [
          {
            kind: "github",
            lane: "github",
            on: true,
            permissions: { pull_requests: "write", contents: "read" },
            permission_names: { pull_requests: "Pull requests" },
          },
        ],
        mcp: [],
        boxes: [
          {
            cell: "container",
            memory_bytes: 4 * 2 ** 30,
            observed: true,
            browser: "chromium",
          },
        ],
      },
    },
  ],
};

async function mount(
  answer: AdvisorAnswer,
  assistant: AssistantControl = NO_ASSISTANT,
  review: unknown = REVIEW,
) {
  location.hash = "#/settings/advisor";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "viewer"
        ? ADMIN_VIEWER
        : what === "go_live_review"
          ? review
          : what === "advisor_analysis"
            ? ANALYST_OFF
            : answer,
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <AssistantControlContext.Provider value={assistant}>
              <Advisor />
            </AssistantControlContext.Provider>
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
}

/** Opens the health finding's preview and picks the SRE option. */
async function reviewSre() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Review the change" }));
    await settled();
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("radio", { name: /Let SRE watch/ }));
    await settled();
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  access.mockReturnValue({ can: true });
  getEntity.mockResolvedValue({ kind: "entity", entity: SRE, etag: '"roles-abc"' });
  dryRunEntity.mockResolvedValue(VALID);
  putEntity.mockResolvedValue({
    kind: "saved",
    revisionId: "r2",
    epoch: 3,
    warnings: [],
    derived: null,
  });
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

test("the owner's findings are listed, and the engine's never as the owner's to-do", async () => {
  await mount({ revision: "r1", findings: [HEALTH, ENGINE], unread: [] });
  expect(screen.getByText(HEALTH.title)).toBeTruthy();
  expect(screen.getByText("What to change")).toBeTruthy();
  expect(screen.getByText("What the engine is handling")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Review the change" })).toHaveLength(1);
});

test("a company with nothing to change says so", async () => {
  await mount({ revision: "r1", findings: [], unread: [] });
  expect(screen.getByText("Nothing to change")).toBeTruthy();
});

test("the preview is the engine's dry run of the entity the change writes", async () => {
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  expect(getEntity).toHaveBeenCalledWith("roles", "sre", expect.anything());
  const sent = dryRunEntity.mock.calls.map((call) => call[2] as Record<string, unknown>);
  expect(sent).toContainEqual(SRE);
  expect(sent).toContainEqual({ ...SRE, health_tools: true });
  expect(dryRunEntity.mock.calls.every((call) => call[3] === '"roles-abc"')).toBe(true);
  expect(screen.getByRole("group", { name: "The change" }).textContent).toMatch(
    /health_tools: not set[\s\S]*health_tools: on/,
  );
  expect(putEntity).not.toHaveBeenCalled();
});

test("only the warnings the change adds are shown", async () => {
  const old = { kind: "advisory", path: "roles", message: "Already there." };
  const added = { kind: "advisory", path: "roles[1]", message: "The change adds this." };
  dryRunEntity
    .mockResolvedValueOnce({ ...VALID, warnings: [old] })
    .mockResolvedValueOnce({ ...VALID, warnings: [old, added] });
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  expect(screen.getByText("The change adds this.")).toBeTruthy();
  expect(screen.queryByText("Already there.")).toBeNull();
});

test("apply writes that entity under its own tag, with the change's summary", async () => {
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await settled();
  });
  expect(putEntity).toHaveBeenCalledOnce();
  const [kind, id, body, etag, summary] = putEntity.mock.calls[0] ?? [];
  expect([kind, id, etag, summary]).toEqual([
    "roles",
    "sre",
    '"roles-abc"',
    "Let sre watch the engine's health",
  ]);
  expect(body).toEqual({ ...SRE, health_tools: true });
});

test("a refused write is said, and the dialog stays open", async () => {
  putEntity.mockResolvedValue({ kind: "conflict", reason: "stale", currentRevisionId: "r9" });
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await settled();
  });
  expect(screen.getByText("Couldn't save that")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
});

test("a reader who may not write cannot apply", async () => {
  access.mockReturnValue({
    can: false,
    block: "grant",
    reason: "Only an administrator can change this.",
  });
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText("Only an administrator can change this.")).toBeTruthy();
});

const BACKUP: AdvisorFinding = {
  ...HEALTH,
  class: "durability.no_verified_backup",
  severity: "blocks",
  source: "evidence",
  area: "durability",
  title: "Your company has no recent verified backup.",
  path: "",
  fix: {
    kind: "open_page",
    page: "backups",
    sentence: "Take a backup now, then schedule backups that are copied off this server.",
    options: [],
  },
};

// A STEP TAKEN ON A PAGE HERE IS THAT PAGE'S GESTURE: the backup finding's row
// opens Backups & retention's own Take a backup, where its sentence used to
// send a person to a command for it.
test("a step taken on a page of the dashboard says what to do and opens its gesture", async () => {
  await mount({ revision: "r1", findings: [BACKUP], unread: [] });
  expect(screen.getByText(BACKUP.fix.sentence)).toBeTruthy();
  const link = screen.getByRole("link", { name: "Take a backup" });
  expect(link.getAttribute("href")).toBe("#/settings/backups?take=backup");
  expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull();
});

// THE ROW NAMES WHAT IT IS ABOUT ONCE. The engine writes a seat's finding with
// the seat in its title, and the name again under it said one fact twice on
// every row; a title that names nobody still gets its subject under it.
test("a row names its subject only where its title does not", async () => {
  const learns: AdvisorFinding = {
    ...HEALTH,
    class: "learning.auxiliary_model",
    severity: "note",
    subject: { kind: "seat", seat: "ceo", name: "Agent CEO" },
    title: "Agent CEO learns on the model it works on.",
  };
  const unnamed: AdvisorFinding = {
    ...HEALTH,
    class: "schedule.frequent",
    subject: { kind: "schedule", schedule: "digest", name: "Daily digest" },
    title: "A schedule runs twelve or more times a day.",
  };
  await mount({ revision: "r1", findings: [learns, unnamed], unread: [] });
  expect(screen.getAllByText(/Agent CEO/)).toHaveLength(1);
  expect(screen.getByText("Daily digest")).toBeTruthy();
});

// THE ENGINE'S WORDS ARE DRAWN AS ITS SENTENCES ARE EVERYWHERE: a value it
// wrote in backticks is code, never the punctuation it travelled in.
test("a value in backticks in a finding is drawn as code", async () => {
  const ticked: AdvisorFinding = {
    ...BACKUP,
    why: "Nothing runs `crewlet backup` for you.",
  };
  await mount({ revision: "r1", findings: [ticked], unread: [] });
  fireEvent.click(screen.getByRole("button", { name: /Details/ }));
  expect(document.body.textContent).not.toContain("`");
  expect(screen.getByText("crewlet backup").tagName).toBe("CODE");
});

// AGENTS TOLD THE SAME ARE ONE ENTRY, headed by every name: an entry each drew
// one box sentence six times over, and the agent told something different was
// the one a reader had to find.
test("the go-live review says what several agents are told alike once", async () => {
  const box = { cell: "direct", memory_bytes: 8 * 2 ** 30, observed: true };
  const alike = { integrations: [], mcp: [], boxes: [box] };
  const review = {
    agents: [
      { seat: "ceo", name: "Agent CEO", access: alike },
      { seat: "pm", name: "Agent PM", access: { ...alike } },
      { seat: "ai", name: "Agent AI", access: { ...alike, boxes: [{ ...box, observed: false }] } },
      { seat: "cto", name: "Agent CTO", access: { ...alike } },
    ],
  };
  await mount({ revision: "r1", findings: [], unread: [] }, NO_ASSISTANT, review);
  const many = screen.getByRole("region", {
    name: "What Agent CEO, Agent PM, and Agent CTO can do",
  });
  expect(many).toBeTruthy();
  expect(screen.getByRole("region", { name: "What Agent AI can do" })).toBeTruthy();
  expect(screen.getAllByText("Runs in")).toHaveLength(2);
});

test("two findings of one class are one row, and two seats sharing a name stay two", async () => {
  const pm = { ...HEALTH, subject: { kind: "seat", seat: "pm", name: "Engineer" } };
  const qa = { ...HEALTH, subject: { kind: "seat", seat: "qa", name: "Engineer" } };
  await mount({ revision: "r1", findings: [pm, qa], unread: [] });
  const row = screen
    .getByText(
      (_, el) =>
        el?.className === "crewlet-attention-list__line" && el.textContent!.endsWith(HEALTH.title),
    )
    .closest("li")!;
  expect(row.querySelector(".crewlet-attention-list__subject")?.textContent).toBe("2 agents");
  expect(row.querySelector(".crewlet-attention-list__items")?.textContent).toBe(
    "Engineer (pm), Engineer (qa)",
  );
});

const APPROVAL: AdvisorFinding = {
  class: "integration.approval_required",
  severity: "blocks",
  actor: "owner",
  source: "reconcile",
  area: "integrations",
  subject: { kind: "integration", name: "GitHub" },
  title: "GitHub is waiting for you to approve the app's permissions.",
  why: "Until you do, the agents' GitHub tools stay off.",
  path: "integrations.github",
  evidence: [],
  fix: {
    kind: "external_step",
    sentence: "Accept the new permissions on GitHub.",
    options: [],
    setting: "github",
    url: "https://github.com/settings/installations/1",
  },
};

test("a step outside the engine says what to do and links where to do it", async () => {
  await mount({ revision: "r1", findings: [APPROVAL], unread: [] });
  expect(screen.getByText("Accept the new permissions on GitHub.")).toBeTruthy();
  const link = screen.getByRole("link", { name: /Open GitHub/ });
  expect(link.getAttribute("href")).toBe("https://github.com/settings/installations/1");
  expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull();
});

test("integrations the server couldn't read are said, so a short list is not all clear", async () => {
  await mount({ revision: "r1", findings: [], unread: ["integrations"] });
  expect(screen.getByText("Some of the setup couldn't be read")).toBeTruthy();
  expect(screen.getByText(/integrations' last check couldn't be read/)).toBeTruthy();
});

// EVERY UNREAD SOURCE IS SAID IN ITS OWN WORDS, and while any is unread an empty
// owner list never reads "Nothing to change": the source may hold the blocker.
test.each([
  ["work", /last 7 days of work couldn't be read/],
  ["ledger", /agents' turns couldn't be read/],
  ["alarms", /whether a backup is missing can't be said yet/],
  ["future_source", /couldn't read future_source/],
])("an unread %s source is said, and nothing reads as all clear", async (key, words) => {
  await mount({ revision: "r1", findings: [ENGINE], unread: [key] });
  expect(screen.getByText("Some of the setup couldn't be read")).toBeTruthy();
  expect(screen.getByText(words)).toBeTruthy();
  expect(screen.queryByText("Nothing to change")).toBeNull();
});

/** The go-live check's steps as drawn, label and state. */
function goLive() {
  const list = screen.getByRole("list", { name: "Go-live check" });
  return [...list.querySelectorAll("li")].map((li) => li.textContent ?? "");
}

test("the go-live check counts what is left by what it costs", async () => {
  const blocker = { ...HEALTH, severity: "blocks" as const, path: "integrations.datadog" };
  await mount({ revision: "r1", findings: [blocker, HEALTH, ENGINE], unread: [] });
  const steps = goLive();
  expect(steps[0]).toMatch(/Fix what blocks the company \(1\)/);
  expect(steps[0]).toMatch(/To do/);
  expect(steps[1]).toMatch(/Look at what costs it \(1\)/);
  expect(steps[2]).toMatch(/Nothing else to read/);
  expect(steps[2]).toMatch(/Done/);
});

test("a source the answer could not read leaves its steps unsaid, never done", async () => {
  await mount({ revision: "r1", findings: [], unread: ["integrations"] });
  const steps = goLive();
  expect(steps[0]).toMatch(/Nothing that could be read blocks the company/);
  expect(steps[0]).toMatch(/Can't say yet/);
  expect(steps[0]).not.toMatch(/Done/);
});

const RUBBER_STAMP: AdvisorFinding = {
  ...HEALTH,
  class: "approval.rubber_stamp",
  severity: "note",
  source: "evidence",
  area: "approvals",
  subject: { kind: "seat", seat: "pm", name: "PM" },
  title: "owner chose pm's recommendation in 19 of 20 decisions.",
  path: "",
  fix: {
    kind: "draft_with_assistant",
    sentence: "Draft a policy with Ask Crewlet, and review it before it is saved.",
    options: [],
    prompt: "Read those decisions and draft a policy. Show me the change before making it.",
  },
};

test("a policy to draft is asked of Ask Crewlet, in the engine's words", async () => {
  await mount({ revision: "r1", findings: [RUBBER_STAMP], unread: [] }, ASSISTANT);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Draft with Ask Crewlet" }));
    await settled();
  });
  expect(asked).toHaveBeenCalledWith(RUBBER_STAMP.fix.prompt);
  expect(putEntity).not.toHaveBeenCalled();
});

test("a reader who may not ask is told why, and offered no draft", async () => {
  await mount({ revision: "r1", findings: [RUBBER_STAMP], unread: [] });
  expect(screen.queryByRole("button", { name: "Draft with Ask Crewlet" })).toBeNull();
  expect(screen.getByText("Checking whether you can ask.")).toBeTruthy();
});

// THE GO-LIVE REVIEW SAYS WHAT EACH AGENT CAN DO, as its turns are told: the
// vendors it acts at and what it may write there, and the box its runs use.
test("the go-live review says what each agent can do and where it runs", async () => {
  await mount({ revision: "r1", findings: [], unread: [] });
  const sre = await screen.findByRole("region", { name: "What SRE can do" });
  expect(sre.textContent).toContain("github (Pull requests: write)");
  expect(sre.textContent).toContain(
    "A container on the engine host, 4 GB of memory, with a browser (chromium) and no container runtime.",
  );
  expect(sre.textContent).toContain("No tool servers");
});

/** An atlas finding as a holder of the atlas app is answered it. */
const UNOWNED: AdvisorFinding = {
  class: "atlas.unowned",
  severity: "note",
  actor: "owner",
  source: "atlas",
  area: "atlas",
  subject: { kind: "atlas" },
  title: "2 services have no owner.",
  why: "Nobody is named as answering for a service the company runs.",
  path: "",
  evidence: [{ key: "entries", value: 2 }],
  entries: [
    {
      id: "0195a1c0-0000-8000-8000-00000000c0de",
      kind: "service",
      key: "checkout-api",
      name: "Checkout API",
    },
    {
      id: "0195a1c0-0000-8000-8000-00000000b111",
      kind: "service",
      key: "billing",
      name: "Billing",
    },
  ],
  fix: {
    kind: "atlas_act",
    sentence: "Name the seat or the unit that answers for each.",
    options: [],
    act: { tool: "atlas_set_owner", asks: "owner" },
  },
};

test("an atlas finding opens its fix from the atlas's own chunk, and writes no settings", async () => {
  const restore = overrideChunkForTest("atlas", async () => ({
    AdvisorAtlasFix: ({ finding }: { finding: AdvisorFinding }) => (
      <div role="dialog" aria-label="atlas fix">
        {(finding.entries ?? []).map((entry) => entry.name).join(", ")}
      </div>
    ),
  }));
  try {
    await mount({ revision: "r1", findings: [UNOWNED], unread: [] });
    expect(screen.getByText("2 services have no owner.")).toBeTruthy();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Review the fix" }));
      await settled();
    });
    expect(screen.getByRole("dialog", { name: "atlas fix" }).textContent).toBe(
      "Checkout API, Billing",
    );
    expect(getEntity).not.toHaveBeenCalled();
    expect(putEntity).not.toHaveBeenCalled();
  } finally {
    restore();
  }
});

test("an atlas finding answered without its entries offers nothing to send", async () => {
  await mount({
    revision: "r1",
    findings: [
      {
        ...UNOWNED,
        entries: undefined,
        fix: {
          kind: "none",
          sentence:
            "Reading the atlas takes the Atlas permission. Somebody who holds it sees which, and can fix it.",
          options: [],
        },
      },
    ],
    unread: [],
  });
  expect(screen.getByText("2 services have no owner.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Review the fix" })).toBeNull();
  expect(screen.queryByText(/Checkout API/)).toBeNull();
});

test("an atlas the server couldn't read is said", async () => {
  await mount({ revision: "r1", findings: [], unread: ["atlas", "atlas_sources"] });
  expect(screen.getByText(/The atlas couldn't be read/)).toBeTruthy();
  expect(screen.getByText(/The atlas's sources couldn't be read/)).toBeTruthy();
  expect(screen.queryByText("Nothing to change")).toBeNull();
});

// THE READ IN FLIGHT IS A SKELETON: the screen was its one sentence over an
// empty page, which reads as a setup with nothing to change.
test("the advisor in flight is a skeleton, and no claim that nothing needs changing", async () => {
  await mount(new Promise(() => {}) as never);
  expect(document.querySelector(".crewlet-skeleton")).not.toBeNull();
  expect(screen.queryByText("Nothing to change")).toBeNull();
});

test("a lone finding about one agent names it once, before its title", async () => {
  const one = {
    ...HEALTH,
    names: ["Engineer"],
    subject: { kind: "seat", seat: "pm", name: "Engineer" },
  };
  await mount({ revision: "r1", findings: [one], unread: [] });
  const row = screen
    .getByText(
      (_, el) =>
        el?.className === "crewlet-attention-list__line" && el.textContent!.endsWith(HEALTH.title),
    )
    .closest("li")!;
  expect(row.querySelector(".crewlet-attention-list__subject")?.textContent).toBe("Engineer");
  expect(row.querySelector(".crewlet-attention-list__meta")).toBeNull();
});

/** One seat's learning finding, as the engine raises it for each seat alike. */
function learns(seat: string, name: string): AdvisorFinding {
  return {
    ...HEALTH,
    class: "learning.auxiliary_model",
    severity: "note",
    subject: { kind: "seat", seat, name },
    title: "Agents that learn on their main model.",
    why: "Learning costs a share of its spend.",
    path: `roles[0].llm_auxiliary`,
  };
}

// A CLASS RAISED FOR NINE SEATS IS ONE THING TO DO: one row naming them, and
// one count in the go-live check.
test("the go-live check counts a class said for several seats once", async () => {
  const seats = ["a", "b", "c", "d", "e", "f", "g", "h", "i"].map((s) =>
    learns(s, s.toUpperCase()),
  );
  await mount({ revision: "r1", findings: seats, unread: [] });
  expect(goLive()[2]).toMatch(/Read what is worth knowing \(1\)/);
  expect(screen.getAllByText(/Agents that learn on their main model/)).toHaveLength(1);
});

const BOTTLENECK: AdvisorFinding = {
  ...HEALTH,
  class: "work.bottleneck",
  source: "evidence",
  area: "work",
  subject: { kind: "seat", seat: "dev", name: "Dev" },
  title: "Open work waits on one item.",
  why: "Everything waiting on it moves only when it does.",
  item: "ENG-1: 12 items wait on it",
  path: "",
  records: [
    { kind: "item", id: "ENG-1" },
    { kind: "turn", id: "0123456789abcdef" },
    { kind: "change", id: "https://github.com/acme/web/pull/42" },
    { kind: "change", id: "web!7" },
    { kind: "comment", id: "c-1" },
  ],
  fix: {
    kind: "open_page",
    page: "item",
    target: "ENG-1",
    sentence: "Open the item: move it first.",
    options: [],
  },
};

// A FINDING'S RECORDS ARE LINKS A PERSON OPENS, each to its own page, and its
// page fix opens on its target.
test("a finding names its records as links and opens the page it names", async () => {
  await mount({ revision: "r1", findings: [BOTTLENECK], unread: [] });
  expect(screen.getByRole("link", { name: "Open ENG-1" }).getAttribute("href")).toBe(
    "#/worklet/ENG-1",
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Details/ }));
    await settled();
  });
  const links = Object.fromEntries(
    screen
      .getAllByRole("link", { hidden: true })
      .map((a) => [a.textContent ?? "", a.getAttribute("href") ?? ""]),
  );
  expect(links["ENG-1"]).toBe("#/worklet/ENG-1");
  expect(links["turn 01234567"]).toBe("#/live/turns/0123456789abcdef");
  const pull = screen
    .getAllByRole("link", { hidden: true })
    .find((a) => a.getAttribute("href") === "https://github.com/acme/web/pull/42");
  expect(pull?.textContent).toMatch(/^acme\/web#42/);
  expect(links["web!7"]).toBe("#/gitlet/repos/web/changes/7");
  expect(screen.getByText(/and 1 comment/)).toBeTruthy();
});
