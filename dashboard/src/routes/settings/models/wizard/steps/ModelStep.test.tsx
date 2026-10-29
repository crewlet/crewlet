/**
 * The Model step: the CLI's own aliases as choices, and "Another model" for
 * any other id.
 *
 * The invariant: an edit opens on the choice its model is, whenever the
 * aliases arrive. They come from a query that answers after the first render,
 * and the step decided once, on that render, that `haiku` was "another
 * model". Until the reader chooses, the choice is read from the aliases as
 * they stand; once they choose, it is theirs.
 */

import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ModelsAnswer } from "~/protocol/models.ts";
import { emptyDraft, type Draft } from "../draft.ts";
import { ModelStep } from "./ModelStep.tsx";

const ALIASES = [
  { id: "opus", name: "Opus", detail: "The CLI's latest Opus" },
  { id: "sonnet", name: "Sonnet", detail: "The CLI's latest Sonnet" },
  { id: "haiku", name: "Haiku", detail: "The CLI's latest Haiku" },
];

const listing = vi.hoisted(() => ({ answer: null as unknown }));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({
    data: listing.answer,
    loading: listing.answer === null,
    error: null,
    refusal: null,
    refetch: () => {},
  }),
}));

const answered: ModelsAnswer = {
  source: "models.dev",
  fetched_at: null,
  provider: "anthropic",
  aliases: ALIASES,
  models: [{ id: "claude-haiku-4-5", name: "Claude Haiku 4.5" }],
  docs_url: "https://example.com/models",
} as unknown as ModelsAnswer;

let latest: Draft;
function Step({ model }: { model: string }) {
  const [draft, setDraft] = useState<Draft>({ ...emptyDraft(), key: "fast", model });
  latest = draft;
  return (
    <ModelStep
      draft={draft}
      error={undefined}
      onChange={(next) => setDraft((d) => ({ ...d, model: next }))}
    />
  );
}

/** Opens the step as an edit does: the aliases answer only after the first render. */
function openEdit(model: string) {
  listing.answer = null;
  const view = render(<Step model={model} />);
  listing.answer = answered;
  view.rerender(<Step model={model} />);
}

const checked = (name: string | RegExp) =>
  (screen.getByRole("radio", { name }) as HTMLInputElement).checked;

afterEach(() => {
  cleanup();
  listing.answer = null;
});

test.each(ALIASES)(
  "an edit of a model on $id opens on $name, however late the aliases answer",
  ({ id, name }) => {
    openEdit(id);
    expect(checked(new RegExp(`^${name}`))).toBe(true);
    expect(checked(/^Another model/)).toBe(false);
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(latest.model).toBe(id);
  },
);

test("a full model id opens on Another model, named as the catalogue names it and said to be a fixed version", () => {
  openEdit("claude-haiku-4-5");
  expect(checked(/^Another model/)).toBe(true);
  expect(checked(/^Haiku/)).toBe(false);
  expect((screen.getByRole("combobox") as HTMLInputElement).value).toBe("claude-haiku-4-5");
  expect(screen.getByText("Claude Haiku 4.5, a fixed version.")).toBeTruthy();
  expect(screen.queryByText("Any model id the CLI takes.")).toBeNull();
});

test("an id the catalogue does not list is described as any id the CLI takes", () => {
  openEdit("claude-experimental-x");
  expect(checked(/^Another model/)).toBe(true);
  expect(screen.getByText("Any model id the CLI takes.")).toBeTruthy();
  expect(screen.queryByText(/a fixed version/)).toBeNull();
});

test("once the reader chooses, the choice is theirs: Another model stays chosen while its field is empty", () => {
  openEdit("haiku");
  fireEvent.click(screen.getByRole("radio", { name: /^Another model/ }));
  expect(latest.model).toBe("");
  expect(checked(/^Another model/)).toBe(true);
  fireEvent.click(screen.getByRole("radio", { name: /^Sonnet/ }));
  expect(latest.model).toBe("sonnet");
  expect(checked(/^Sonnet/)).toBe(true);
});
