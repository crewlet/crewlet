/**
 * The one destructive dialog in the product, and what it must not infer.
 *
 * Removing what the engine registered for itself is never in question.
 * Removing the ACCOUNTS it created is a separate decision, because each is a
 * colleague at that third-party app with history attached, so the checkbox is
 * off until somebody ticks it, and the request says so explicitly rather than
 * omitting the field and letting a default on the far side decide.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { DisconnectDialog } from "./DisconnectDialog.tsx";

type Sent = { method: string; path: string; body: unknown };

function stubFetch(sent: Sent[], status = 202) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      return new Response(JSON.stringify({ key: "github", disconnecting: true }), { status });
    }),
  );
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  // RESTORED HERE rather than at the end of the test that installs them: a
  // fake clock left running by a FAILING test makes every later test in the
  // file fail too, which hides the one real failure behind a page of noise.
  vi.useRealTimers();
  localStorage.clear();
});

test("the accounts are kept unless the box is ticked", async () => {
  const sent: Sent[] = [];
  stubFetch(sent);
  render(
    <DisconnectDialog name="GitHub" kinds={["github"]} onClose={() => {}} onDone={() => {}} />,
  );

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  await waitFor(() => expect(sent.length).toBe(1));

  expect(sent[0]!.method).toBe("DELETE");
  expect(sent[0]!.path).toBe("/setup/integrations/github");
  expect(sent[0]!.body).toEqual({ remove_seats: false, force: false });
});

test("ticking the box asks for the accounts too", async () => {
  const sent: Sent[] = [];
  stubFetch(sent);
  render(
    <DisconnectDialog name="GitHub" kinds={["github"]} onClose={() => {}} onDone={() => {}} />,
  );

  fireEvent.click(screen.getByRole("checkbox"));
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  await waitFor(() => expect(sent.length).toBe(1));
  expect(sent[0]!.body).toEqual({ remove_seats: true, force: false });
});

// FORCE IS NOT OFFERED UP FRONT. It leaves the third-party app holding things
// nobody will remove, so it appears only once a teardown has actually failed —
// otherwise it is just the easier button beside the correct one.
test("forcing is offered only after a failure", async () => {
  const sent: Sent[] = [];
  stubFetch(sent, 503);
  render(
    <DisconnectDialog name="GitHub" kinds={["github"]} onClose={() => {}} onDone={() => {}} />,
  );

  expect(screen.queryByRole("button", { name: /anyway/i })).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  const force = await screen.findByRole("button", { name: /anyway/i });

  fireEvent.click(force);
  await waitFor(() => expect(sent.length).toBe(2));
  expect(sent[1]!.body).toEqual({ remove_seats: false, force: true });
});

// A TEARDOWN FAILS IN THE LOOP, NOT IN THE REQUEST.
//
// Forcing used to appear only after the request itself failed — but the
// request answers 202 and the teardown runs minutes later, so an app that
// refuses permanently (a revoked token, an instance that is gone) left the
// card reading Disconnecting for ever with no way out of it from the screen.
// A stuck surface opens the dialog already showing why, with the way out.
test("a disconnect stuck at the app offers the way out immediately", async () => {
  const sent: Sent[] = [];
  stubFetch(sent);
  render(
    <DisconnectDialog
      name="Jira"
      kinds={["jira"]}
      stuck="jira: GET /rest/webhooks/1.0/webhook: 401: Client must be authenticated"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  // The app's own words, and the sentence that says what happened.
  expect(screen.getByText(/401: Client must be authenticated/)).toBeTruthy();
  expect(screen.getByText(/has not let the engine finish it/)).toBeTruthy();

  const force = screen.getByRole("button", { name: /anyway/i });
  fireEvent.click(force);
  await waitFor(() => expect(sent.length).toBe(1));
  expect(sent[0]!.body).toEqual({ remove_seats: false, force: true });
});

// AND A DISCONNECT THAT IS MERELY RUNNING DOES NOT. Offering the way out of a
// teardown that is working invites somebody to abandon it a second after
// asking for it.
test("a disconnect in flight does not offer forcing", () => {
  const sent: Sent[] = [];
  stubFetch(sent);
  render(<DisconnectDialog name="Jira" kinds={["jira"]} onClose={() => {}} onDone={() => {}} />);
  expect(screen.queryByRole("button", { name: /anyway/i })).toBeNull();
});

// WHO IS LEFT, AND WHERE.
//
// The engine uninstalls each agent's app, which stops it acting at once, and
// cannot delete the app itself: neither GitHub nor Slack offers that at any
// permission this engine could hold. So the agents are named, as agents rather
// than as handles inside a sentence, each with the page that finishes it off
// and one line saying what to click when it opens.
test("removing the accounts names each agent and where to delete its app", () => {
  render(
    <DisconnectDialog
      name="GitHub"
      kinds={["github"]}
      apps={[
        {
          handle: "swe",
          name: "Agent SWE",
          url: "https://github.com/settings/apps/acme-swe/advanced",
        },
        {
          handle: "sre",
          name: "SRE Lead",
          url: "https://github.com/settings/apps/acme-sre/advanced",
        },
      ]}
      appPath="Delete GitHub App"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  // NOTHING UNTIL IT IS ASKED FOR. The checkbox is the decision; the roster
  // is what that decision leaves an operator holding.
  expect(screen.queryByText("Agent SWE")).toBeNull();

  fireEvent.click(screen.getByRole("checkbox"));
  expect(screen.getByText("Agent SWE")).toBeDefined();
  expect(screen.getByText("SRE Lead")).toBeDefined();
  expect(screen.getAllByText("Link to delete").length).toBe(2);
  // THE AGENT, not the handle: a colleague is a name, and "swe" is a config
  // key.
  expect(screen.queryByText(/Delete swe's app/)).toBeNull();
  // AND THE LAST CLICK, because the link lands on a settings page whose
  // delete control is at the bottom under a heading named nothing like it.
  expect(screen.getByText(/Delete GitHub App/)).toBeDefined();

  const links = screen.getAllByRole("link");
  expect(links[0]!.getAttribute("href")).toBe("https://github.com/settings/apps/acme-swe/advanced");
});

// AN APP THE ENGINE REMOVES ITSELF HANDS OVER NOTHING.
test("an integration with nothing to hand over renders no roster", () => {
  render(
    <DisconnectDialog name="GitLab" kinds={["gitlab"]} onClose={() => {}} onDone={() => {}} />,
  );
  fireEvent.click(screen.getByRole("checkbox"));
  expect(screen.queryByText("Link to delete")).toBeNull();
});

/**
 * Names a response carries, so a test can drive the list this dialog was
 * throwing away.
 */
function stubFetchWithOrphans(sent: Sent[], byKind: Record<string, string[]>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      sent.push({
        method: init?.method ?? "GET",
        path,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      const kind = path.split("/").pop() ?? "";
      return new Response(
        JSON.stringify({ key: kind, disconnecting: true, orphaned_secrets: byKind[kind] ?? [] }),
        { status: 202 },
      );
    }),
  );
}

// THE CREDENTIALS LEFT BEHIND ARE SHOWN, which is the whole point of naming
// them rather than deleting them.
//
// The engine does not remove a company's sealed values on a disconnect — one
// an operator shares with another deployment is not something a button decides
// about — and it has always returned the list saying which survived. This
// dialog discarded every response and closed, so nobody ever saw one. Seven
// credentials survived a real disconnect and not one was named on screen.
test("the credentials left in the store are named on screen", async () => {
  const sent: Sent[] = [];
  stubFetchWithOrphans(sent, {
    jira: ["SRE_ATLASSIAN"],
    atlassian: ["ORG_KEY", "SRE_EMAIL", "SRE_ATLASSIAN"],
  });
  const closed = vi.fn();
  render(
    <DisconnectDialog
      name="Atlassian"
      kinds={["jira", "atlassian"]}
      onClose={closed}
      onDone={() => {}}
    />,
  );

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));

  // UNIONED ACROSS THE SURFACES, and deduplicated: one card is several
  // requests, and a seat's token is named by every product that reads it.
  await waitFor(() => expect(screen.getByText("ORG_KEY")).toBeTruthy());
  expect(screen.getAllByText("SRE_ATLASSIAN")).toHaveLength(1);
  expect(screen.getByText("SRE_EMAIL")).toBeTruthy();
  // AND THE GESTURE THAT REMOVES THEM, because a list nobody can act on is
  // not better than no list.
  expect(screen.getByText(/crewlet secrets unset/)).toBeTruthy();
  // HELD OPEN. Closing is what lost this in the first place.
  expect(closed).not.toHaveBeenCalled();
});

// AND A DISCONNECT THAT LEAVES NOTHING JUST CLOSES, rather than showing an
// empty list over a company with nothing left to do.
test("a disconnect that leaves nothing behind closes", async () => {
  const sent: Sent[] = [];
  stubFetchWithOrphans(sent, {});
  const closed = vi.fn();
  render(<DisconnectDialog name="GitHub" kinds={["github"]} onClose={closed} onDone={() => {}} />);

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));

  await waitFor(() => expect(closed).toHaveBeenCalled());
});

/**
 * A BUSY SURFACE IS WAITED OUT, AND THE REST OF THE CARD STILL GOES.
 *
 * A reconcile tick or an operator's own pass holds a surface while it runs,
 * and the engine answers `surface_busy` — the one refusal here that clears on
 * its own. This dialog stopped at the first refusal of any kind, so a
 * collision on the second of Atlassian's three surfaces left the tool half
 * disconnected with nothing retrying: measured on a live disconnect, where
 * the same gesture minutes later completed cleanly.
 *
 * RETRIED RATHER THAN SKIPPED, because the order is load-bearing: the
 * organization's credential is what removes the accounts, so carrying on past
 * a busy product would take it away from one that still needs it.
 */
test("a surface that is busy is retried rather than abandoning the rest", async () => {
  vi.useFakeTimers();
  const sent: Sent[] = [];
  let refusals = 2;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      sent.push({
        method: init?.method ?? "GET",
        path,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      if (path.endsWith("/jira") && refusals > 0) {
        refusals--;
        return new Response(
          JSON.stringify({ error: "surface_busy", detail: "try again in a moment" }),
          { status: 503 },
        );
      }
      return new Response(JSON.stringify({ key: "x", disconnecting: true }), { status: 202 });
    }),
  );

  const done = vi.fn();
  render(
    <DisconnectDialog
      name="Atlassian"
      kinds={["jira", "confluence", "atlassian"]}
      onClose={() => {}}
      onDone={done}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));

  // THE WAIT IS SAID, and not as an error: the disconnect has not failed, it
  // has not started.
  await vi.waitFor(() => expect(screen.getByText(/has to wait its turn/)).toBeTruthy());
  await vi.advanceTimersByTimeAsync(10_000);

  await vi.waitFor(() => expect(done).toHaveBeenCalled());
  const asked = sent.filter((s) => s.method === "DELETE").map((s) => s.path);
  for (const kind of ["jira", "confluence", "atlassian"]) {
    expect(asked.some((p) => p.endsWith(`/${kind}`))).toBe(true);
  }
  // AND THE ORDER HELD: the organization is last, after the product that had
  // to wait.
  expect(asked[asked.length - 1]).toMatch(/atlassian$/);
});

/**
 * AND A REFUSAL THAT WILL NOT CHANGE IS STILL TERMINAL.
 *
 * Retrying a node with no fleet status store is a loop with no exit, and the
 * operator needs the banner and the way out rather than a spinner.
 */
test("a refusal that is not a race stops and offers the force", async () => {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      return new Response(
        JSON.stringify({ error: "no_status_store", detail: "this node has no fleet status store" }),
        { status: 503 },
      );
    }),
  );

  render(
    <DisconnectDialog name="Jira" kinds={["jira"]} onClose={() => {}} onDone={() => {}} />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));

  await waitFor(() => expect(screen.getByText(/no fleet status store/)).toBeTruthy());
  expect(screen.queryByText(/has to wait its turn/)).toBeNull();
  expect(sent.filter((s) => s.method === "DELETE")).toHaveLength(1);
});
