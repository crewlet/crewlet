/**
 * The Change kind dialog.
 *
 * What these protect: the fields the change removes are named before it is
 * recorded, with the credential ones called out as unrecoverable; a human seat
 * can be made without a contact identity, as the engine admits one, but not
 * out of the Datadog fallback without a replacement; the consequences say what
 * the seat becomes; and a schedule the change strands is named first.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { AgentRow, ChartRead, ChartSeat } from "~/protocol/index.ts";
import { ChangeKindDialog } from "./ChangeKindDialog.tsx";
import { locate } from "./model/draft.ts";
import { getPath } from "./model/json.ts";
import type { BuilderState } from "./model/reducer.ts";
import { chartOf, fixtureChart, fixtureSettings, strippedChart } from "./model/testkit.ts";
import { renderInBuilder, type HarnessOptions } from "./viewTestkit.tsx";
import { checkedEdit, record } from "./testState.ts";
import { pick } from "~/testing.tsx";

afterEach(cleanup);

function open(state: BuilderState, key: string, options: HarnessOptions = {}) {
  const onClose = vi.fn();
  const view = renderInBuilder(
    state,
    <ChangeKindDialog nodeKey={key} onClose={onClose} />,
    options,
  );
  return { ...view, onClose };
}

const confirm = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;
const toHuman = () => confirm("Change to human seat");

/** The fixture chart with Dev's seat replaced by `dev`. */
function withDev(dev: Omit<ChartSeat, "handle" | "unit">): ChartRead {
  const chart = fixtureChart();
  chart.seats = chart.seats.map((s) =>
    s.handle === "dev" ? { handle: "dev", unit: "engineering", ...dev } : s,
  );
  return chart;
}

/** Dev carrying what a human seat may not: a model, a budget, and its tools' credentials. */
function withFields(): ChartRead {
  return withDev({
    name: "Dev",
    goal: "Build",
    behavioral_guidelines: ["Be kind"],
    runtime: {
      llm: "fast",
      token_budget: 10,
      mcp_env: { tracker: { TOKEN: "__redacted__" } },
      slack: { bot_token: "${DEV_SLACK}", signing_secret: "${DEV_SIGN}", channel: "C1" },
      github: { tier: "review", app_slug: "acme-dev", private_key: "${DEV_KEY}" },
    },
  });
}

const devData = (state: BuilderState) => {
  const seat = locate(state.draft, "seat:dev");
  return seat?.kind === "seat" ? seat.node.data : undefined;
};

test("the fields the change removes are named, the credential ones as gone for good", () => {
  const view = open(checkedEdit(withFields()), "seat:dev");
  expect(screen.getByText("llm")).toBeDefined();
  expect(screen.getByText("token_budget")).toBeDefined();
  expect(screen.getByText("behavioral_guidelines")).toBeDefined();
  for (const credential of ["slack", "mcp_env", "github"]) {
    const item = screen.getByText(credential).closest("li") as HTMLElement;
    expect(item.textContent).toContain("holds credentials, which are gone for good");
  }
  // The control: a field that is not a credential is not called one.
  expect(screen.getByText("llm").closest("li")!.textContent).not.toContain("credentials");
  expect(
    screen.getByText("Dev stops running. Its memory is kept but unused while it is a human seat."),
  ).toBeDefined();
  expect(view.container.ownerDocument.body.innerHTML).not.toContain("__redacted__");
});

// Stripping a field tears nothing down at a vendor: the seat's app and bot
// stay, and so do the entries its removed fields referenced, exactly as for a
// deleted seat. A seat this draft created was never saved and has none.
test("what stays at the vendors and in the secret store is named, and nothing for a new seat", () => {
  const view = open(checkedEdit(withFields()), "seat:dev");
  const entry = screen.getByText(/the GitHub App acme-dev/);
  expect(entry.textContent).toContain("its Slack app");
  for (const name of ["DEV_KEY", "DEV_SLACK", "DEV_SIGN"])
    expect(entry.textContent).toContain(name);
  expect(screen.getByText(/These stay until you decommission them./)).toBeDefined();
  expect(view.container.ownerDocument.body.innerHTML).not.toContain("__redacted__");
  cleanup();

  const added = record(checkedEdit(), {
    type: "addSeat",
    key: "new:qa",
    placement: { parent: "unit:sales" },
    data: {
      handle: "qa",
      name: "QA",
      runtime: { mcp_env: { tracker: { TOKEN: "${QA_TRACKER}" } }, slack: { channel: "C9" } },
    },
  });
  open(added, "new:qa");
  expect(screen.queryByText(/These stay until you decommission them/)).toBeNull();
  // Never saved, so it never ran: nothing stops and no memory is kept.
  expect(
    screen.getByText(
      "QA will not run: a human seat is a person in the chart, with no agent behind it.",
    ),
  ).toBeDefined();
});

// THE ENGINE ADMITS A HUMAN SEAT WITH NO CONTACT IDENTITY — a person who works
// only through the dashboard has none to give — so the dialog must not demand
// one, and a change made without one writes no contact block at all.
test("a human seat is made without a contact identity, and writes no contact block", () => {
  const view = open(checkedEdit(withFields()), "seat:dev");
  expect(toHuman().disabled).toBe(false);
  fireEvent.click(toHuman());
  expect(view.state().log.ops[0]).toMatchObject({
    type: "changeKind",
    target: "seat:dev",
    after: "human",
  });
  expect(view.state().log.ops[0]).not.toHaveProperty("contact");
  const data = devData(view.state());
  expect(data).toMatchObject({ handle: "dev", name: "Dev", kind: "human", goal: "Build" });
  expect(getPath(data, ["runtime", "contact"])).toBeUndefined();
  expect(getPath(data, ["runtime", "llm"])).toBeUndefined();
  expect(data).not.toHaveProperty("behavioral_guidelines");
});

test("a contact identity given to a human seat is recorded in one operation", () => {
  const view = open(checkedEdit(withFields()), "seat:dev");
  pick(screen.getByLabelText("Contact"), "GitHub login");
  fireEvent.change(screen.getByLabelText(/^GitHub login/), { target: { value: "dev" } });
  expect(toHuman().disabled).toBe(false);
  fireEvent.click(toHuman());
  expect(view.state().log.ops).toHaveLength(1);
  expect(view.state().log.ops[0]).toMatchObject({
    type: "changeKind",
    target: "seat:dev",
    after: "human",
    contact: { github_login: "dev" },
  });
  expect(devData(view.state())).toEqual({
    handle: "dev",
    name: "Dev",
    kind: "human",
    goal: "Build",
    runtime: { contact: { github_login: "dev" } },
  });
  expect(view.onClose).toHaveBeenCalledTimes(1);
});

test("the Datadog fallback cannot become a human seat without a replacement", () => {
  const view = open(checkedEdit(), "seat:sre");
  pick(screen.getByLabelText("Contact"), "GitHub login");
  fireEvent.change(screen.getByLabelText(/^GitHub login/), { target: { value: "sre" } });
  expect(toHuman().disabled).toBe(true);
  expect(screen.getByText(/SRE is the Datadog fallback/)).toBeDefined();
  pick(screen.getByLabelText("Datadog fallback"), "Dev (@dev)");
  fireEvent.click(toHuman());
  expect(getPath(view.state().draft.company, ["integrations", "datadog", "route_to"])).toBe("dev");
});

// A switched-off Datadog wakes nobody and the engine requires no fallback of
// it, so the seat its route_to names changes kind like any other.
test("a seat named by a switched-off Datadog becomes human with no replacement", () => {
  const settings = fixtureSettings();
  settings.integrations = {
    ...settings.integrations,
    datadog: { enabled: false, route_to: "sre" },
  };
  const view = open(checkedEdit(fixtureChart(), {}, settings), "seat:sre");
  expect(screen.queryByLabelText("Datadog fallback")).toBeNull();
  pick(screen.getByLabelText("Contact"), "GitHub login");
  fireEvent.change(screen.getByLabelText(/^GitHub login/), { target: { value: "sre" } });
  fireEvent.click(toHuman());
  expect(view.state().log.ops[0]).toMatchObject({ type: "changeKind", target: "seat:sre" });
});

// The change stays unavailable until a fallback is chosen, so the company's
// only agent seat would otherwise leave a button that never becomes available
// beside a picker with nothing in it.
test("the company's only agent seat is told why it cannot become human, not offered an empty choice", () => {
  const chart = chartOf({
    seats: [
      { handle: "only", name: "Only" },
      { handle: "pat", name: "Pat", kind: "human", runtime: { contact: { github_login: "pat" } } },
    ],
  });
  const settings = { name: "X", integrations: { datadog: { enabled: true, route_to: "only" } } };
  open(checkedEdit(chart, {}, settings), "seat:only");
  expect(screen.queryByLabelText("Datadog fallback")).toBeNull();
  expect(
    screen.getByText(
      /It is the company's only agent seat, so add another before changing this one, or disconnect Datadog/,
    ),
  ).toBeDefined();
  pick(screen.getByLabelText("Contact"), "GitHub login");
  fireEvent.change(screen.getByLabelText(/^GitHub login/), { target: { value: "only" } });
  expect(toHuman().disabled).toBe(true);
});

test("a schedule the change strands, and the seat's work in flight, are said first", () => {
  const chart = chartOf({
    units: [
      {
        key: "team",
        name: "Team",
        lead: "lead",
        runtime: {
          schedules: [{ name: "standup", cron: "0 9 * * *", task: "Standup", target: "lead" }],
        },
      },
    ],
    seats: [
      { handle: "lead", name: "Lead", unit: "team" },
      { handle: "member", name: "Member", unit: "team" },
    ],
  });
  const agents: AgentRow[] = [{ id: "1", role: "Lead", handle: "lead", state: "working" }];
  open(checkedEdit(chart), "seat:lead", { agents });
  expect(screen.getByText(/Schedule standup on Team would have no runner/)).toBeDefined();
  expect(screen.getByText(/Lead is working now/)).toBeDefined();
});

test("a human seat becomes an agent seat again, losing the fields an agent may not carry", () => {
  const chart = withDev({
    name: "Dev",
    kind: "human",
    runtime: { contact: { github_login: "dev" }, availability: "Weekdays" },
  });
  const view = open(checkedEdit(chart), "seat:dev");
  expect(
    screen.getByText(
      "Dev starts running as an agent once the engine applies the change, on the company's model providers.",
    ),
  ).toBeDefined();
  expect(screen.getByText("contact")).toBeDefined();
  expect(screen.getByText("availability")).toBeDefined();
  fireEvent.click(confirm("Change to agent seat"));
  const data = devData(view.state());
  expect(data).toMatchObject({ handle: "dev", name: "Dev" });
  expect(data).not.toHaveProperty("kind");
  expect(getPath(data, ["runtime", "contact"])).toBeUndefined();
});

// THE KIND CHANGE STRIPS FIELDS IN THE RUNTIME HALF, so a reader the chart did
// not show that half cannot tell what it would strip, and is refused rather
// than handed a change that silently discards what they cannot see.
test("a reader who was not shown the runtime half is refused the change", () => {
  const view = open(checkedEdit(strippedChart(withFields())), "seat:dev");
  expect(toHuman().disabled).toBe(true);
  fireEvent.click(toHuman());
  expect(view.state().log.ops).toHaveLength(0);
});

test("a read-only builder changes nothing, and says why the button is unavailable", () => {
  const chart = withDev({
    name: "Dev",
    kind: "human",
    runtime: { contact: { github_login: "dev" } },
  });
  open(checkedEdit(chart), "seat:dev", { readOnly: true });
  expect(confirm("Change to agent seat").disabled).toBe(true);
  expect(
    screen.getByText(
      "The organization cannot be changed right now, so this change cannot be applied.",
    ),
  ).toBeDefined();
});
