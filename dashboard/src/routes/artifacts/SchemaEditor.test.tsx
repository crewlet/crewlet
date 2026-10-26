/**
 * The schema editor reads as a form a person can scan: collections as a list
 * with the one on show marked, colours as swatches and tones in words, flags
 * at the step of the controls beside them, and Save in reach with how much it
 * will apply.
 */

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ToastProvider } from "@crewlethq/ui";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { reviewSheetSchema } from "~/lib/artifacts/schema.ts";
import { SchemaEditor } from "./SchemaEditor.tsx";
import { context, draftField, openFrame, setDraft } from "./draftTesting.ts";

afterEach(cleanup);

const detail = { id: "a-1", title: "Q4 launch review" } as ArtifactDetail;

function mount() {
  const collections = reviewSheetSchema();
  render(
    <ToastProvider>
      <SchemaEditor detail={detail} collections={collections} role="owner" onSaved={() => {}} />
    </ToastProvider>,
  );
  return collections;
}

test("the collections are a list with the one on show marked", () => {
  const collections = mount();
  const list = screen.getByRole("list", { name: "Collections" });
  const first = collections[0]!;
  const row = within(list).getByRole("button", { name: new RegExp(first.title || first.name) });
  expect(row.getAttribute("aria-current")).toBe("true");
});

test("a choice's colour is a swatch row and a status's tone is said in words", () => {
  const collections = mount();
  const columns = collections[0]!.columns ?? [];
  const choice = columns.find((c) => c.type === "select" || c.type === "multiselect");
  const status = columns.find((c) => c.type === "status");
  if (choice) {
    const option = choice.options![0]!;
    expect(screen.getByRole("radiogroup", { name: `Colour of ${option.label}` })).toBeTruthy();
  }
  if (status) {
    const option = status.options![0]!;
    expect(screen.queryByRole("radiogroup", { name: `Colour of ${option.label}` })).toBeNull();
    expect(screen.getAllByLabelText(`Tone of ${option.label}`).length).toBeGreaterThan(0);
  }
  expect(choice ?? status).toBeTruthy();
  // No raw token word is offered as a colour's name.
  expect(screen.queryByText("purple")).toBeNull();
});

test("a column's flags sit at the compact step and a new column asks for a name", () => {
  mount();
  for (const box of screen.getAllByRole("checkbox", { name: "Required" })) {
    expect(box.closest("label")?.classList.contains("crewlet-checkbox--sm")).toBe(true);
  }
  expect(screen.getByPlaceholderText("Column name")).toBeTruthy();
  expect(screen.queryByPlaceholderText("Owner")).toBeNull();
});

test("the foot counts the unsaved changes", () => {
  const collections = mount();
  expect(screen.queryByText(/unsaved change/)).toBeNull();
  const column = collections[0]!.columns![0]!;
  fireEvent.change(screen.getByLabelText(`Name of column ${column.key}`), {
    target: { value: "Renamed" },
  });
  expect(screen.getByText("1 unsaved change")).toBeTruthy();
});

const screens = readFileSync(
  join(dirname(fileURLToPath(import.meta.url)), "../../styles/screens.css"),
  "utf8",
);

function block(selector: string): string {
  const at = screens.indexOf(`\n${selector} {`);
  expect(at, `${selector} is declared`).toBeGreaterThan(-1);
  return screens.slice(at, screens.indexOf("}", at));
}

// EVERY ROW ON THE SAME TRACKS. Wrapping flex rows put the type select wherever
// the key before it ended, so no two rows lined up; and Save sticks to the
// view's foot rather than waiting at the end of a long form.
test("column rows share bounded tracks and the foot sticks to the view", () => {
  const head = block(".artifact-column-head");
  expect(head).toMatch(/display:\s*grid/);
  expect(head).toMatch(/grid-template-columns:/);
  expect(block(".artifact-schema-foot")).toMatch(/position:\s*sticky/);
});

test("a store with no collections says what one is and how to add it", () => {
  render(
    <ToastProvider>
      <SchemaEditor detail={detail} collections={[]} role="owner" onSaved={() => {}} />
    </ToastProvider>,
  );
  expect(screen.getByText("No collections yet")).toBeTruthy();
  expect(screen.getByText("Lowercase letters, digits and underscores.")).toBeTruthy();
});

// THE SCHEMA BEING CHANGED IS THE ASSISTANT'S TO READ, and a change it proposes
// lands in the editor's own draft, counted with the reader's; Save stays theirs.
test("the open schema is the assistant's to read, and a draft.set is one more unsaved change", () => {
  openFrame("artifact");
  const collections = mount();
  expect(context().draft).toMatchObject({ kind: "client", id: "a-1", step: "collections.1" });
  expect(draftField("collections.1.columns")).toMatchObject({ kind: "list" });
  expect(draftField("collections.1.columns")).not.toHaveProperty("editable");
  expect(setDraft({ "collections.1.title": "Launch reviews" })).toMatchObject({
    outcome: "applied",
  });
  expect(screen.getByText("1 unsaved change")).toBeTruthy();
  expect(draftField("collections.1.title")?.value).toBe("Launch reviews");
  expect(setDraft({ "collections.1.preset": "everyone" })).toMatchObject({ outcome: "invalid" });
  expect(collections[0]!.title).not.toBe("Launch reviews");
});

// A READER WHO MAY NOT CHANGE THE SCHEMA HOLDS NO DRAFT to change.
test("a schema read by a viewer declares no draft", () => {
  openFrame("artifact");
  render(
    <ToastProvider>
      <SchemaEditor
        detail={detail}
        collections={reviewSheetSchema()}
        role="viewer"
        onSaved={() => {}}
      />
    </ToastProvider>,
  );
  expect(context().draft).toBeUndefined();
});
