/**
 * The Model step: the harness's own aliases as choices, and "Another model" for
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

// Haiku carries no version: an alias the engine cannot resolve.
const ALIASES = [
  {
    id: "opus",
    name: "Opus",
    detail: "The harness's latest Opus",
    model: "claude-opus-5-5",
    version: "5.5",
  },
  {
    id: "sonnet",
    name: "Sonnet",
    detail: "The harness's latest Sonnet",
    model: "claude-sonnet-5-5",
    version: "5.5",
  },
  { id: "haiku", name: "Haiku", detail: "The harness's latest Haiku" },
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
function Step({ model, method = "api-key" }: { model: string; method?: Draft["method"] }) {
  const [draft, setDraft] = useState<Draft>({ ...emptyDraft(), key: "fast", model, method });
  latest = draft;
  return (
    <ModelStep
      draft={draft}
      error={undefined}
      effortError={undefined}
      onChange={(next) => setDraft((d) => ({ ...d, model: next }))}
      onEffort={(effort) => setDraft((d) => ({ ...d, effort }))}
    />
  );
}

/** Opens the step as an edit does: the aliases answer only after the first render. */
function openEdit(model: string, method?: Draft["method"]) {
  listing.answer = null;
  const view = render(<Step model={model} method={method} />);
  listing.answer = answered;
  view.rerender(<Step model={model} method={method} />);
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
    expect(screen.queryByRole("combobox", { name: /Model id/ })).toBeNull();
    expect(latest.model).toBe(id);
  },
);

test("an alias names the version it resolves to beside its name, in the kit's secondary caption", () => {
  openEdit("opus");
  expect(screen.getByRole("radio", { name: /^Opus 5\.5The/ })).toBeTruthy();
  expect(screen.getByRole("radio", { name: /^Sonnet 5\.5The/ })).toBeTruthy();
  for (const label of screen.getAllByText("5.5")) {
    expect(label.className).toContain("crewlet-text--caption");
    expect(label.className).toContain("crewlet-text--tone-secondary");
  }
});

test("an alias whose version the engine does not know shows no label", () => {
  openEdit("opus");
  expect(screen.getByRole("radio", { name: /^HaikuThe/ })).toBeTruthy();
});

test.each(["cloud", "unchanged"] as const)(
  "a %s sign-in, which may resolve an alias elsewhere, shows no version",
  (method) => {
    openEdit("opus", method);
    expect(screen.getByRole("radio", { name: /^OpusThe/ })).toBeTruthy();
    expect(screen.queryByText("5.5")).toBeNull();
  },
);

test("a full model id opens on Another model, named as the catalogue names it and said to be a fixed version", () => {
  openEdit("claude-haiku-4-5");
  expect(checked(/^Another model/)).toBe(true);
  expect(checked(/^Haiku/)).toBe(false);
  expect((screen.getByRole("combobox", { name: /Model id/ }) as HTMLInputElement).value).toBe(
    "claude-haiku-4-5",
  );
  expect(screen.getByText("Claude Haiku 4.5, a fixed version.")).toBeTruthy();
  expect(screen.queryByText("Any model id the harness takes.")).toBeNull();
});

test("an id the catalogue does not list is described as any id the harness takes", () => {
  openEdit("claude-experimental-x");
  expect(checked(/^Another model/)).toBe(true);
  expect(screen.getByText("Any model id the harness takes.")).toBeTruthy();
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

// THE EFFORT CONTROL OFFERS ONLY WHAT THE HARNESS TAKES ON THE MODEL: every Claude
// Code level on an alias, none on a model it runs without effort, and a harness
// with no setting says why instead of offering one.
test("the effort control offers the levels the harness takes on the model", () => {
  openEdit("opus");
  fireEvent.click(screen.getByRole("combobox", { name: /Reasoning effort/ }));
  for (const level of ["Harness default", "low", "medium", "high", "xhigh", "max"])
    expect(screen.getByRole("option", { name: level })).toBeTruthy();
  fireEvent.mouseDown(screen.getByRole("option", { name: "xhigh" }));
  expect(latest.effort).toBe("xhigh");
  cleanup();
  openEdit("claude-haiku-4-5");
  expect(
    screen.getByText("Claude Code runs claude-haiku-4-5 with no reasoning effort setting."),
  ).toBeTruthy();
});

test("a harness with no effort setting says why", () => {
  render(
    <ModelStep
      draft={{ ...emptyDraft(), harness: "cursor-agent", model: "gpt-5.5" }}
      error={undefined}
      effortError={undefined}
      onChange={() => {}}
      onEffort={() => {}}
    />,
  );
  expect(screen.queryByRole("combobox", { name: /Reasoning effort/ })).toBeNull();
  expect(
    screen.getByText(
      /No reasoning effort setting here. Cursor Agent takes effort as part of the model id/,
    ),
  ).toBeTruthy();
});
