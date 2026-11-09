/**
 * An agent's access request answered (ADR-0118). The invariants: the grant's
 * button says the engine's words, never the asker's; choosing it previews the
 * exact change the engine built and sends nothing; Apply saves the grant and
 * only then answers the question; closing the preview answers nothing;
 * declining answers at once.
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

const FIX = {
  kind: "config_change",
  sentence: "Allow Ada to watch the engine's health and wake on its alarms.",
  unchanged: "Ada gets no other new access.",
  options: [
    {
      label: "Allow Ada to watch the engine's health and wake on its alarms",
      change: {
        entity: "roles",
        id: "ada",
        set: { health_tools: true },
        summary: "Allow ada to watch the engine's health",
      },
    },
  ],
};
const query = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useQuery.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useQuery.ts")>()),
  useQuery: query,
}));
const getEntity = vi.hoisted(() => vi.fn());
const dryRunEntity = vi.hoisted(() => vi.fn());
const putEntity = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getEntity,
  dryRunEntity,
  putEntity,
}));
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: () => ({ can: true }),
}));

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
    { name: "Ada", handle: "ada", kind: "agent" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
});

let sent: Record<string, unknown>[];

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(JANE);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(ORG as never);
  query.mockReturnValue({
    data: { seat: "ada", access: "grant-health", revision: "r1", fix: FIX },
  });
  getEntity.mockResolvedValue({ kind: "entity", entity: { name: "Ada" }, etag: '"roles-1"' });
  dryRunEntity.mockResolvedValue({
    kind: "valid",
    baseRevisionId: "r1",
    warnings: [],
    derived: null,
  });
  putEntity.mockResolvedValue({
    kind: "saved",
    revisionId: "r2",
    epoch: 2,
    warnings: [],
    derived: null,
  });
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
});

function mount() {
  render(
    <ToastProvider>
      <LayerHost>
        <AnswerAsk
          item="NIM-7"
          comment="c-1"
          asker="Ada"
          askedBy="ada"
          question="Allow Ada to watch the engine's health and wake on its alarms?"
          options={[
            { id: "grant-health", label: "Approve the release" },
            { id: "decline", label: "Not now" },
          ]}
          recommended="grant-health"
          open={false}
          onOpenChange={() => {}}
          onAnswered={() => {}}
          layout="buttons"
        />
      </LayerHost>
    </ToastProvider>,
  );
}

test("the grant's button says the engine's words, not the asker's", () => {
  mount();
  expect(
    screen.getByRole("button", { name: /Allow Ada to watch the engine's health/ }),
  ).toBeTruthy();
  expect(screen.queryByText("Approve the release")).toBeNull();
});

test("choosing the grant previews the change and answers nothing yet", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Allow Ada to watch/ }));
  await waitFor(() => expect(dryRunEntity).toHaveBeenCalled());
  expect(query).toHaveBeenCalledWith("advisor_access", { seat: "ada", access: "grant-health" });
  expect(sent).toHaveLength(0);
  expect(putEntity).not.toHaveBeenCalled();
});

test("apply saves the grant, and only then answers the question with it", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Allow Ada to watch/ }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy());
  await waitFor(() =>
    expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  });
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(putEntity).toHaveBeenCalledOnce();
  expect(sent[0]).toMatchObject({ item: "NIM-7", answers: "c-1", choice: "grant-health" });
});

test("closing the preview answers nothing", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Allow Ada to watch/ }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Apply" })).toBeNull());
  expect(sent).toHaveLength(0);
});

test("declining answers at once", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: /Not now/ }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ choice: "decline" });
  expect(putEntity).not.toHaveBeenCalled();
});

test("a GitHub grant sets its one permission on the entity as read, and keeps the rest", async () => {
  const pulls = {
    label: "Allow Ada to review and merge pull requests on GitHub",
    change: {
      entity: "roles",
      id: "ada",
      set: {},
      merge: { integrations: { github: { permissions: { pull_requests: "write" } } } },
      summary: "Allow ada to review and merge pull requests on GitHub",
    },
  };
  query.mockReturnValue({
    data: {
      seat: "ada",
      access: "grant-github-pull-requests",
      revision: "r1",
      fix: { ...FIX, options: [pulls] },
    },
  });
  const integrations = {
    github: { enabled: true, permissions: { contents: "write" } },
    cloudflare: { enabled: true, level: "operate" },
  };
  getEntity.mockResolvedValue({
    kind: "entity",
    entity: { name: "Ada", integrations },
    etag: '"roles-2"',
  });
  render(
    <ToastProvider>
      <LayerHost>
        <AnswerAsk
          item="NIM-7"
          comment="c-1"
          asker="Ada"
          askedBy="ada"
          options={[
            { id: "grant-github-pull-requests", label: "Allow it" },
            { id: "decline", label: "Not now" },
          ]}
          recommended="grant-github-pull-requests"
          open={false}
          onOpenChange={() => {}}
          onAnswered={() => {}}
          layout="buttons"
        />
      </LayerHost>
    </ToastProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: /review and merge pull requests/ }));
  await waitFor(() =>
    expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  });
  await waitFor(() => expect(putEntity).toHaveBeenCalledOnce());
  const [, , written, etag] = putEntity.mock.calls[0] as unknown[];
  expect(etag).toBe('"roles-2"');
  expect(written).toEqual({
    name: "Ada",
    integrations: {
      github: { enabled: true, permissions: { contents: "write", pull_requests: "write" } },
      cloudflare: { enabled: true, level: "operate" },
    },
  });
});
