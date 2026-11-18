import { renderHook, waitFor } from "@testing-library/react";
import { expect, test } from "vitest";

import type { DiffHunk } from "~/components/DiffView.tsx";
import { useHunkTokens } from "./syntax.tsx";

const hunks: DiffHunk[] = [
  {
    skipped: 0,
    lines: [
      { kind: "same", text: "\treturn nil", before: 18, after: 18 },
      { kind: "add", text: '\tmux.Handle("GET /p", nil)', before: null, after: 19 },
    ],
  },
];

// AN EXCERPT IS COLOURED WITH NO BLOB READ, each side walked from its first line.
// Mutation: colour only the before side, and the added line is plain.
test("a few lines' tokens colour a Go keyword and string with no blob read", async () => {
  const { result } = renderHook(() => useHunkTokens("internal/server/routes.go", hunks));
  await waitFor(() => expect(result.current).toBeDefined());
  const spans = (side: "before" | "after", n: number) =>
    (result.current!(side, n) ?? []).map((s) => s.className);
  expect(spans("before", 18)).toContain("code-syn-keyword");
  expect(spans("after", 19)).toContain("code-syn-string");
  expect(result.current!("after", 99)).toBeNull();
});
