/**
 * Attaching a record to a message: what the picker lists for what the reader
 * typed.
 *
 * THE INVARIANTS: a result says which record it is in full. A title is cut at
 * a line, with the key that leads it whole and the rest in the row's tooltip,
 * never at the dialog's edge mid-word; and the example key the task finder
 * offers is one of this company's own.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { ProjectKeysProvider } from "~/lib/projectKeys.ts";
import { ADA, NATIVE_SERVICES, mountChat } from "~/test/chat.tsx";

import { AttachDialog } from "./Attach.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const TITLE =
  "Draft the programme for the spring planning offsite, with the agenda and the list of who attends";

/** The task picker, for a reader who holds Worklet, answering with `hits`. */
function mountPicker(answers: Record<string, unknown> = {}) {
  return mountChat(
    <ProjectKeysProvider>
      <AttachDialog kind="task" onPick={() => undefined} onClose={() => undefined} />
    </ProjectKeysProvider>,
    {
      viewer: { ...ADA, grants: ["chatlet", "worklet"] },
      services: NATIVE_SERVICES,
      answers: {
        work_projects: { projects: [] },
        work_search: {
          available: true,
          hits: [{ id: "t-40", key: "OPS-40", title: TITLE }],
        },
        ...answers,
      } as never,
    },
  );
}

// Mutation: draw the label in an inline `.truncate` span again, or drop the
// row's title, and this goes red.
test("a long task title is cut at a line, its key kept and its whole label in the tooltip", async () => {
  mountPicker();
  fireEvent.change(screen.getByRole("textbox", { name: "Find a task" }), {
    target: { value: "programme" },
  });
  const row = await screen.findByRole("button", { name: `OPS-40 ${TITLE}` });
  expect(row.closest("li")?.getAttribute("title")).toBe(`OPS-40 ${TITLE}`);
  const words = row.querySelector(".clamp");
  expect(words?.textContent).toBe(`OPS-40 ${TITLE}`);
  expect(row.querySelector(".truncate")).toBeNull();
});

// Mutation: write a fixed key into the helper again and this goes red.
test("the task finder's example key is the company's own first project's", async () => {
  mountPicker({ work_projects: { projects: [{ key: "OPS" }, { key: "ENG" }] } });
  expect(await screen.findByText("A key like OPS-1, or words from its title.")).toBeTruthy();
});

test("a company with no project is offered no example key", async () => {
  mountPicker();
  expect(await screen.findByText("A task key, or words from its title.")).toBeTruthy();
});
