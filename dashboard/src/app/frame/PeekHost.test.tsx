/**
 * What a list hands the one peek: the order `[` and `]` walk, and the question
 * a task opened through the peek's Open carries.
 *
 * The failure it prevents: a task opened from a board through its peek landed
 * on its own page with no `list=`, so the page had nowhere to say "3 of 18"
 * and no neighbour to step to — the one way into a task a board offers on a
 * plain click was the one way that lost its place.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Suspense } from "react";
import { afterEach, expect, test } from "vitest";

import { PeekHost, PeekNeighbours, usePeekNeighbours } from "./PeekHost.tsx";
import { Router } from "~/app/router.tsx";
import type { ObjectRef } from "./objects.ts";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

const LIST = "container=project:ENG&group_by=status";

function List({ refs, query }: { refs: ObjectRef[]; query?: Record<string, string> }) {
  usePeekNeighbours(refs, query);
  return null;
}

async function mount(peek: string, query?: Record<string, string>) {
  location.hash = `#/worklet/ENG?peek=${peek}`;
  // IN AN ASYNC ACT: the item's peek body is a lazy chunk, and a render that
  // suspends on one leaves the list's publishing effect queued until the act
  // that started it settles.
  await act(async () => {
    render(
      <Router>
        <PeekNeighbours>
          <List
            refs={[
              { kind: "item", id: "ENG-1" },
              { kind: "item", id: "ENG-2" },
            ]}
            query={query}
          />
          <Suspense fallback={null}>
            <PeekHost />
          </Suspense>
        </PeekNeighbours>
      </Router>,
    );
  });
}

// FOCUS STEPS WITH THE PEEK: a row left focused behind a stepped peek read as a
// second current row, and Enter on it opened the old object again. The rail's
// keys are the frame's, so `]` is pressed where the reader presses it.
function Rows() {
  return (
    <main>
      <a href="#/worklet/ENG-1?list=x">first</a>
      <a href="#/worklet/ENG-2?list=x">second</a>
      <button>elsewhere</button>
    </main>
  );
}

async function mountRows() {
  location.hash = "#/worklet/ENG?peek=item:ENG-1";
  await act(async () => {
    render(
      <Router>
        <PeekNeighbours>
          <List
            refs={[
              { kind: "item", id: "ENG-1" },
              { kind: "item", id: "ENG-2" },
            ]}
          />
          <Rows />
          <Suspense fallback={null}>
            <PeekHost />
          </Suspense>
        </PeekNeighbours>
      </Router>,
    );
  });
}

const stepNext = async () => {
  await act(async () => {
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "]" });
  });
  await waitFor(() => expect(location.hash).toContain("peek=item%3AENG-2"));
};

test("focus on the row the peek was on moves with the step", async () => {
  await mountRows();
  screen.getByRole("link", { name: "first" }).focus();
  await stepNext();
  expect(document.activeElement).toBe(screen.getByRole("link", { name: "second" }));
});

test("focus anywhere else stays where the reader put it", async () => {
  await mountRows();
  const elsewhere = screen.getByRole("button", { name: "elsewhere" });
  elsewhere.focus();
  await stepNext();
  expect(document.activeElement).toBe(elsewhere);
});

test("a task opened through the peek carries the list it was opened from", async () => {
  await mount("item:ENG-2", { list: LIST });
  // THE LIST PUBLISHES IN AN EFFECT, so the rail's link is the second render's.
  await waitFor(() =>
    expect(screen.getByRole("link", { name: /^Open/ }).getAttribute("href")).toBe(
      `#/worklet/ENG-2?list=${encodeURIComponent(LIST)}`,
    ),
  );
});

// ONLY FOR AN OBJECT IN THAT LIST: a peek on something else has no place in it.
test("a peek on something the list does not hold opens its plain page", async () => {
  await mount("item:ENG-9", { list: LIST });
  expect(screen.getByRole("link", { name: /^Open/ }).getAttribute("href")).toBe("#/worklet/ENG-9");
});
