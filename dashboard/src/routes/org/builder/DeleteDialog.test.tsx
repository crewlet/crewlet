/**
 * The Delete dialog.
 *
 * What these protect: the counts and the cleared references come from the
 * operation the dialog will dispatch; the Datadog fallback must be replaced
 * before the seat that is it can go; what stays at the vendors and in the
 * secret store is named, and never a credential; the mailbox and memory copy
 * matches what the engine does; and a mass removal is acknowledged.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentRow, ChartRead } from "~/protocol/index.ts";
import { DeleteDialog } from "./DeleteDialog.tsx";
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
  const view = renderInBuilder(state, <DeleteDialog nodeKey={key} onClose={onClose} />, options);
  return { ...view, onClose };
}

const deleteButton = () => screen.getByRole("button", { name: "Delete" }) as HTMLButtonElement;
/** The fixture's Datadog fallback is SRE, so a removal that takes SRE needs one. */
const replaceFallback = (label: string) => pick(screen.getByLabelText("Datadog fallback"), label);
const routeTo = (state: BuilderState) =>
  getPath(state.draft.company, ["integrations", "datadog", "route_to"]);

describe("a unit", () => {
  test("counts what goes with it, lists the references it clears, and removes it on Delete", () => {
    const view = open(checkedEdit(), "unit:engineering");
    expect(
      screen.getByText(/Deletes the unit Engineering with 1 unit and 4 seats inside it./),
    ).toBeDefined();
    // Named by the addresses the CEO's list held, one sentence per entry.
    expect(screen.getByText("CEO no longer manages engineering.")).toBeDefined();
    expect(screen.getByText("CEO no longer manages designer.")).toBeDefined();
    replaceFallback("CEO (@ceo)");
    // Four of the saved company's six seats: a mass removal, acknowledged first.
    expect(deleteButton().disabled).toBe(true);
    fireEvent.click(
      screen.getByRole("checkbox", { name: "Delete 4 of the 6 seats the company has" }),
    );
    fireEvent.click(deleteButton());
    expect(view.state().log.ops[0]).toMatchObject({ type: "remove", target: "unit:engineering" });
    expect(locate(view.state().draft, "unit:engineering")).toBeUndefined();
    expect(locate(view.state().draft, "seat:designer")).toBeUndefined();
    expect(view.onClose).toHaveBeenCalledTimes(1);
  });
});

test("an empty unit is said to hold nothing", () => {
  const chart = fixtureChart();
  chart.units.push({ key: "legal", name: "Legal" });
  open(checkedEdit(chart), "unit:legal");
  expect(screen.getByText(/Deletes the unit Legal, which holds nothing./)).toBeDefined();
});

describe("outside the chart", () => {
  test("the Datadog fallback must be replaced before the seat that is it can go", () => {
    const view = open(checkedEdit(), "seat:sre");
    expect(deleteButton().disabled).toBe(true);
    expect(screen.getByText(/SRE is the Datadog fallback/)).toBeDefined();
    replaceFallback("Dev (@dev)");
    expect(deleteButton().disabled).toBe(false);
    fireEvent.click(deleteButton());
    expect(routeTo(view.state())).toBe("dev");
  });

  // The engine validates `route_to` only while Datadog is enabled, and reads
  // it trimmed: a switched-off block wakes nobody and requires nothing, and a
  // value written with a space still names its seat.
  test("the fallback is the engine's: only while Datadog is on, and read trimmed", () => {
    const off = fixtureSettings();
    off.integrations = { ...off.integrations, datadog: { enabled: false, route_to: "sre" } };
    const view = open(checkedEdit(fixtureChart(), {}, off), "seat:sre");
    expect(screen.queryByLabelText("Datadog fallback")).toBeNull();
    expect(deleteButton().disabled).toBe(false);
    fireEvent.click(deleteButton());
    expect(view.state().log.ops[0]).toMatchObject({ type: "remove", target: "seat:sre" });
    cleanup();

    const spaced = fixtureSettings();
    spaced.integrations = { ...spaced.integrations, datadog: { enabled: true, route_to: " sre " } };
    open(checkedEdit(fixtureChart(), {}, spaced), "seat:sre");
    expect(screen.getByText(/SRE is the Datadog fallback/)).toBeDefined();
    expect(deleteButton().disabled).toBe(true);
  });

  // The replacements are what the removal leaves: a seat the removal takes is
  // never offered, since a fallback naming it is one the engine refuses.
  test("a seat the removal takes is never offered as the replacement", () => {
    open(checkedEdit(), "unit:platform");
    fireEvent.click(screen.getByLabelText("Datadog fallback"));
    const offered = screen.getAllByRole("option").map((o) => o.textContent);
    expect(offered).not.toContain("Designer (@designer)");
    expect(offered).not.toContain("SRE (@sre)");
    // The control: a seat outside the unit is.
    expect(offered).toContain("CEO (@ceo)");
  });

  // Delete stays unavailable until a fallback is chosen, so with nothing to
  // choose the dialog has to name the way out rather than leave a button that
  // never becomes available beside a picker with nothing in it.
  test("with no agent seat left to take the fallback, the dialog says so instead of offering an empty choice", () => {
    const chart = chartOf({
      seats: [
        { handle: "only", name: "Only" },
        {
          handle: "pat",
          name: "Pat",
          kind: "human",
          runtime: { contact: { github_login: "pat" } },
        },
      ],
    });
    const settings = { name: "X", integrations: { datadog: { enabled: true, route_to: "only" } } };
    open(checkedEdit(chart, {}, settings), "seat:only");
    expect(screen.queryByLabelText("Datadog fallback")).toBeNull();
    expect(
      screen.getByText(
        /This removal would leave no agent seat to take it over, so add one first, or disconnect Datadog/,
      ),
    ).toBeDefined();
    expect(deleteButton().disabled).toBe(true);
  });

  test("a removed seat's GitLab access level goes with it, so no later seat inherits it", () => {
    open(checkedEdit(), "seat:dev");
    expect(
      screen.getByText("The GitLab access level for @dev (developer) is removed with the seat."),
    ).toBeDefined();
  });

  test("what the engine made at the vendors and the entries the seat references are named, never a credential", () => {
    const settings = fixtureSettings();
    settings.integrations = {
      ...settings.integrations,
      atlassian: { org_id: "acme" },
      mattermost: { enabled: true, url: "https://chat.example.com", team: "acme" },
      datadog: { enabled: true, route_to: "sre", provisioning: { site: "datadoghq.eu" } },
    };
    const chart = fixtureChart();
    chart.seats = chart.seats.map((s) =>
      s.handle === "dev"
        ? {
            handle: "dev",
            name: "Dev",
            unit: "engineering",
            runtime: {
              github: { tier: "review", app_slug: "acme-dev", private_key: "${DEV_GITHUB_KEY}" },
              mattermost: { bot_token: "${DEV_MM_TOKEN}", username: "dev-bot" },
              mcp_env: {
                gitlab: { GITLAB_TOKEN: "${DEV_GITLAB_TOKEN}" },
                datadog: { DD_APP_KEY: "${DEV_DD_KEY}" },
                jira: { JIRA_API_TOKEN: "__redacted__" },
              },
            },
          }
        : s,
    );
    const view = open(checkedEdit(chart, {}, settings), "seat:dev");
    expect(screen.getByText(/These stay until you decommission them./)).toBeDefined();
    const entry = screen.getByText(/the GitHub App acme-dev/);
    expect(entry.textContent).toContain("its Mattermost bot");
    expect(entry.textContent).toContain("its GitLab service account");
    expect(entry.textContent).toContain("its Datadog service account");
    expect(entry.textContent).toContain("its Atlassian account");
    expect(entry.textContent).toContain("DEV_GITHUB_KEY");
    expect(entry.textContent).toContain("DEV_MM_TOKEN");
    const html = view.container.ownerDocument.body.innerHTML;
    expect(html).not.toContain("__redacted__");
    expect(screen.getByRole("link", { name: "Open Secrets" }).getAttribute("href")).toBe(
      "#/admin/credentials",
    );
  });

  // THE RUNTIME HALF IS WHERE ALL OF IT LIVES, so a reader the chart did not
  // show it is told there may be something and why it cannot be listed.
  test("a reader not shown the runtime half is told what cannot be listed", () => {
    open(checkedEdit(strippedChart(fixtureChart())), "seat:dev");
    expect(screen.getByText(/which was not shown to you, so it is not listed here/)).toBeDefined();
    cleanup();
    // The control: shown the half, a seat with nothing at a vendor lists nothing.
    open(checkedEdit(), "seat:ceo");
    expect(screen.queryByText(/which was not shown to you/)).toBeNull();
  });

  // The provisioners make an account only for a seat enrolled with the vendor
  // (a credential for its mcp_env server), so a connected tool is not an
  // account for every seat, and nothing was ever made for a seat this draft
  // created.
  test("an account is named only where the seat is enrolled for it, and never for a new seat", () => {
    const settings = fixtureSettings();
    settings.integrations = {
      ...settings.integrations,
      atlassian: { org_id: "acme" },
      datadog: { enabled: true, route_to: "sre", provisioning: { site: "datadoghq.eu" } },
    };
    // Dev holds only the unit's tracker credential; GitLab provisioning and
    // Atlassian are connected in this company all the same.
    open(checkedEdit(fixtureChart(), {}, settings), "seat:dev");
    expect(screen.queryByText(/service account|Atlassian account/)).toBeNull();
    cleanup();

    // Enrolled through its unit's mcp_env, which every direct member receives.
    const chart: ChartRead = fixtureChart();
    chart.units.find((u) => u.key === "sales")!.runtime = {
      mcp_env: { gitlab: { GITLAB_TOKEN: "${SALES_GITLAB}" } },
    };
    const saved = checkedEdit(chart);
    open(saved, "seat:account-executive");
    expect(screen.getByText(/its GitLab service account/)).toBeDefined();
    cleanup();

    const added = record(saved, {
      type: "addSeat",
      key: "new:closer",
      placement: { parent: "unit:sales" },
      data: {
        handle: "closer",
        name: "Closer",
        runtime: { mcp_env: { gitlab: { GITLAB_TOKEN: "${CLOSER_GITLAB}" } } },
      },
    });
    open(added, "new:closer");
    expect(screen.getByText(/Deletes the agent seat Closer./)).toBeDefined();
    expect(screen.queryByText(/These stay until you decommission them/)).toBeNull();
  });

  // THE MEMORY IS KEPT UNDER AN IDENTITY THE CHART NEVER ISSUES AGAIN, so no
  // seat added later under the same handle inherits it — the opposite of what
  // this note used to promise.
  test("the mailbox is retired after the grace period and the memory stays with the removed seat", () => {
    open(checkedEdit(), "seat:dev");
    const note = screen.getByText(/kept for 24 hours after the engine applies the change/);
    expect(note.textContent).toContain("then retired");
    expect(note.textContent).toContain("any coding runs it still has are ended then");
    expect(note.textContent).toContain(
      "Its memory is kept under an identity no seat added later can have.",
    );
  });

  test("a seat added in this draft has no mailbox to retire and nothing at a vendor", () => {
    const state = record(checkedEdit(chartOf({ seats: [{ handle: "dev", name: "Dev" }] })), {
      type: "addSeat",
      key: "new:qa",
      placement: { parent: "company" },
      data: { handle: "qa", name: "QA" },
    });
    open(state, "new:qa");
    expect(screen.queryByText(/kept for 24 hours/)).toBeNull();
    expect(screen.queryByRole("heading", { name: "Outside the chart" })).toBeNull();
    // A saved seat of the same company does carry the note.
    cleanup();
    open(state, "seat:dev");
    expect(screen.getByText(/kept for 24 hours/)).toBeDefined();
  });
});

describe("before the seats go", () => {
  test("a schedule the removal strands, and the seat working now, are said first", () => {
    const chart = chartOf({
      units: [
        {
          key: "ops",
          name: "Ops",
          runtime: { schedules: [{ name: "sweep", cron: "0 * * * *", task: "Sweep" }] },
        },
      ],
      seats: [
        { handle: "runner", name: "Runner", unit: "ops" },
        {
          handle: "pat",
          name: "Pat",
          kind: "human",
          unit: "ops",
          runtime: { contact: { github_login: "pat" } },
        },
      ],
    });
    const agents: AgentRow[] = [{ id: "1", role: "Runner", handle: "runner", state: "working" }];
    open(checkedEdit(chart), "seat:runner", { agents });
    expect(screen.getByText(/Schedule sweep on Ops would have no runner/)).toBeDefined();
    expect(screen.getByText(/Runner is working now/)).toBeDefined();
  });

  test("every seat a unit's removal takes that is working now is named", () => {
    const agents: AgentRow[] = [
      { id: "1", role: "VP Engineering", handle: "vp-engineering", state: "working" },
      { id: "2", role: "SRE", handle: "sre", state: "working" },
    ];
    open(checkedEdit(), "unit:engineering", { agents });
    const notes = screen.getAllByText(/is working now/);
    expect(notes.map((n) => n.textContent?.split(" is working")[0]).sort()).toEqual([
      "SRE",
      "VP Engineering",
    ]);
    expect(notes.every((n) => n.tagName === "LI")).toBe(true);
  });

  test("removing more than half the saved company's seats is acknowledged first", () => {
    const chart = chartOf({
      units: [
        { key: "ops", name: "Ops" },
        { key: "other", name: "Other" },
      ],
      seats: [
        { handle: "runner", name: "Runner", unit: "ops" },
        { handle: "second", name: "Second", unit: "ops" },
        { handle: "keeper", name: "Keeper", unit: "other" },
      ],
    });
    const view = open(checkedEdit(chart), "unit:ops");
    const ack = screen.getByRole("checkbox", { name: "Delete 2 of the 3 seats the company has" });
    expect(deleteButton().disabled).toBe(true);
    fireEvent.click(ack);
    expect(deleteButton().disabled).toBe(false);
    fireEvent.click(deleteButton());
    expect(view.state().log.ops).toHaveLength(1);
  });

  // A disabled button is not a reason: without the note the operator fills
  // the dialog in and nothing on screen says the builder is what is in the way.
  test("a read-only builder deletes nothing, and says why the button is unavailable", () => {
    open(checkedEdit(), "seat:dev", { readOnly: true });
    expect(deleteButton().disabled).toBe(true);
    expect(
      screen.getByText(
        "The organization cannot be changed right now, so this change cannot be applied.",
      ),
    ).toBeDefined();
  });
});
