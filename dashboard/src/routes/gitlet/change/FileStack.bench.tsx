/**
 * What a change's stack costs to draw: the real `FileStack` over five Go and TSX files, its
 * walk and blobs answered at once, plain and coloured. jsdom lays nothing out: render work.
 */
import { cleanup } from "@testing-library/react";
import { expect, test } from "vitest";
import { WINDOW_FROM_ROWS } from "@crewlethq/ui";
import type { CodeChangeDetail, CodeDiffFile } from "~/protocol/index.ts";
import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { SAMPLES } from "~/test/syntax.ts";
import { FOLD_FROM_LINES, FileStack, type StackNotes } from "./FileStack.tsx";

const RUNS = 20;
const WARMUP = 3;
const bounded = { time: 0, iterations: RUNS, warmupTime: 0, warmupIterations: WARMUP };

const CHANGE = {
  id: "change-1",
  repo: "web",
  repo_id: "repo-web",
  number: 42,
  ref: "web!42",
  revision: 2,
  version: 5,
  head: "4f1c2a90123456789abcdef0123456789abcdef0",
  state: "open",
} as unknown as CodeChangeDetail;

const NOTES: StackNotes = { findings: [], placed: {}, comments: [], drafts: null, thread: "" };

/** A file of `rows` lines of its language's sample, every fourth one edited, in one hunk. */
function fileOf(path: string, rows: number, at: number) {
  const sample = (/\.(go|txt)$/.test(path) ? SAMPLES.go : SAMPLES.ts).split("\n");
  const text = Array.from({ length: rows }, (_, i) => sample[i % sample.length]!);
  const patch = [`@@ -1,${rows} +1,${rows} @@`];
  let edits = 0;
  text.forEach((line, i) => {
    if (i % 4 !== 3) return void patch.push(` ${line}`);
    patch.push(`-${line}`, `+${line.replace(/\w+/, "renamed")}`);
    edits++;
  });
  const entry = {
    path,
    status: "M",
    added: edits,
    deleted: edits,
    old_oid: `o${at}`,
    new_oid: `n${at}`,
  } as CodeDiffFile;
  return { entry, patch: `${patch.join("\n")}\n`, text: `${text.join("\n")}\n`, rows };
}

/** The stack's five files, the last `last` rows long; plain ones in paths no grammar reads. */
const stackOf = (last: number, plain = false) =>
  (
    [
      ["internal/render/page.go", 130],
      ["internal/render/stream.go", 120],
      ["internal/server/routes.go", 40],
      ["web/src/product/Page.tsx", 60],
      ["web/src/product/Shell.tsx", last],
    ] as const
  ).map(([path, rows], i) => fileOf(plain ? path.replace(/\.\w+$/, ".txt") : path, rows, i));

/** The engine: the walk in one page, and each blob whole. */
function answer(files: ReturnType<typeof stackOf>) {
  const blobs = new Map<string, (typeof files)[number]>(
    files.flatMap((f, i) => [[`o${i}`, f] as const, [`n${i}`, f] as const]),
  );
  stubCodeEngine({
    "/query/code_change": () =>
      jsonAnswer({
        position: CODE_POSITION,
        level: "session",
        revision: 2,
        diff: {
          base: CHANGE.head,
          head: CHANGE.head,
          files: [],
          total: files.length,
          added: 0,
          deleted: 0,
          patches: files.map((f, file) => ({
            file,
            path: f.entry.path,
            status: "M",
            patch: f.patch,
          })),
        },
      }),
    "/query/code_blob": (url) => {
      const ref = url.searchParams.get("ref") ?? "";
      const f = blobs.get(ref)!;
      return jsonAnswer({
        position: CODE_POSITION,
        level: "session",
        blob: {
          oid: ref,
          size: f.text.length,
          lines: f.rows,
          start_line: 1,
          end_line: f.rows,
          text: f.text,
        },
      });
    },
  });
}

/** Mount the stack and wait, a task at a time, until its five sections draw their diffs. */
async function draw(files: ReturnType<typeof stackOf>, coloured: boolean) {
  answer(files);
  const entries = files.map((f) => f.entry);
  mountCode(
    "#/gitlet/repos/web/changes/42?tab=files",
    <FileStack
      change={CHANGE}
      head={CHANGE.head}
      rev={2}
      since={null}
      moved="5"
      files={entries}
      indexOf={(f) => entries.indexOf(f)}
      layout="unified"
      notes={NOTES}
      jump={null}
      onCurrent={() => {}}
    />,
  );
  const done = () =>
    document.querySelectorAll(".code-file .code-file__diff").length === 5 &&
    (!coloured || document.querySelectorAll(".code-file [class*='code-syn-']").length > 0);
  for (let pass = 0; !done(); pass++) {
    if (pass > 500) throw new Error("the stack never drew its five files");
    await afterMicrotasks();
  }
}

test("the real stack of five files, plain and coloured, and with one file at the threshold", async ({
  bench,
}) => {
  const small = stackOf(64);
  const plain = stackOf(64, true);
  // 800 LINES, A QUARTER EDITED: the window's 1,000 rows, and the fold's 400 changed lines.
  const large = stackOf(800);
  expect(800 * 1.25).toBe(WINDOW_FROM_ROWS);
  expect(800 / 2).toBe(FOLD_FROM_LINES);
  await draw(small, true);
  expect(document.querySelectorAll(".code-file .code-file__diff")).toHaveLength(5);
  cleanup();

  await bench("plain", async () => {
    await draw(plain, false);
    cleanup();
  }).run(bounded);
  await bench("coloured", async () => {
    await draw(small, true);
    cleanup();
  }).run(bounded);
  await bench("coloured, one file at the threshold", async () => {
    await draw(large, true);
    cleanup();
  }).run(bounded);
});
