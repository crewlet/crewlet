/**
 * THE PEOPLE LIST BESIDE THE PERSON PEEK, LAID OUT: narrowed to the column the
 * peek leaves it, it keeps what a reader compares people by (who, their login,
 * their stage and their access) and gives way where the peek already says it.
 *
 * It gave way in the order Access, Kind, Login and kept the seat's handle, so
 * beside the peek the Access page's own list hid access. Mutation: give Access
 * a `drop` again, or take the login off the name's second line, and a case
 * fails.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { DataGrid, LayerHost } from "@crewlethq/ui";
import "~/test/layout.tsx";
import { Router } from "~/app/router.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { peopleColumns, type DirectoryRow } from "./Access.tsx";

const ROWS: DirectoryRow[] = [
  {
    id: "p-1",
    kind: "person",
    stage: "active",
    name: "Maximiliana Konstantinopoulou-Vanderberg",
    login: "maximiliana.konstantinopoulou",
    seat: "max-k",
    grants: ["worklet", "doclet", "artifacts", "chatlet", "gitlet"],
  },
  {
    id: "p-2",
    kind: "person",
    stage: "suspended",
    name: "Priya Natarajan",
    login: "priya.natarajan",
    seat: "priya-eng",
    grants: ["worklet", "chatlet"],
  },
  {
    id: "p-3",
    kind: "seat",
    stage: "active",
    login: "seat:agent-cto",
    seat: "agent-cto",
    grants: [],
  },
];

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

/** The grid at `width`, as the list column draws it, settled. */
async function mount(width: number) {
  await page.viewport(1440, 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Router>
        <LayerHost>
          <DataGrid<DirectoryRow>
            rows={ROWS}
            rowKey={(p) => p.id}
            columns={peopleColumns(indexOrg(null))}
          />
        </LayerHost>
      </Router>,
    ),
  );
  await document.fonts.ready;
  // THE FIT RUNS AFTER EACH COMMIT, one column at a time: let it settle.
  for (let i = 0; i < 10; i++) await new Promise((r) => requestAnimationFrame(r));
}

const heads = () =>
  [...host!.querySelectorAll<HTMLElement>('[role="columnheader"]')].map((h) =>
    (h.textContent ?? "").trim(),
  );

// 490px is the list's column beside the person peek in a 1440px window.
test("beside the peek the list keeps the name and login, the stage and the access", async () => {
  await mount(490);
  expect(heads()).toEqual(["Name", "Stage", "Access"]);
  expect(host!.textContent).toContain("Hidden to fit: Kind and Seat");
  expect(host!.textContent).toContain("maximiliana.konstantinopoulou");
  expect(host!.textContent).toContain("Worklet");
});

test("with the room of a whole window every column is drawn", async () => {
  await mount(1100);
  expect(heads()).toEqual(["Name", "Kind", "Stage", "Seat", "Access"]);
  expect(host!.textContent).not.toContain("Hidden to fit");
});
