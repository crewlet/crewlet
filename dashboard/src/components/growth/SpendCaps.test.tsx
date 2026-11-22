/**
 * The caps an admin sets are what the document stores: both or neither per
 * account, in the account's currency, checked by the engine before they are
 * saved, and never offered to a reader who may not write the document.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import type { GrowthSpend } from "~/contract/growth.ts";
import { SpendCaps } from "./SpendCaps.tsx";

const save = vi.hoisted(() => vi.fn());
const read = vi.hoisted(() => vi.fn());
const check = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: read,
  savePatch: save,
  dryRunPatch: check,
}));

const access = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: access,
}));

const SPEND: GrowthSpend = {
  accounts: [{ account: "123-456-7890", currency: "EUR", per_change: "50.00", daily: "200.00" }],
};
const ACCOUNTS = [
  { id: "123-456-7890", name: "Nimbus Ads", currency: "EUR" },
  { id: "987-654-3210", name: "Nimbus Japan", currency: "JPY" },
];

async function mount(spend: GrowthSpend = SPEND, accounts = ACCOUNTS, onSaved = () => {}) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
    Promise.resolve(ADMIN_VIEWER);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <SpendCaps kind="google_ads" spend={spend} accounts={accounts} onSaved={onSaved} />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
  return view;
}

beforeEach(() => {
  vi.clearAllMocks();
  access.mockReturnValue({ can: true });
  read.mockResolvedValue({
    kind: "document",
    document: { integrations: { google_ads: { enabled: true, spend: SPEND } } },
    etag: '"rev"',
  });
  check.mockResolvedValue({ kind: "valid", warnings: [], derived: null });
  save.mockResolvedValue({ kind: "saved", revisionId: "r", epoch: 2, warnings: [], derived: null });
});

afterEach(cleanup);

test("each account's caps are drawn in its own currency, and an account without caps is said", async () => {
  await mount();
  expect(screen.getByText("€50.00")).toBeTruthy();
  expect(screen.getByText("€200.00")).toBeTruthy();
  expect(
    screen.getByText("Nimbus Japan has no caps, so agents cannot ask to spend on it."),
  ).toBeTruthy();
});

test("a raise is checked by the engine, then saved as the document stores it", async () => {
  await mount();
  fireEvent.click(screen.getByRole("button", { name: "Set spending caps" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getAllByRole("textbox", { name: /Daily/ })[0]!, {
    target: { value: "250" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save caps" }));
  await waitFor(() => expect(save).toHaveBeenCalled());
  expect(check).toHaveBeenCalled();
  const [patch, etag, summary] = save.mock.calls[0]!;
  expect(patch).toEqual({
    integrations: {
      google_ads: {
        spend: {
          accounts: [
            { account: "123-456-7890", currency: "EUR", per_change: "50.00", daily: "250.00" },
          ],
        },
      },
    },
  });
  expect(etag).toBe('"rev"');
  expect(summary).toMatch(/^Set the spending caps of /);
});

test("a new account takes its currency from the vendor, and half a pair cannot be saved", async () => {
  await mount();
  fireEvent.click(screen.getByRole("button", { name: "Set spending caps" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Nimbus Japan" }));
  // An account the vendor reports keeps its ID and currency: neither is asked.
  expect(within(dialog).queryByRole("textbox", { name: "Account ID" })).toBeNull();
  const per = within(dialog).getAllByRole("textbox", { name: /Per change/ })[1]!;
  expect(per.getAttribute("placeholder")).toBe("0");
  fireEvent.change(per, { target: { value: "3400" } });
  expect(await within(dialog).findByText(/Set both caps, or neither/)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("button", { name: "Save caps" }));
  expect(save).not.toHaveBeenCalled();
});

test("a reader who may not write the document is told why, and opens no form", async () => {
  access.mockReturnValue({
    can: false,
    block: "refused",
    reason: "Only an admin changes the company's settings.",
  });
  await mount();
  const button = screen.getByRole("button", { name: "Set spending caps" });
  expect(button.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(button);
  expect(screen.queryByRole("dialog")).toBeNull();
});

const documentOf = (spend: GrowthSpend) => ({
  kind: "document",
  document: { integrations: { google_ads: { enabled: true, spend } } },
  etag: '"rev"',
});

test("a stored row without its currency is put right in place, and saved", async () => {
  const bare: GrowthSpend = {
    accounts: [{ account: "123-456-7890", per_change: "50", daily: "200" }],
  };
  read.mockResolvedValue(documentOf(bare));
  await mount(bare);
  fireEvent.click(screen.getByRole("button", { name: "Set spending caps" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).queryByRole("textbox", { name: "Account ID" })).toBeNull();
  const currency = within(dialog).getByRole("textbox", { name: "Currency" });
  expect(within(dialog).getByText(/three-letter code/)).toBeTruthy();
  fireEvent.change(currency, { target: { value: "eur" } });
  await waitFor(() => expect(within(dialog).queryByText(/three-letter code/)).toBeNull());
  fireEvent.click(within(dialog).getByRole("button", { name: "Save caps" }));
  await waitFor(() => expect(save).toHaveBeenCalled());
  expect(save.mock.calls[0]![0]).toEqual({
    integrations: {
      google_ads: {
        spend: {
          accounts: [
            { account: "123-456-7890", currency: "EUR", per_change: "50.00", daily: "200.00" },
          ],
        },
      },
    },
  });
});

test("an account the vendor gives no name is told by its ID everywhere", async () => {
  await mount(SPEND, [...ACCOUNTS, { id: "555-000-1111", name: "", currency: "USD" }]);
  expect(screen.getByText(/^2 accounts have no caps/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Set spending caps" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "555-000-1111" }));
  expect(within(dialog).getByRole("heading", { name: "555-000-1111" })).toBeTruthy();
  expect(
    within(dialog).getByRole("button", { name: "Remove the caps of 555-000-1111" }),
  ).toBeTruthy();
});

test("caps another admin saved since the form opened are reloaded, never written over", async () => {
  const onSaved = vi.fn();
  await mount(SPEND, ACCOUNTS, onSaved);
  read.mockResolvedValue(
    documentOf({
      accounts: [
        ...SPEND.accounts!,
        { account: "987-654-3210", currency: "JPY", per_change: "500", daily: "2000" },
      ],
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Set spending caps" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getAllByRole("textbox", { name: /Daily/ })[0]!, {
    target: { value: "250" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save caps" }));
  expect(await within(dialog).findByText(/Reload to see the caps as they are now/)).toBeTruthy();
  expect(save).not.toHaveBeenCalled();
  // Reload hands back to the section, which reads the caps again, so the next save is made
  // over what is there now.
  fireEvent.click(within(dialog).getByRole("button", { name: /Reload/ }));
  await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
});

test("caps the account's currency does not match, or with no currency, are flagged as refusing spend", async () => {
  await mount({
    accounts: [
      { account: "123-456-7890", currency: "USD", per_change: "50", daily: "200" },
      { account: "987-654-3210", per_change: "500", daily: "2000" },
    ],
  });
  expect(screen.getByText("The account is in EUR: spending refused")).toBeTruthy();
  expect(screen.getByText("No currency: spending refused")).toBeTruthy();
  // A cap that cannot be drawn as money is never drawn as a bare number.
  expect(screen.queryByText("500")).toBeNull();
  expect(screen.getAllByText(/not readable as an amount/)).toHaveLength(2);
});
