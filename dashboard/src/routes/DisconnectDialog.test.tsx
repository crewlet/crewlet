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
        { handle: "swe", name: "Agent SWE", url: "https://github.com/settings/apps/acme-swe" },
        { handle: "sre", name: "SRE Lead", url: "https://github.com/settings/apps/acme-sre" },
      ]}
      appPath="Advanced > Delete GitHub App"
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
  // AND THE LAST CLICKS, because the link lands on a settings page whose
  // delete control is at the bottom under a heading named nothing like it.
  expect(screen.getByText(/Delete GitHub App/)).toBeDefined();

  const links = screen.getAllByRole("link");
  expect(links[0]!.getAttribute("href")).toBe("https://github.com/settings/apps/acme-swe");
});

// AN APP THE ENGINE REMOVES ITSELF HANDS OVER NOTHING.
test("an integration with nothing to hand over renders no roster", () => {
  render(
    <DisconnectDialog name="GitLab" kinds={["gitlab"]} onClose={() => {}} onDone={() => {}} />,
  );
  fireEvent.click(screen.getByRole("checkbox"));
  expect(screen.queryByText("Link to delete")).toBeNull();
});
