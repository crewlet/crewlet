/**
 * The share dialog: a stored share that reaches nobody is listed with why and
 * offered for removal, never hidden.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { ArtifactShare } from "./Share.tsx";
import { Router } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { keyedWrite } from "~/lib/keyedWrite.ts";
import { invalidShareWords } from "~/lib/artifacts/words.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { eventually } from "~/test/settle.ts";

vi.mock("~/lib/useQuery.ts", () => ({ useQuery: vi.fn() }));
vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return { ...actual, keyedWrite: vi.fn(async () => ({ kind: "done", body: {} })) };
});
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useOrg: () => null };
});
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "jane", acts: [] }) };
});

afterEach(cleanup);

const ID = "0192f2c4-3333-7000-8000-000000000009";
const LOGIN = { kind: "seat", id: "token:maya" } as const;

test("the sentence names the sign-in and says what to do instead", () => {
  expect(invalidShareWords(LOGIN)).toBe(
    "token:maya is a sign-in, not a seat. Remove it and share with their seat.",
  );
});

// A SHARE AN EARLIER ENGINE STORED TO A SIGN-IN reaches nobody. Hidden, it was
// a record its owner could neither see nor remove.
test("a share the engine marks invalid is listed with why, and Remove withdraws it", async () => {
  vi.mocked(useQuery).mockReturnValue({
    data: {
      owner: "jane",
      owner_kind: "human",
      role: "owner",
      share_count: 1,
      shares: [{ audience: LOGIN, role: "viewer", invalid: true }],
    },
    loading: false,
    error: null,
    refusal: null,
    refetch: vi.fn(),
  } as never);
  render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <ArtifactShare
            detail={{ id: ID, title: "Plan", role: "owner" } as unknown as ArtifactDetail}
            open
            onClose={() => {}}
            onChanged={() => {}}
          />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
  const row = screen.getByText(invalidShareWords(LOGIN)).closest("li") as HTMLElement;
  expect(within(row).getByText("No access")).toBeTruthy();
  expect(within(row).queryByRole("combobox")).toBeNull();
  fireEvent.click(within(row).getByRole("button", { name: /^Remove / }));
  await eventually(() => expect(keyedWrite).toHaveBeenCalled());
  const req = vi.mocked(keyedWrite).mock.calls[0]![0];
  expect(req.method).toBe("DELETE");
  // ONE SEGMENT, its colons escaped: the engine's router reads it back as `seat:token:maya`.
  expect(req.path).toBe(`/artifacts/${ID}/shares/seat%3Atoken%3Amaya`);
});
