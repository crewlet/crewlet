/**
 * Settings › File storage holds what the company keeps against its storage
 * quotas: each a figure over its ceiling with a bar, and the key that sets it.
 *
 * Its own file because it mocks the screen's query hook.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { FileStoragePage } from "./FileStorage.tsx";

const answers = vi.hoisted(() => ({ usage: true }));

vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (what: string) => {
    const ok = (data: unknown) => ({ data, loading: false, error: null, refusal: null });
    if (what === "fleet") return ok({ objects: undefined });
    if (what === "artifact_usage") {
      return answers.usage
        ? ok({
            usage: { bytes: 110, artifacts: 1 },
            limits: { company_bytes: 10 * 1024 ** 3, artifacts: 50_000 },
          })
        : { data: null, loading: false, error: "unavailable", refusal: null };
    }
    if (what === "artifact_data_usage") {
      return ok({ usage: { footprint: 0 }, data_bytes: 1024 ** 3 });
    }
    return ok(null);
  },
}));

afterEach(() => {
  cleanup();
  answers.usage = true;
});

function mount() {
  return render(
    <Router>
      <FileStoragePage />
    </Router>,
  );
}

test("each quota is a figure over its ceiling, with a bar and its key", () => {
  mount();
  const card = screen.getByText("Storage quotas").closest(".crewlet-card") as HTMLElement;
  expect(within(card).getAllByRole("meter")).toHaveLength(3);
  for (const key of [
    "artifacts.limits.company_bytes",
    "artifacts.limits.artifacts",
    "artifacts.limits.data_bytes",
  ]) {
    expect(within(card).getByText(key)).toBeTruthy();
  }
  expect(within(card).getByText("of 50k")).toBeTruthy();
});

test("a quota read that did not answer draws no figure", () => {
  answers.usage = false;
  mount();
  expect(screen.queryByText("Storage quotas")).toBeNull();
  expect(screen.queryAllByRole("meter")).toHaveLength(0);
});
