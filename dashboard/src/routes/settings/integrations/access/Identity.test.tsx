/**
 * A standing identity (a Google growth identity Crewlet made, which a person
 * adds at the product) on the one pattern: its box sets it up, never
 * replaces it, and its sheet is the engine's guide with nothing to bring back.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import type { AccessIdentity, AccessIdentityGuide, AccessSeat } from "~/contract/access.ts";
import { IdentityBox, IdentitySheet, handInOf, identityRequest, verdictOf } from "./Identity.tsx";

afterEach(cleanup);

const EMAIL = "crewlet-mobile-lead-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com";

const SEAT: AccessSeat = { handle: "mobile-lead", name: "Mobile lead", lanes: {} };

/** Play Console's sheet for Mobile lead's identity at View, as the engine words it. */
const USERS = "https://play.google.com/console/developers/users-and-permissions";

const PLAY_GUIDE: AccessIdentityGuide = {
  vendor: "Google Play",
  noun: "Google identity",
  records: "its changes under its own email in the activity log",
  why: "Google Play lets Mobile lead in as its own Google identity and records its changes under its own email in the activity log.",
  name: EMAIL,
  name_rule: "Crewlet made it; it cannot be renamed.",
  name_what: "Google identity",
  role: "View app information (read-only)",
  steps: [
    {
      text: [
        { text: "Open " },
        { ui: ["Play Console", "Users and permissions"], url: USERS },
        { text: "." },
      ],
    },
    { text: [{ text: "Press " }, { ui: ["Invite new users"] }, { text: "." }] },
    { text: [{ text: "Email address:" }], copy: { label: "identity's email", value: EMAIL } },
    { text: [{ text: "Under " }, { ui: ["App permissions"] }, { text: ", add com.acme.app." }] },
    { text: [{ text: "Tick " }, { ui: ["View app information (read-only)"] }, { text: "." }] },
    {
      text: [
        { text: "Leave " },
        { ui: ["Account permissions"] },
        { text: " empty, then press " },
        { ui: ["Invite user"] },
        { text: "." },
      ],
    },
  ],
  back: {
    kind: "nothing",
    text: [{ text: "Nothing to bring back: Crewlet checks Google Play as the identity." }],
  },
  make: {
    label: "Add it for me",
    does: [
      { text: `With a Play Console admin's sign-in, Crewlet adds ${EMAIL} to com.acme.app with ` },
      { ui: ["View app information (read-only)"] },
      { text: "." },
    ],
  },
  checks: [
    { id: "token", label: `Sign in as ${EMAIL}` },
    { id: "reach", label: "Find each of the agent's apps" },
    { id: "role", label: "Read its role on each app" },
  ],
};

/** Mobile lead's identity not added yet: its own, which Play refuses until it is. */
const NOT_ADDED: AccessIdentity = {
  offered: true,
  mode: "own",
  holds: "own",
  status: "none",
  status_label: "Not set up",
  name: EMAIL,
  needs: "View app information (read-only)",
  said: "Needs you: add Mobile lead's own Google identity in Google Play. Until then Google Play refuses its calls.",
  problem: {
    title: "Not set up",
    body: `${EMAIL} is not added to com.acme.app in Google Play yet.`,
  },
  set_up: true,
  replace: false,
  use_company: false,
  guide: PLAY_GUIDE,
};

// A STANDING IDENTITY IS SET UP, NEVER REPLACED: it holds its own identity
// before anybody added it, and the sheet still says Set up identity, with the
// email to copy, Add it for me, and nothing to bring back.
//
// Mutation: title the sheet by `holds` alone, and it reads Replace identity.
test("a Google identity nobody added is set up, not replaced, from the engine's guide", () => {
  const onSetUp = vi.fn();
  render(
    <LayerHost>
      <IdentityBox
        seat={SEAT}
        vendor="Google Play"
        identity={NOT_ADDED}
        checking={false}
        held={undefined}
        onSetUp={onSetUp}
        onUseCompany={vi.fn()}
      />
      <IdentitySheet
        seat={SEAT}
        identity={NOT_ADDED}
        guide={PLAY_GUIDE}
        onClose={vi.fn()}
        onCheck={vi.fn()}
      />
    </LayerHost>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Set up identity: Mobile lead" }));
  expect(onSetUp).toHaveBeenCalledOnce();
  expect(screen.getByText(NOT_ADDED.said)).toBeTruthy();
  const sheet = screen.getByRole("dialog");
  expect(within(sheet).getByText(/Set up identity: Mobile lead/)).toBeTruthy();
  expect(within(sheet).queryByText(/Replace identity/)).toBeNull();
  expect(within(sheet).getAllByText(EMAIL).length).toBeGreaterThan(0);
  // The steps by hand are folded under Make it for me, so read the list as written.
  const items = [...sheet.querySelectorAll("li")].map((li) => li.textContent);
  expect(items).toContain("Press Invite new users.");
  expect(within(sheet).getByText(/Add it for me/)).toBeTruthy();
});

// THE PAGE IS LINKED WHERE A PERSON FIRST READS ITS NAME: the first step names
// Users and permissions, so it carries the link and no button repeats it below.
test("the sheet links the vendor page on the step that names it, with no separate button", () => {
  render(
    <LayerHost>
      <IdentitySheet
        seat={SEAT}
        identity={NOT_ADDED}
        guide={PLAY_GUIDE}
        onClose={vi.fn()}
        onCheck={vi.fn()}
      />
    </LayerHost>,
  );
  const sheet = screen.getByRole("dialog");
  fireEvent.click(within(sheet).getByRole("button", { name: /make it by hand/i }));
  const link = within(sheet).getByRole("link", { name: /Play Console then Users and permissions/ });
  expect(link.getAttribute("href")).toBe(USERS);
  expect(link.closest("li")?.textContent).toMatch(/^Open Play Console/);
  expect(within(sheet).queryByRole("link", { name: /^Open Users and permissions/ })).toBeNull();
  expect(sheet.querySelectorAll(".crewlet-vendor-ui").length).toBeGreaterThan(5);
});

// A READY STANDING IDENTITY OFFERS NOTHING TO DO: the engine sends neither
// Set up nor Replace, so the box draws no action.
test("a ready Google identity's box offers no action", () => {
  const ready: AccessIdentity = {
    ...NOT_ADDED,
    status: "ready",
    status_label: "Ready",
    said: `Google Play sees Mobile lead's own Google identity, ${EMAIL}.`,
    set_up: false,
    problem: undefined,
  };
  render(
    <LayerHost>
      <IdentityBox
        seat={SEAT}
        vendor="Google Play"
        identity={ready}
        checking={false}
        held={undefined}
        onSetUp={vi.fn()}
        onUseCompany={vi.fn()}
      />
    </LayerHost>,
  );
  expect(screen.queryByRole("button", { name: /Set up identity|Replace identity/ })).toBeNull();
});

// WHAT THE SHEET SENDS: Add it for me is a way of its own, and a check with
// nothing brought back asks the engine to read the identity a person added.
test("Add it for me asks the engine to make it, a bare check to read what was added", async () => {
  expect(await identityRequest({ made: true })).toEqual({ way: "made" });
  expect(await identityRequest({ made: false })).toEqual({ way: "invited" });
});

// A FILE MAY BRING ONE OPTIONAL VALUE BESIDE IT (Apple's vendor number for a
// Sales key): the guide's detail is the kit's, asked on a number pad, and the
// check sends it only where the person typed one. Mutation: drop the detail
// from either side, and its line below fails.
test("a file's optional detail is asked beside it and sent only when typed", async () => {
  const handIn = handInOf({
    kind: "file",
    accept: ".p8",
    hint: [{ text: "The file Apple offered" }],
    label: "Key ID",
    detail: { label: "Vendor number", hint: "Only for a Sales key." },
  });
  expect(handIn).toMatchObject({
    kind: "file",
    detail: { label: "Vendor number", hint: "Only for a Sales key.", inputMode: "numeric" },
  });
  expect(handInOf({ kind: "file", accept: ".p8", hint: [] })).not.toHaveProperty("detail");
  const file = new File(["-----BEGIN PRIVATE KEY-----"], "AuthKey_7KQ4M2ZX9P.p8");
  expect(
    await identityRequest({ made: false, file, field: "7KQ4M2ZX9P", detail: "85012345" }),
  ).toEqual({
    way: "handed_in",
    value: "-----BEGIN PRIVATE KEY-----",
    file_name: "AuthKey_7KQ4M2ZX9P.p8",
    id: "7KQ4M2ZX9P",
    detail: "85012345",
  });
  expect(await identityRequest({ made: false, file, field: "7KQ4M2ZX9P" })).not.toHaveProperty(
    "detail",
  );
});

// A REFUSED CHECK MARKS THE READ IT STOPPED AT, with the vendor's reason under
// it, and the reads after it as not run: never every read stopped, and never
// a later read failed while an earlier one is said done.
//
// Mutation: drop failed_at from verdictOf, and the sheet draws the last read
// failed and the refused first read done.
test("a refused check marks the read it stopped at, says why, and runs nothing after it", async () => {
  const reason =
    "Apple refuses every API call for this team until its Account Holder accepts the latest agreement.";
  const onCheck = vi.fn(async () =>
    verdictOf({
      outcome: "refused",
      summary: reason,
      failed_at: "token",
      steps: [{ id: "token", said: reason }],
      warnings: [
        { action: "Sign in to App Store Connect as the Account Holder, then check again." },
      ],
    }),
  );
  render(
    <LayerHost>
      <IdentitySheet
        seat={SEAT}
        identity={NOT_ADDED}
        guide={PLAY_GUIDE}
        onClose={vi.fn()}
        onCheck={onCheck}
      />
    </LayerHost>,
  );
  const sheet = screen.getByRole("dialog");
  await act(async () => {
    fireEvent.click(within(sheet).getByRole("button", { name: "Make it for me" }));
  });
  const reads = within(sheet).getByRole("list", { name: /Checking Mobile lead's identity/ });
  const states = [...reads.querySelectorAll(":scope > li")].map(
    (li) => /crewlet-step-list__step--(\w+)/.exec(li.className)?.[1],
  );
  expect(states).toEqual(["failed", "pending", "pending"]);
  expect(within(reads).getByText(reason)).toBeTruthy();
  expect(within(sheet).queryByText("The check did not finish")).toBeNull();
});

// THE VENDOR'S ANSWERS ARE KEPT UNDER THE READ THAT MADE THEM, so a role
// Crewlet could not tell says which answer disagreed.
test("verdictOf carries each read's reason and the vendor's answers to the sheet", () => {
  const v = verdictOf({
    outcome: "refused",
    summary: "Apple's answers fit no role.",
    failed_at: "role",
    steps: [
      {
        id: "role",
        said: "Apple's answers fit no role.",
        detail: "financeReports_getCollection 200",
      },
    ],
  });
  expect(v.failedAt).toBe("role");
  expect(v.steps).toEqual({
    role: { note: "Apple's answers fit no role.", detail: "financeReports_getCollection 200" },
  });
  expect(verdictOf({ outcome: "works", summary: "Works." })).not.toHaveProperty("failedAt");
});

// THE VERDICT SAYS WHAT HAPPENED, THE IDENTITY'S FACTS AND WHAT TO DO NEXT,
// and no read twice: each is said on its own step. An ID is in the mono face.
//
// Mutation: draw the facts as plain text, and the key's ID reads as prose.
test("the verdict is the headline, the identity's facts and the next step", () => {
  const v = verdictOf({
    outcome: "refused",
    summary: "Apple's answers fit no role.",
    facts: [
      { label: "Identity", value: "QD2KH2G8B3", identifier: true },
      { label: "Role needed", value: "App Manager" },
      { label: "Team", value: "main", identifier: true },
    ],
    warnings: [{ action: "Check the key's Access in Team Keys." }],
  });
  render(<LayerHost>{v.view}</LayerHost>);
  expect(screen.getByText("Apple's answers fit no role.")).toBeTruthy();
  const facts = screen.getByText("Identity").closest("dl")!;
  expect(within(facts).getByText("Role needed")).toBeTruthy();
  expect(within(facts).getByText("QD2KH2G8B3").className).toMatch(/mono/);
  expect(within(facts).getByText("App Manager").className).not.toMatch(/mono/);
  expect(screen.getByText("Next step")).toBeTruthy();
  expect(screen.getByText("Check the key's Access in Team Keys.")).toBeTruthy();
});

// A SHEET OPEN WHILE GOOGLE CANNOT FIND THE IDENTITY YET HOLDS ITS ADD LINKS,
// saying when they work, and opens them the moment ready_at passes.
//
// Mutation: drop notReady, and a person opens a page whose add Google refuses.
test("each entry's add link waits until Google can find the identity", () => {
  vi.useFakeTimers({ now: Date.parse("2026-11-25T08:00:00Z") });
  try {
    const guide: AccessIdentityGuide = {
      ...PLAY_GUIDE,
      ready_at: "2026-11-25T08:01:00Z",
      entries: [
        {
          id: "com.acme.app",
          name: "Acme",
          state: "missing",
          add_url: "https://play.example/acme",
        },
      ],
    };
    render(
      <LayerHost>
        <IdentitySheet
          seat={SEAT}
          identity={NOT_ADDED}
          guide={guide}
          onClose={vi.fn()}
          onCheck={vi.fn()}
        />
      </LayerHost>,
    );
    const sheet = screen.getByRole("dialog");
    expect(sheet.textContent).toContain("Ready to add in about a minute");
    expect(within(sheet).queryByRole("link", { name: /Acme/ })).toBeNull();
    act(() => {
      vi.advanceTimersByTime(61_000);
    });
    expect(sheet.textContent).not.toContain("Ready to add in about a minute");
    expect(within(sheet).getByRole("link", { name: /Acme/ }).getAttribute("href")).toBe(
      "https://play.example/acme",
    );
  } finally {
    vi.useRealTimers();
  }
});
