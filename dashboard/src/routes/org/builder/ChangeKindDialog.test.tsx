/**
 * The Change kind dialog.
 *
 * What these protect: the fields the change removes are named before it is
 * recorded, with the credential ones called out as unrecoverable; a human seat
 * cannot be made without a contact identity, nor out of the Datadog fallback
 * without a replacement; the consequences say what the seat becomes; and a
 * schedule the change strands is named first.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { AgentRow, CompanyDocument } from "~/protocol/index.ts";
import { ChangeKindDialog } from "./ChangeKindDialog.tsx";
import { locate } from "./model/draft.ts";
import { getPath } from "./model/json.ts";
import type { BuilderState } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { toDocument } from "./model/document.ts";
import { renderInBuilder, type HarnessOptions } from "./testBuilder.tsx";
import { keyedState } from "./testState.ts";

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
const toHuman = () => confirm("Change to a human seat");

function withFields(): CompanyDocument {
  const doc = fixtureCompany();
  doc.units![0]!.roles![1] = {
    name: "Dev",
    goal: "Build",
    llm: "fast",
    token_budget: 10,
    behavioral_guidelines: ["Be kind"],
    mcp_env: { tracker: { TOKEN: "__redacted__" } },
    integrations: {
      slack: { bot_token: "${DEV_SLACK}", signing_secret: "${DEV_SIGN}", channel: "C1" },
      github: { tier: "review", app_slug: "acme-dev", private_key: "${DEV_KEY}" },
    },
  };
  return doc;
}

test("the fields the change removes are named, the credential ones as gone for good", () => {
  const view = open(keyedState(withFields()), "seat:dev");
  expect(screen.getByText("llm")).toBeDefined();
  expect(screen.getByText("token_budget")).toBeDefined();
  for (const credential of ["integrations.slack", "mcp_env", "integrations.github"]) {
    const item = screen.getByText(credential).closest("li") as HTMLElement;
    expect(item.textContent).toContain("holds credentials, which are gone for good");
  }
  expect(
    screen.getByText("Dev stops running. Its memory is kept but unused while it is a human seat."),
  ).toBeDefined();
  expect(view.container.ownerDocument.body.innerHTML).not.toContain("__redacted__");
});

test("a human seat is not made without a contact identity, and the change records one operation", () => {
  const view = open(keyedState(withFields()), "seat:dev");
  expect(toHuman().disabled).toBe(true);
  fireEvent.change(screen.getByLabelText("Contact"), { target: { value: "github_login" } });
  fireEvent.change(screen.getByLabelText("GitHub login"), { target: { value: "dev" } });
  expect(toHuman().disabled).toBe(false);
  fireEvent.click(toHuman());
  expect(view.state().log.ops[0]).toMatchObject({
    type: "changeKind",
    target: "seat:dev",
    after: "human",
    contact: { github_login: "dev" },
  });
  const seat = locate(view.state().draft, "seat:dev");
  expect(seat?.kind === "seat" && seat.node.data).toEqual({
    name: "Dev",
    kind: "human",
    goal: "Build",
    contact: { github_login: "dev" },
  });
  expect(view.onClose).toHaveBeenCalledTimes(1);
});

test("the Datadog fallback cannot become a human seat without a replacement", () => {
  const view = open(keyedState(fixtureCompany()), "seat:sre");
  fireEvent.change(screen.getByLabelText("Contact"), { target: { value: "github_login" } });
  fireEvent.change(screen.getByLabelText("GitHub login"), { target: { value: "sre" } });
  expect(toHuman().disabled).toBe(true);
  expect(screen.getByText(/SRE is the Datadog fallback/)).toBeDefined();
  fireEvent.change(screen.getByLabelText("Datadog fallback"), { target: { value: "dev" } });
  fireEvent.click(toHuman());
  expect(
    getPath(toDocument(view.state().draft).document, ["integrations", "datadog", "route_to"]),
  ).toBe("dev");
});

// The change stays unavailable until a fallback is chosen, so the company's
// only agent seat would otherwise leave a button that never becomes available
// beside a picker with nothing in it.
test("the company's only agent seat is told why it cannot become human, not offered an empty choice", () => {
  const doc: CompanyDocument = {
    name: "X",
    integrations: { datadog: { route_to: "only" } },
    roles: [{ name: "Only" }, { name: "Pat", kind: "human", contact: { github_login: "pat" } }],
  };
  open(keyedState(doc), "seat:only");
  expect(screen.queryByLabelText("Datadog fallback")).toBeNull();
  expect(
    screen.getByText(
      /It is the company's only agent seat, so add another before changing this one, or disconnect Datadog/,
    ),
  ).toBeDefined();
  fireEvent.change(screen.getByLabelText("Contact"), { target: { value: "github_login" } });
  fireEvent.change(screen.getByLabelText("GitHub login"), { target: { value: "only" } });
  expect(toHuman().disabled).toBe(true);
});

test("a schedule the change strands, and the seat's work in flight, are said first", () => {
  const doc: CompanyDocument = {
    name: "X",
    units: [
      {
        name: "Team",
        lead: "Lead",
        schedules: [{ name: "standup", cron: "0 9 * * *", task: "Standup", target: "lead" }],
        roles: [{ name: "Lead" }, { name: "Member" }],
      },
    ],
  };
  const agents: AgentRow[] = [{ id: "1", role: "Lead", handle: "lead", state: "working" }];
  open(keyedState(doc), "seat:lead", { agents });
  expect(screen.getByText(/Schedule standup on Team would have no runner/)).toBeDefined();
  expect(screen.getByText(/Lead is working now/)).toBeDefined();
});

test("a human seat becomes an agent seat again, losing the fields an agent may not carry", () => {
  const doc = fixtureCompany();
  doc.units![0]!.roles![1] = {
    name: "Dev",
    kind: "human",
    contact: { github_login: "dev" },
    availability: "Weekdays",
  };
  const view = open(keyedState(doc), "seat:dev");
  expect(
    screen.getByText(
      "Dev starts running as an agent once the engine applies the change, on the company's model providers.",
    ),
  ).toBeDefined();
  expect(screen.getByText("contact")).toBeDefined();
  expect(screen.getByText("availability")).toBeDefined();
  fireEvent.click(confirm("Change to an agent seat"));
  const seat = locate(view.state().draft, "seat:dev");
  expect(seat?.kind === "seat" && seat.node.data).toEqual({ name: "Dev" });
});

test("a read-only builder changes nothing", () => {
  const doc = fixtureCompany();
  doc.units![0]!.roles![1] = { name: "Dev", kind: "human", contact: { github_login: "dev" } };
  open(keyedState(doc), "seat:dev", { readOnly: true });
  expect(confirm("Change to an agent seat").disabled).toBe(true);
});
