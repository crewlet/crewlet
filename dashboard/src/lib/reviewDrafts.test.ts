/**
 * A reviewer's drafted findings are the tab's, per change and per revision,
 * until a review sends them.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import {
  draftedRevisions,
  findingOf,
  forgetDrafts,
  readDrafts,
  useReviewDrafts,
} from "./reviewDrafts.ts";

afterEach(() => sessionStorage.clear());

const DRAFT = {
  path: "app.go",
  side: "new" as const,
  line_start: 3,
  line_end: 4,
  severity: "major",
  category: "tests",
  title: "Test the retry",
  body: "",
};

describe("a revision's drafts", () => {
  // PER REVISION: a finding names the lines of the revision it was read in,
  // so a draft on revision 1 is no finding on revision 2. Mutation: key the
  // drafts by the change alone, and revision 2 holds revision 1's.
  test("are kept per change and revision, in the tab's session", () => {
    const one = renderHook(() => useReviewDrafts("c1", 1));
    act(() => one.result.current.add(DRAFT));
    expect(readDrafts("c1", 1).map((d) => d.title)).toEqual(["Test the retry"]);
    expect(readDrafts("c1", 2)).toEqual([]);
    expect(draftedRevisions("c1")).toEqual([{ revision: 1, count: 1 }]);
  });

  // HEARD WHEREVER THE TAB WRITES THEM: the review that sends them is not the
  // diff that drew them, which must stop drawing them once they are sent.
  // Mutation: keep the drafts in the hook's state alone, and the diff still
  // draws a sent draft.
  test("are forgotten everywhere the tab draws them once a review has sent them", () => {
    const diff = renderHook(() => useReviewDrafts("c1", 2));
    act(() => diff.result.current.add(DRAFT));
    expect(diff.result.current.drafts).toHaveLength(1);
    act(() => forgetDrafts("c1", 2));
    expect(diff.result.current.drafts).toEqual([]);
    expect(sessionStorage.length).toBe(0);
  });

  // SENT AS THE TOOL TAKES A FINDING: nothing of the tab's own, and a
  // suggestion only on the newer side, which is the side it replaces lines on.
  test("are sent as the tool takes a finding, and nothing of the tab's", () => {
    const sent = findingOf({ ...DRAFT, id: "local", suggestion: "x := 1" });
    expect(sent).toEqual({
      path: "app.go",
      side: "new",
      line_start: 3,
      line_end: 4,
      severity: "major",
      category: "tests",
      title: "Test the retry",
      suggestion: "x := 1",
    });
    expect(findingOf({ ...DRAFT, id: "l", side: "old", suggestion: "y" })).not.toHaveProperty(
      "suggestion",
    );
  });
});
