/**
 * A server's OAuth sign-in from the page: the Sign in dialog, the Revoke
 * dialog, and a client's secret stored as one. What each sends, in what order
 * against the browser, and what it says of the engine's answer.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import type { McpServerAnswer, McpSignIn } from "~/contract/mcp.ts";
import { readSignIn, signOut, startSignIn, storeValue } from "../api.ts";
import type { McpData } from "../data.ts";
import { LINEAR_DETAIL, LINEAR_OAUTH, SERVICE_OAUTH, mcpData, withOAuth } from "../fixtures.ts";
import { useFlow } from "../parts.tsx";
import { Flows } from "./Flows.tsx";
import { SIGN_IN_POLL_MS } from "./SignInDialog.tsx";

const CONSENT = "https://auth.linear.app/authorize?client_id=registered-client&state=signed";

const pending: McpSignIn = {
  id: "018f3a9c-0000-7000-8000-0000000000aa",
  server: "linear",
  status: "pending",
  detail: "",
  started_at: "2026-11-01T12:00:00Z",
  expires_at: "2026-11-01T12:15:00Z",
  authorize_url: CONSENT,
};

/** Linear's answer with its client's secret listed, unset. */
const WITH_SECRET: McpServerAnswer = {
  ...LINEAR_DETAIL,
  values: [
    {
      key: "oauth.client_secret",
      as: "oauth",
      variable: "MCP_LINEAR_OAUTH_CLIENT_SECRET",
      set: false,
      stored_at: null,
      stored_by: null,
      optional: false,
      bound_by: null,
    },
  ],
};

/** The order the browser and the engine were asked in. */
let asked: string[] = [];

vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
  readServer: vi.fn(async () => WITH_SECRET),
  startSignIn: vi.fn(async () => {
    asked.push("start");
    return { kind: "ok", status: 201, value: pending };
  }),
  readSignIn: vi.fn(),
  signOut: vi.fn(),
  storeValue: vi.fn(async () => ({
    kind: "ok",
    status: 200,
    value: {
      server: "linear",
      key: "oauth.client_secret",
      seat: "",
      variable: "MCP_LINEAR_OAUTH_CLIENT_SECRET",
      tested: true,
      pointed: true,
      revision_id: "r2",
      reload: { active: true, deferred: false },
      values: [],
    },
  })),
}));

function Host({ data }: { data: McpData }) {
  const flow = useFlow();
  return <Flows data={data} name="linear" flow={flow} onRemoved={() => {}} />;
}

function mountAt(hash: string, data: McpData) {
  location.hash = hash;
  render(
    <TestClient>
      <ToastProvider>
        <LayerHost>
          <Router>
            <Host data={data} />
          </Router>
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

/** A tab as `window.open` hands one back, recording where it is sent. */
function openedTab() {
  const tab = { opener: {} as unknown, location: { href: "" }, close: vi.fn() };
  vi.spyOn(window, "open").mockImplementation(() => {
    asked.push("open");
    return tab as unknown as Window;
  });
  return tab;
}

const signedIn = (status: McpSignIn["status"], detail = "") => ({
  kind: "ok" as const,
  status: 200,
  value: { ...pending, status, detail, authorize_url: "" },
});

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  asked = [];
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  location.hash = "";
});

// THE TAB IS OPENED ON THE CLICK AND POINTED ONCE ANSWERED: a browser lets a
// tab through that a click opens, not one an answer does. Severed from the
// page, sent to the consent address the engine answered, and the dialog says
// connected only once the engine reads the sign-in approved. Mutation: open
// the tab after the start answers, keep its opener, or call it connected on
// the start, and this is red.
test("a sign-in opens its tab on the click, sends it to the vendor, and waits for the engine's approval", async () => {
  const tab = openedTab();
  vi.mocked(readSignIn)
    .mockResolvedValueOnce(signedIn("pending"))
    .mockResolvedValueOnce(signedIn("approved"));
  mountAt(
    "#/settings/mcp-tools?flow=signin&server=linear",
    withOAuth(mcpData("admin"), "linear", {
      ...LINEAR_OAUTH,
      status: "needs_sign_in",
    }),
  );
  expect(screen.getByText("Sign in to Linear")).toBeDefined();
  expect(
    screen.getByText(/Signing in as a client registered automatically, asking for read and write/),
  ).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Continue to Linear" }));
  await afterMicrotasks();
  expect(asked).toEqual(["open", "start"]);
  expect(window.open).toHaveBeenCalledWith("about:blank", "_blank");
  expect(tab.opener).toBeNull();
  expect(tab.location.href).toBe(CONSENT);
  expect(screen.getByText("Waiting for Linear to confirm the sign-in.")).toBeDefined();
  expect(screen.queryByText("Linear is connected")).toBeNull();

  await act(async () => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
  expect(screen.queryByText("Linear is connected")).toBeNull();
  await act(async () => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
  expect(screen.getByText("Linear is connected")).toBeDefined();
  expect(readSignIn).toHaveBeenCalledWith(pending.id, expect.anything());
});

// A TAB THE BROWSER HOLDS BACK is offered as a link to the same consent page,
// and the sign-in still waits for the engine.
test("a tab the browser holds back is offered as a link to the vendor's page", async () => {
  vi.spyOn(window, "open").mockReturnValue(null);
  vi.mocked(readSignIn).mockResolvedValue(signedIn("pending"));
  mountAt(
    "#/settings/mcp-tools?flow=signin&server=linear",
    withOAuth(mcpData("admin"), "linear", LINEAR_OAUTH),
  );
  expect(screen.getByText("Sign in to Linear again")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Continue to Linear" }));
  await afterMicrotasks();
  expect(screen.getByText("Your browser kept the sign-in from opening")).toBeDefined();
  expect(screen.getByRole("link", { name: "Open Linear's sign-in" }).getAttribute("href")).toBe(
    CONSENT,
  );
});

// A START THE ENGINE REFUSES closes the tab it opened, and a sign-in the
// vendor or the engine fails says why in their words, with Try again.
test("a refused start closes its tab and a failed sign-in says why", async () => {
  const tab = openedTab();
  vi.mocked(startSignIn).mockResolvedValueOnce({
    kind: "refused",
    status: 409,
    code: "conflict",
    sentence: "This engine has no public address for a sign-in: set api.external_url.",
    body: {},
  });
  mountAt(
    "#/settings/mcp-tools?flow=signin&server=linear",
    withOAuth(mcpData("admin"), "linear", LINEAR_OAUTH),
  );
  fireEvent.click(screen.getByRole("button", { name: "Continue to Linear" }));
  await afterMicrotasks();
  expect(tab.close).toHaveBeenCalled();
  expect(screen.getByText(/no public address for a sign-in/)).toBeDefined();

  vi.mocked(readSignIn).mockResolvedValue(
    signedIn("failed", "the vendor refused the code: invalid_grant"),
  );
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  await afterMicrotasks();
  await act(async () => vi.advanceTimersByTimeAsync(SIGN_IN_POLL_MS));
  expect(screen.getByText("The sign-in did not finish")).toBeDefined();
  expect(screen.getByText("the vendor refused the code: invalid_grant")).toBeDefined();
});

// A SERVICE IDENTITY IS NOBODY'S SIGN-IN: the engine refuses one, so the
// page opens none, and a grant nobody holds has nothing to revoke.
test("a service identity opens no sign-in, and a grant nobody holds no revocation", async () => {
  mountAt(
    "#/settings/mcp-tools?flow=signin&server=linear",
    withOAuth(mcpData("admin"), "linear", SERVICE_OAUTH),
  );
  expect(screen.queryByRole("dialog")).toBeNull();
  cleanup();
  mountAt(
    "#/settings/mcp-tools?flow=revoke&server=linear",
    withOAuth(mcpData("admin"), "linear", {
      ...LINEAR_OAUTH,
      status: "needs_sign_in",
    }),
  );
  expect(screen.queryByRole("dialog")).toBeNull();
});

// THE ENGINE FORGETS THE GRANT WHATEVER THE VENDOR SAID: a revocation the
// vendor refused is still a sign-out, said as one, with the vendor's words
// and what is left to do there.
test("a revocation the vendor refused still signs out, and says to revoke it there", async () => {
  vi.mocked(signOut).mockResolvedValueOnce({
    kind: "ok",
    status: 200,
    value: { server: "linear", revocation: "failed", said: "the revocation endpoint answered 503" },
  });
  mountAt(
    "#/settings/mcp-tools?flow=revoke&server=linear",
    withOAuth(mcpData("admin"), "linear", LINEAR_OAUTH),
  );
  expect(screen.getByText("Revoke Linear's sign-in?")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
  await afterMicrotasks();
  expect(signOut).toHaveBeenCalledWith("linear");
  expect(screen.getAllByText("Signed out of Linear, not revoked there").length).toBeGreaterThan(0);
  expect(screen.getAllByText(/the revocation endpoint answered 503/).length).toBeGreaterThan(0);
});

// A CLIENT'S SECRET IS NAMED AS ONE, and tested only where a test can use it:
// a service identity signs in with it, while a person's sign-in sends it only
// when the grant renews, so that one is stored untested. Mutation: test every
// client secret, and the engine's 400 is what a person's sign-in meets.
test("a client's secret is tested for a service identity and stored untested for a person's sign-in", async () => {
  for (const [oauth, test] of [
    [LINEAR_OAUTH, false],
    [SERVICE_OAUTH, true],
  ] as const) {
    mountAt(
      "#/settings/mcp-tools?flow=value&server=linear&key=oauth.client_secret",
      withOAuth(mcpData("admin"), "linear", oauth),
    );
    await afterMicrotasks();
    expect(screen.getByText("Set the client secret")).toBeDefined();
    fireEvent.change(screen.getByLabelText("New value"), { target: { value: "secret-not-real" } });
    fireEvent.click(screen.getByRole("button", { name: test ? "Test and save" : "Save" }));
    await afterMicrotasks();
    expect(storeValue).toHaveBeenLastCalledWith(
      "linear",
      "oauth.client_secret",
      "secret-not-real",
      {
        test,
      },
    );
    cleanup();
  }
});
