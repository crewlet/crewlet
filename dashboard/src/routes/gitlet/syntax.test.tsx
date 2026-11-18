import { act, renderHook, waitFor } from "@testing-library/react";
import { expect, test, vi } from "vitest";

import type { DiffHunk } from "~/components/DiffView.tsx";
import { engineJson } from "~/test/engineAnswer.ts";
import { useExpandLines, useHunkTokens, type LinesRead } from "./syntax.tsx";

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

// EXPANDED LINES COME FROM THE COPY HELD, else one ranged read by the blob's
// id with no path. Mutation: always read a range, and the first case asks.
test("expanded lines are answered from the held blob with no request", async () => {
  const fetched = vi.fn();
  vi.stubGlobal("fetch", fetched);
  const { result } = renderHook(() => useExpandLines("web", "b1", ["one", "two", "three"]));
  expect(await result.current.read(1, 3)).toEqual({ failed: null, total: 3 });
  expect(result.current.line(2)).toBe("two");
  expect(result.current.total).toBe(3);
  expect(fetched).not.toHaveBeenCalled();
  vi.unstubAllGlobals();
});

test("expanded lines are read by the blob's id, one range, where no copy is held", async () => {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      asked.push(url);
      return engineJson(
        {
          position: { stream: "CREWLET_CODE_LOG", generation: 1, seq: 1 },
          level: "session",
          blob: { oid: "b1", size: 40, lines: 30, start_line: 3, end_line: 4, text: "c\nd\n" },
        },
        200,
      );
    }),
  );
  const { result } = renderHook(() => useExpandLines("web", "b1", null));
  let got: LinesRead = { failed: "unset" };
  await act(async () => {
    got = await result.current.read(3, 4);
  });
  expect(got).toEqual({ failed: null, total: 30 });
  expect(asked).toHaveLength(1);
  expect(asked[0]!.pathname).toBe("/query/code_blob");
  expect(Object.fromEntries(asked[0]!.searchParams)).toMatchObject({
    name: "web",
    ref: "b1",
    start_line: "3",
    end_line: "4",
  });
  expect(asked[0]!.searchParams.has("path")).toBe(false);
  expect([result.current.line(3), result.current.line(4), result.current.total]).toEqual([
    "c",
    "d",
    30,
  ]);
  vi.unstubAllGlobals();
});
