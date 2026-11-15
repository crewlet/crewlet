/**
 * A grant request answered from its question (ADR-0118): choosing shows the grant
 * and sends nothing; Grant sends one write, the answer; a refusal keeps it open.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { AnswerAsk } from "./writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { withDerived } from "~/test/org.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

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

test("Grant answers the question with the option chosen, in one write, and saves nothing itself", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Add to this agent permanently/ }));
  await screen.findByText("Origin Rules: Edit (Origin Write)");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Grant" }));
  });
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toEqual({ item: "NIM-7", answers: "c-1", choice: "grant-rabcdefghij-p" });
  expect(put).not.toHaveBeenCalled();
  await waitFor(() => expect(screen.queryByRole("button", { name: "Grant" })).toBeNull());
});

test("a refused answer keeps the grant open with the refusal under it", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init: RequestInit) => {
      sent.push((JSON.parse(init.body as string) as { args: Record<string, unknown> }).args);
      return new Response(JSON.stringify({ error: "unauthorized" }), {
        status: 403,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Grant for this change/ }));
  await screen.findByText("Origin Rules: Edit (Origin Write)");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Grant" }));
  });
  const dialog = screen.getByRole("dialog");
  await waitFor(() => expect(within(dialog).getByText(ACT_ERRORS.unauthorized!)).toBeTruthy());
  expect(sent).toHaveLength(1);
  expect(within(dialog).getByRole("button", { name: "Grant" })).toBeTruthy();
  expect(put).not.toHaveBeenCalled();
});

test("declining answers at once and saves nothing", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Not now/ }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ choice: "decline" });
  expect(put).not.toHaveBeenCalled();
});
