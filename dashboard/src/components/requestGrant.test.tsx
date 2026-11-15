/**
 * An agent's request for one integration permission, answered from its
 * question (ADR-0118). The invariants: choosing a grant option shows the grant
 * built from the stored request and sends nothing; Grant saves it on the
 * integration's Agents tab, with the request's id, and only then answers the
 * question; declining answers at once and saves nothing.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { AnswerAsk } from "./writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { withDerived } from "~/test/org.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));
const get = vi.hoisted(() => vi.fn());
const put = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/rest.ts", async (original) => {
  const real = await original<typeof import("~/protocol/rest.ts")>();
  return { ...real, rest: { ...real.rest, get, put } };
});

const JANE = {
  login: "founder",
  grants: ["admin", "worklet"],
  admin: true,
  worklet: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: ["comment_on_work_item"],
  kind: "human",
  loading: false,
  configWriter: true,
  configManagedBy: [],
} as unknown as ViewerState;

const ORG = withDerived({
  name: "Nimbus",
  roles: [
    { name: "SRE", handle: "sre", kind: "agent" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
});

const REQUEST = {
  id: "abcdefghij",
  kind: "cloudflare",
  lane: "cloudflare",
  handle: "sre",
  permission: "Origin Write",
  scope: ["example.com"],
  hours: 4,
  reason: "Cloudflare refused creating the origin rule with 403.",
  item: "NIM-7",
  ask: "jane",
  asked_at: "2026-11-15T09:00:00Z",
};

const ACCESS = {
  kind: "cloudflare",
  revision: "rev-3",
  syncing: false,
  options: [
    {
      lane: "cloudflare",
      live: true,
      levels: [{ id: "secure", name: "Secure" }],
      grants: [
        {
          id: "Origin Write",
          name: "Origin Rules: Edit",
          group: "Zone",
          scope_kind: "zone",
          write: true,
        },
      ],
      grant_scope: ["example.com"],
    },
  ],
  seats: [
    {
      handle: "sre",
      name: "SRE",
      lanes: {
        cloudflare: {
          desired: { enabled: true, level: "secure" },
          observed: { exists: true, enabled: true },
        },
      },
    },
  ],
};

let sent: Record<string, unknown>[];

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(JANE);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(ORG as never);
  get.mockImplementation(async (path: string) =>
    path.startsWith("/setup/access/requests/") ? REQUEST : ACCESS,
  );
  put.mockResolvedValue(ACCESS);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init: RequestInit) => {
      sent.push((JSON.parse(init.body as string) as { args: Record<string, unknown> }).args);
      return new Response(
        JSON.stringify({ outcome: "applied", position: "CREWLET_TRACKER_LOG@1:3" }),
        {
          status: 200,
          headers: { "Content-Type": "application/json" },
        },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  get.mockReset();
  put.mockReset();
});

function mount() {
  render(
    <ToastProvider>
      <LayerHost>
        <AnswerAsk
          item="NIM-7"
          comment="c-1"
          asker="SRE"
          askedBy="sre"
          question="Grant SRE Origin Rules: Edit on example.com at Cloudflare?"
          options={[
            { id: "grant-rabcdefghij-t", label: "Grant for this change (expires in 4 hours)" },
            { id: "grant-rabcdefghij-p", label: "Add to this agent permanently" },
            { id: "decline", label: "Not now" },
          ]}
          recommended="grant-rabcdefghij-t"
          open={false}
          onOpenChange={() => {}}
          onAnswered={() => {}}
          layout="buttons"
        />
      </LayerHost>
    </ToastProvider>,
  );
}

test("choosing a grant shows the grant built from the request and answers nothing yet", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Grant for this change/ }));
  expect(await screen.findByText("Origin Rules: Edit (Origin Write)")).toBeTruthy();
  expect(screen.getByText("4 hours")).toBeTruthy();
  expect(screen.getByText(/asks you to sign in again/)).toBeTruthy();
  expect(get).toHaveBeenCalledWith("/setup/access/requests/abcdefghij", expect.anything());
  expect(sent).toHaveLength(0);
  expect(put).not.toHaveBeenCalled();
});

test("Grant saves the grant with the request's id, and only then answers the question", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Add to this agent permanently/ }));
  await screen.findByText("Origin Rules: Edit (Origin Write)");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Grant" }));
  });
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(put).toHaveBeenCalledOnce();
  const [path, body, headers] = put.mock.calls[0]!;
  expect(path).toBe("/setup/integrations/cloudflare/access");
  expect(headers).toEqual({ "If-Match": '"rev-3"' });
  const grant = (body as { seats: { sre: { cloudflare: { grants: Record<string, unknown>[] } } } })
    .seats.sre.cloudflare.grants[0]!;
  expect(grant).toMatchObject({
    permission: "Origin Write",
    scope: ["example.com"],
    request: "abcdefghij",
  });
  expect(grant.expires_at).toBeUndefined();
  expect(sent[0]).toMatchObject({ item: "NIM-7", answers: "c-1", choice: "grant-rabcdefghij-p" });
});

test("declining answers at once and saves nothing", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Not now/ }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ choice: "decline" });
  expect(put).not.toHaveBeenCalled();
});
