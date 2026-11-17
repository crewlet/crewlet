/**
 * Gitlet's shared marks: Gitlet drawn as its product mark wherever it is the
 * writer, a check's verdict as a glyph and a word with no cross for an
 * absence, a tag opening its whole ref, and a change's pill opening it.
 */

import { cleanup, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { CircleXGlyph } from "@crewlethq/icons/glyphs";
import { afterEach, expect, test } from "vitest";

import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type CodeCheckRollup } from "~/protocol/index.ts";
import { CODE_ORG } from "~/test/codeScreens.tsx";
import { ChangeRefPill } from "./ChangeRow.tsx";
import { gitletWrote } from "./repo/History.tsx";
import { CheckRollupMark, Face, GITLET, TagChip, useWhoName } from "./ui.tsx";

afterEach(cleanup);

function mount(node: ReactNode) {
  const store = new Store();
  store.applyOrg(CODE_ORG);
  const socket = new LiveSocket(store);
  return render(<ClientContext.Provider value={{ store, socket }}>{node}</ClientContext.Provider>);
}

function Named({ who }: { who: Parameters<ReturnType<typeof useWhoName>>[0] }) {
  return <span data-testid="name">{useWhoName()(who)}</span>;
}

// GITLET IS ITS PRODUCT MARK, never a seat's badge, and named "Gitlet". Mutation: draw
// Gitlet through SeatAvatar.
test("Gitlet's face is its product mark and its name is Gitlet", () => {
  const { container } = mount(
    <>
      <Face who={GITLET} />
      <Named who={GITLET} />
    </>,
  );
  expect(container.querySelector(".code-face svg")).toBeTruthy();
  expect(container.querySelector(".crewlet-avatar")).toBeNull();
  expect(screen.getByTestId("name").textContent).toBe("Gitlet");
});

const rollup = (state: string, extra: Partial<CodeCheckRollup> = {}): CodeCheckRollup => ({
  state,
  counts: {},
  required: 2,
  required_passed: 2,
  ...extra,
});

// A CROSS ONLY FOR A FAILURE: cancelled and pending wear none, running wears the kit's ring.
// Mutation: draw CircleXGlyph for cancelled.
test("a check's verdict draws a cross only for a failure, and the ring while running", () => {
  const { container } = mount(
    <>
      <CheckRollupMark rollup={rollup("passed")} title="t" of />
      <CheckRollupMark rollup={rollup("cancelled")} title="t" />
      <CheckRollupMark rollup={rollup("pending")} title="t" />
      <CheckRollupMark rollup={rollup("running")} title="t" />
      <CheckRollupMark
        rollup={rollup("failing", { failing: [{ name: "e2e", state: "failed" }] })}
        title="3 checks"
      />
    </>,
  );
  const marks = [...container.querySelectorAll<HTMLElement>(".code-check-mark")];
  expect(marks.map((m) => m.textContent)).toEqual([
    "Passed 2 of 2",
    "Cancelled",
    "Pending",
    "Running",
    "e2e failed",
  ]);
  expect(marks.map((m) => m.dataset.tone)).toEqual([
    "success",
    "neutral",
    "neutral",
    "info",
    "danger",
  ]);
  const cross = render(<CircleXGlyph size="sm" />).container.querySelector("svg")!.innerHTML;
  expect(marks.filter((m) => m.querySelector("svg")?.innerHTML === cross)).toEqual([marks[4]]);
  expect(marks[3]!.querySelector(".crewlet-spinner")).toBeTruthy();
  expect(marks[4]!.getAttribute("title")).toBe("3 checks");
});

// A BARE VERDICT is the glyph alone, its word what a screen reader says. Mutation: drop the word.
test("a bare verdict draws its glyph and says its word only to a screen reader", () => {
  const { container } = mount(
    <CheckRollupMark rollup={rollup("passed")} title="unit passed" bare />,
  );
  const mark = container.querySelector<HTMLElement>(".code-check-mark")!;
  expect(mark.querySelector("svg")).toBeTruthy();
  expect(mark.querySelector(".code-check-mark__word")).toBeNull();
  expect(mark.textContent).toBe("Passed");
  expect(mark.getAttribute("title")).toBe("unit passed");
});

// GITLET WROTE A MERGE its push names, or one its marks name as a landing's merge commit.
// Mutation: drop the marks' half.
test("Gitlet wrote a two-parent merge its push or its landing names, and nothing else", () => {
  const merge = {
    oid: "c".repeat(40),
    parent_count: 2,
    engine_committed: true,
    authored_at: "",
    committed_at: "",
  };
  const landed = {
    commit: merge.oid,
    merged: [{ repo: "web", ref: "web!41", title: "", state: "merged", merge_oid: merge.oid }],
  };
  expect(gitletWrote(merge, undefined, landed)).toBe(true);
  expect(gitletWrote(merge, undefined, { commit: merge.oid, merged: [] })).toBe(false);
  expect(gitletWrote({ ...merge, parent_count: 1 }, undefined, landed)).toBe(false);
  expect(gitletWrote({ ...merge, engine_committed: false }, undefined, landed)).toBe(false);
});

// A TAG OPENS ITS WHOLE REF, which no branch of its name can shadow; a change's pill opens
// the change. Mutation: link the tag's short name.
test("a tag chip opens its whole ref, and a change's pill opens the change", () => {
  mount(
    <>
      <TagChip repo="web" gitRef="refs/tags/v2026.11.2" />
      <ChangeRefPill change={{ repo: "web", ref: "web!41", title: "cache", state: "merged" }} />
    </>,
  );
  const tag = screen.getByRole("link", { name: /v2026\.11\.2/ });
  expect(tag.getAttribute("href")).toBe("#/gitlet/repos/web?ref=refs%2Ftags%2Fv2026.11.2");
  expect(tag.getAttribute("title")).toBe("Browse the files at v2026.11.2");
  const pill = screen.getByRole("link", { name: /web!41/ });
  expect(pill.getAttribute("href")).toBe("#/gitlet/repos/web/changes/41");
  expect(pill.getAttribute("title")).toBe("Change web!41, merged: cache");
  expect(pill.querySelector(".code-state[data-state='merged']")).toBeTruthy();
});
