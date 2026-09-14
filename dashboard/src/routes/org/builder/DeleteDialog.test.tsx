/**
 * The Delete dialog.
 *
 * What these protect: the counts and the cleared references come from the
 * operation the dialog will dispatch; the seats a removed unit holds only by
 * reference are the operator's choice; the Datadog fallback must be replaced
 * before the seat that is it can go; what stays at the vendors and in the
 * secret store is named, and never a credential; the mailbox and memory copy
 * matches what the engine does; and a mass removal is acknowledged.
 */

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentRow, CompanyDocument } from "~/protocol/index.ts";
import { DeleteDialog } from "./DeleteDialog.tsx";
import { locate } from "./model/draft.ts";
import { getPath } from "./model/json.ts";
import type { BuilderState } from "./model/reducer.ts";
import { builderReducer } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { toDocument } from "./model/document.ts";
import { renderInBuilder, type HarnessOptions } from "./testBuilder.tsx";
import { keyedState } from "./testState.ts";

afterEach(cleanup);

function open(state: BuilderState, key: string, options: HarnessOptions = {}) {
  const onClose = vi.fn();
  const view = renderInBuilder(state, <DeleteDialog nodeKey={key} onClose={onClose} />, options);
  return { ...view, onClose };
}

const deleteButton = () => screen.getByRole("button", { name: "Delete" }) as HTMLButtonElement;
/** The fixture's Datadog fallback is SRE, so a removal that takes SRE needs one. */
const replaceFallback = (handle: string) =>
  fireEvent.change(screen.getByLabelText("Datadog fallback"), { target: { value: handle } });

describe("a unit", () => {
  test("counts what goes with it, lists the references it clears, and removes it on Delete", () => {
    const view = open(keyedState(fixtureCompany()), "unit:Engineering");
    expect(
      screen.getByText(/Deletes the unit Engineering with 1 unit and 3 seats inside it./),
    ).toBeDefined();
    expect(screen.getByText("CEO no longer manages Engineering.")).toBeDefined();
    replaceFallback("ceo");
    fireEvent.click(deleteButton());
    expect(view.state().log.ops[0]).toMatchObject({ type: "remove", target: "unit:Engineering" });
    expect(locate(view.state().draft, "unit:Engineering")).toBeUndefined();
    expect(view.onClose).toHaveBeenCalledTimes(1);
  });

  test("seats placed in it by a unit reference are kept at the top level unless the operator says otherwise", () => {
    const state = keyedState(fixtureCompany());
    const view = open(state, "unit:Platform");
    const section = screen
      .getByRole("heading", { name: "Seats placed here by unit reference" })
      .closest("section") as HTMLElement;
    expect(within(section).getByText("Designer")).toBeDefined();
    replaceFallback("ceo");
    fireEvent.click(deleteButton());
    expect(locate(view.state().draft, "seat:designer")).toBeDefined();
    expect(getPath(toDocument(view.state().draft).document, ["roles"]) !== undefined).toBe(true);
    cleanup();

    const second = open(state, "unit:Platform");
    fireEvent.click(screen.getByRole("radio", { name: "Delete them too" }));
    expect(screen.getByText(/Deletes the unit Platform with 2 seats inside it./)).toBeDefined();
    replaceFallback("ceo");
    fireEvent.click(deleteButton());
    expect(locate(second.state().draft, "seat:designer")).toBeUndefined();
  });
});

describe("outside the chart", () => {
  test("the Datadog fallback must be replaced before the seat that is it can go", () => {
    const view = open(keyedState(fixtureCompany()), "seat:sre");
    expect(deleteButton().disabled).toBe(true);
    expect(screen.getByText(/SRE is the Datadog fallback/)).toBeDefined();
    fireEvent.change(screen.getByLabelText("Datadog fallback"), { target: { value: "dev" } });
    expect(deleteButton().disabled).toBe(false);
    fireEvent.click(deleteButton());
    expect(
      getPath(toDocument(view.state().draft).document, ["integrations", "datadog", "route_to"]),
    ).toBe("dev");
  });

  // Delete stays unavailable until a fallback is chosen, so with nothing to
  // choose the dialog has to name the way out rather than leave a button that
  // never becomes available beside a picker with nothing in it.
  test("with no agent seat left to take the fallback, the dialog says so instead of offering an empty choice", () => {
    const doc: CompanyDocument = {
      name: "X",
      integrations: { datadog: { route_to: "only" } },
      roles: [{ name: "Only" }, { name: "Pat", kind: "human", contact: { github_login: "pat" } }],
    };
    open(keyedState(doc), "seat:only");
    expect(screen.queryByLabelText("Datadog fallback")).toBeNull();
    expect(
      screen.getByText(
        /This removal would leave no agent seat to take it over, so add one first, or disconnect Datadog/,
      ),
    ).toBeDefined();
    expect(deleteButton().disabled).toBe(true);
  });

  test("a removed seat's GitLab access level goes with it, so no later seat inherits it", () => {
    open(keyedState(fixtureCompany()), "seat:dev");
    expect(
      screen.getByText(
        "The GitLab access level for dev (developer) is removed, so it cannot pass to a seat added later under that handle.",
      ),
    ).toBeDefined();
  });

  test("what the engine made at the vendors and the entries the seat references are named, never a credential", () => {
    const doc = fixtureCompany();
    doc.integrations = {
      ...doc.integrations,
      atlassian: { org_id: "acme" },
      mattermost: { enabled: true, url: "https://chat.example.com", team: "acme" },
    };
    doc.units![0]!.roles![1] = {
      name: "Dev",
      integrations: {
        github: { tier: "review", app_slug: "acme-dev", private_key: "${DEV_GITHUB_KEY}" },
        mattermost: { bot_token: "${DEV_MM_TOKEN}", username: "dev-bot" },
      },
      mcp_env: { tracker: { TOKEN: "__redacted__" } },
    };
    const view = open(keyedState(doc), "seat:dev");
    expect(screen.getByText(/These stay until you decommission them./)).toBeDefined();
    const entry = screen.getByText(/the GitHub App acme-dev/);
    expect(entry.textContent).toContain("its Mattermost bot");
    expect(entry.textContent).toContain("its GitLab service account");
    expect(entry.textContent).toContain("its Atlassian account");
    expect(entry.textContent).toContain("DEV_GITHUB_KEY");
    expect(entry.textContent).toContain("DEV_MM_TOKEN");
    const html = view.container.ownerDocument.body.innerHTML;
    expect(html).not.toContain("__redacted__");
    expect(screen.getByRole("link", { name: "Open Secrets" }).getAttribute("href")).toBe(
      "#/secrets",
    );
  });

  test("the mailbox is retired after the grace period and the memory reattaches by handle", () => {
    open(keyedState(fixtureCompany()), "seat:dev");
    const note = screen.getByText(/kept for 24 hours after the engine applies the change/);
    expect(note.textContent).toContain("then retired");
    expect(note.textContent).toContain("any coding runs it still has are ended then");
    expect(note.textContent).toContain("a seat added later with the same handle reattaches to it");
  });

  test("a seat added in this draft has no mailbox to retire and nothing at a vendor", () => {
    const doc: CompanyDocument = { name: "X", roles: [{ name: "Dev" }] };
    const state = builderReducer(keyedState(doc), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "company", after: "seat:dev" },
        data: { name: "QA" },
      },
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
    const doc: CompanyDocument = {
      name: "X",
      units: [
        {
          name: "Ops",
          schedules: [{ name: "sweep", cron: "0 * * * *", task: "Sweep" }],
          roles: [
            { name: "Runner" },
            { name: "Pat", kind: "human", contact: { github_login: "pat" } },
          ],
        },
      ],
    };
    const agents: AgentRow[] = [{ id: "1", role: "Runner", handle: "runner", state: "working" }];
    open(keyedState(doc), "seat:runner", { agents });
    expect(screen.getByText(/Schedule sweep on Ops would have no runner/)).toBeDefined();
    expect(screen.getByText(/Runner is working now/)).toBeDefined();
  });

  test("removing more than half the saved company's seats is acknowledged first", () => {
    const doc: CompanyDocument = {
      name: "X",
      units: [
        { name: "Ops", roles: [{ name: "Runner" }, { name: "Second" }] },
        { name: "Other", roles: [{ name: "Keeper" }] },
      ],
    };
    const view = open(keyedState(doc), "unit:Ops");
    const ack = screen.getByRole("checkbox", { name: "Delete 2 of the 3 seats the company has" });
    expect(deleteButton().disabled).toBe(true);
    fireEvent.click(ack);
    expect(deleteButton().disabled).toBe(false);
    fireEvent.click(deleteButton());
    expect(view.state().log.ops).toHaveLength(1);
  });

  test("a read-only builder deletes nothing", () => {
    open(keyedState(fixtureCompany()), "seat:dev", { readOnly: true });
    expect(deleteButton().disabled).toBe(true);
  });
});
