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
 *
 * `queued` is which of the route's two answers to give, because they are not
 * the same event: 202 with `disconnecting: true` records the intent and leaves
 * the teardown to the reconcile loop, while 200 with `removed: true` is the
 * forced path, where the block is already gone and nothing else will run.
 */
function stubFetchWithOrphans(sent: Sent[], byKind: Record<string, string[]>, queued = true) {
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
        JSON.stringify(
          queued
            ? { key: kind, disconnecting: true, orphaned_secrets: byKind[kind] ?? [] }
            : { key: kind, removed: true, orphaned_secrets: byKind[kind] ?? [] },
        ),
        { status: queued ? 202 : 200 },
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

/*
 * A QUEUED DISCONNECT DOES NOT TELL ANYBODY TO REVOKE ANYTHING YET.
 *
 * The ordinary path answers 202: the intent is recorded and the reconcile loop
 * performs the teardown afterwards, authenticating with the very credentials
 * this dialog lists. Read as completion, it put those names under the word
 * "disconnected" and said to revoke each one — and an operator who does leaves
 * the teardown unable to sign in. It retries, so the card sits in
 * Disconnecting for ever over accounts that are still live and hooks that
 * still deliver.
 */
test("a queued disconnect says to wait before revoking anything", async () => {
  const sent: Sent[] = [];
  stubFetchWithOrphans(sent, { atlassian: ["ORG_KEY"] });
  render(
    <DisconnectDialog
      name="Atlassian"
      kinds={["atlassian"]}
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));

  await waitFor(() => expect(screen.getByText("ORG_KEY")).toBeTruthy());
  // NOT "disconnected", because it is not: the teardown has not run.
  expect(screen.queryByText("Atlassian disconnected")).toBeNull();
  expect(screen.getByText("Disconnecting Atlassian")).toBeTruthy();
  // AND THE ORDER IS STATED, which is the part that keeps the teardown able
  // to authenticate.
  expect(screen.getByText(/Wait until the card stops reporting Disconnecting/)).toBeTruthy();
});

/*
 * AND A FORCED ONE SAYS TO GET ON WITH IT.
 *
 * Forcing drops the block there and then and answers `removed: true`. Nothing
 * else is going to run, so whatever the app still holds is the operator's now
 * and there is nothing left to wait for — telling them to wait would be
 * telling them to wait for something that will never happen.
 */
test("a forced disconnect says to revoke now", async () => {
  const sent: Sent[] = [];
  stubFetchWithOrphans(sent, { atlassian: ["ORG_KEY"] }, false);
  render(
    <DisconnectDialog
      name="Atlassian"
      kinds={["atlassian"]}
      stuck="the teardown keeps failing"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  fireEvent.click(screen.getByRole("button", { name: "Disconnect anyway" }));

  await waitFor(() => expect(screen.getByText("ORG_KEY")).toBeTruthy());
  expect(screen.getByText("Atlassian disconnected")).toBeTruthy();
  expect(screen.queryByText(/Wait until the card stops reporting/)).toBeNull();
  expect(screen.getByText(/crewlet secrets unset/)).toBeTruthy();
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
        // THE ENGINE'S OWN BUSY ANSWER: its code, its sentence, the
        // detail and the Retry-After the disconnect route writes.
        return new Response(
          JSON.stringify({
            error: "surface_busy",
            message:
              "Something else is writing to this integration right now. " +
              "Try again in a moment; nothing was changed.",
            detail:
              "setupapi: this surface is being written at: jira is being " +
              "provisioned right now, so the disconnect was not started; try " +
              "again in a moment",
          }),
          { status: 503, headers: { "Content-Type": "application/json", "Retry-After": "3" } },
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
 * AND A REFUSAL THAT IS NOT A RACE IS STILL TERMINAL.
 *
 * Retrying a status row the node could not write is a loop the fault does not
 * end, and the operator needs the banner and the way out rather than a spinner.
 *
 * THE ENGINE'S OWN ANSWER, verbatim: `503 unavailable` with its sentence and a
 * `Retry-After` (internal/api/setupapi's disconnect route, through
 * httpjson.UnavailableWith). It used to answer `internal_error`, and a fixture
 * still stubbing that passed while exercising an answer nothing sends — so a
 * dialog that started obeying the header here would have gone unnoticed. The
 * clock is run well past the header to prove it is not obeyed.
 */
test("a refusal that is not a race stops and offers the force", async () => {
  vi.useFakeTimers();
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
        JSON.stringify({
          error: "unavailable",
          message:
            "This node cannot answer that yet — something it reads is still " +
            "catching up. Ask again in a moment.",
          detail:
            "setupapi: this node could not take jira to record the disconnect: " +
            "the fleet status row could not be written",
        }),
        { status: 503, headers: { "Content-Type": "application/json", "Retry-After": "2" } },
      );
    }),
  );

  render(<DisconnectDialog name="Jira" kinds={["jira"]} onClose={() => {}} onDone={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));

  await vi.waitFor(() => expect(screen.getByText(/status row could not be written/)).toBeTruthy());
  await vi.advanceTimersByTimeAsync(10_000);
  expect(screen.queryByText(/has to wait its turn/)).toBeNull();
  expect(sent.filter((s) => s.method === "DELETE")).toHaveLength(1);
});
