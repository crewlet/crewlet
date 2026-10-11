// The gate's own suite. Its fixtures are written to a fresh temporary
// directory per case and removed after it, rather than committed: a broken
// diagram committed anywhere in this tree is exactly what the gate fails on,
// and a nested checkout cannot be committed at all (git never tracks a path
// with a `.git` component).

import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import { diagrams } from "./diagrams.mjs";

const CHECK = fileURLToPath(new URL("check.mjs", import.meta.url));

// The two forms that reached main and blanked a page each: a `;` in a
// sequence message ends the statement there, and a `.` in a dotted edge's
// label ends the edge.
const SEMICOLON_IN_A_MESSAGE = [
  "sequenceDiagram",
  "    participant D as daemon",
  "    D->>D: append (or replace on a reset); trim the front past 256 KiB",
].join("\n");
const DOT_IN_A_DOTTED_EDGE = ["flowchart LR", "    engine -. integrations.slack .-> slack"].join(
  "\n",
);

const fence = (source, opener = "```mermaid", closer = "```") =>
  [opener, source, closer].join("\n");

// A temporary tree holding files, keyed by their slash-separated path.
function tree(t, files) {
  const root = mkdtempSync(join(tmpdir(), "mermaid-check-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  for (const [rel, content] of Object.entries(files)) {
    const path = join(root, ...rel.split("/"));
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
  return root;
}

// The command as `make mermaid-check` and CI run it, against root.
function run(root) {
  return new Promise((resolve) => {
    execFile(process.execPath, [CHECK, root], (err, stdout, stderr) => {
      resolve({ status: err ? err.code : 0, stdout, stderr });
    });
  });
}

describe("which fenced blocks are diagrams", () => {
  it("names each diagram by the line of its opening fence", () => {
    const page = ["# Title", "", fence("graph TD\n  a --> b"), "", "text"].join("\n");
    assert.deepEqual(diagrams(page), [{ line: 3, source: "graph TD\n  a --> b\n" }]);
  });

  it("reads a diagram inside a list item, its indentation stripped", () => {
    const page = ["- step", "", "    ```mermaid", "    graph TD", "      a --> b", "    ```"].join(
      "\n",
    );
    assert.deepEqual(diagrams(page), [{ line: 3, source: "graph TD\n  a --> b\n" }]);
  });

  it("reads a diagram inside a block quote, its markers stripped", () => {
    const page = [
      "> A note.", // 1
      ">", // 2
      "> ```mermaid", // 3
      "> graph TD", // 4
      ">   a --> b", // 5
      "> ```", // 6
      "", // 7
      "> - in a list in a quote", // 8
      ">", // 9
      ">   ```mermaid", // 10
      ">   graph LR", // 11
      ">     c --> d", // 12
      ">   ```", // 13
    ].join("\n");
    assert.deepEqual(diagrams(page), [
      { line: 3, source: "graph TD\n  a --> b\n" },
      { line: 10, source: "graph LR\n  c --> d\n" },
    ]);
  });

  it("reads a diagram after an HTML block, as GitHub draws it", () => {
    const page = [
      "<details>", // 1
      "<summary>The flow</summary>", // 2
      "", // 3
      fence("graph TD\n  a --> b"), // 4-7
      "", // 8
      "</details>", // 9
    ].join("\n");
    assert.deepEqual(diagrams(page), [{ line: 4, source: "graph TD\n  a --> b\n" }]);
  });

  // Each of these holds a ```mermaid line that no renderer draws, and each is
  // followed by a diagram that is drawn, so a case also proves the reading
  // carries on past what it passed over.
  for (const [what, lines] of [
    [
      "a diagram shown as an example inside another fence",
      ["````markdown", fence("graph TD\n  broken -. a.b .-> x"), "````"],
    ],
    ["a ```mermaid line inside a tilde fence", ["~~~text", "```mermaid", "~~~"]],
    ["a code span opening a line of prose", ["```mermaid``` is a code span, not a fence."]],
    [
      "an indented code block",
      ["A paragraph.", "", "    ```mermaid", "    graph TD", "    broken -. a.b .-> x", "    ```"],
    ],
    ["an HTML comment", ["<!--", fence("graph TD\n  broken -. a.b .-> x"), "-->"]],
  ]) {
    it(`judges nothing in ${what}`, () => {
      const page = [...lines, "", fence("graph TD\n  a --> b")].join("\n");
      // The drawn diagram's fence is the last four lines of the page.
      const line = page.split("\n").length - 3;
      assert.deepEqual(diagrams(page), [{ line, source: "graph TD\n  a --> b\n" }]);
    });
  }

  it("runs a fence that is never closed to the end of the page", () => {
    const page = ["```mermaid", "graph TD", "  a --> b"].join("\n");
    assert.deepEqual(diagrams(page), [{ line: 1, source: "graph TD\n  a --> b" }]);
  });
});

describe("the command", { concurrency: true }, () => {
  it("passes a tree whose diagrams parse", async (t) => {
    const root = tree(t, {
      "docs/ok.md": [
        "# Fine",
        "",
        // A labelled flowchart is the case that needs the DOM: without one,
        // Mermaid's sanitiser refuses it, whatever the grammar says.
        fence('flowchart LR\n  engine["the <b>engine</b>"] -. slack .-> chat(["chat"])'),
        "",
        fence("sequenceDiagram\n  D->>D: append (or replace on a reset), then trim"),
      ].join("\n"),
    });
    const { status, stdout, stderr } = await run(root);
    assert.equal(stderr, "");
    assert.equal(status, 0);
    assert.match(stdout, /^2 mermaid diagram\(s\) in 1 markdown file\(s\) parse\.$/m);
  });

  it("fails on each diagram that does not parse, naming its file and line", async (t) => {
    const root = tree(t, {
      "docs/flowchart.md": ["# Flowchart", "", fence(DOT_IN_A_DOTTED_EDGE)].join("\n"),
      "docs/ok.md": fence("graph TD\n  a --> b"),
      // A quoted diagram is drawn like any other, so it is judged like one.
      "docs/quoted.md": [
        "# Quoted",
        "",
        "> **Note**",
        ">",
        fence(DOT_IN_A_DOTTED_EDGE)
          .split("\n")
          .map((l) => `> ${l}`)
          .join("\n"),
      ].join("\n"),
      "docs/sequence.md": ["# Sequence", "", "Prose.", "", fence(SEMICOLON_IN_A_MESSAGE)].join(
        "\n",
      ),
    });
    const { status, stderr } = await run(root);
    assert.equal(status, 1);
    const named = stderr.match(/^\S+:\d+: this mermaid diagram does not parse$/gm);
    assert.deepEqual(named, [
      "docs/flowchart.md:3: this mermaid diagram does not parse",
      "docs/quoted.md:5: this mermaid diagram does not parse",
      "docs/sequence.md:5: this mermaid diagram does not parse",
    ]);
    // The parser's own words, which say where inside the diagram it stopped.
    assert.match(stderr, /^ {4}Lexical error on line 2\. Unrecognized text\.$/m);
    assert.match(stderr, /^ {4}Parse error on line 3:$/m);
    assert.match(stderr, /^3 of 4 mermaid diagram\(s\) in 4 markdown file\(s\) do not parse/m);
  });

  it("reads none of a nested checkout, an install, or what the link gate skips", async (t) => {
    const broken = fence(SEMICOLON_IN_A_MESSAGE);
    const root = tree(t, {
      "docs/ok.md": fence("graph TD\n  a --> b"),
      // A linked worktree's gitfile, pointing nowhere: still a copy of the
      // repository, so still not this tree.
      ".claude/worktrees/copy/.git": "gitdir: /nowhere\n",
      ".claude/worktrees/copy/docs/ok.md": broken,
      // A clone dropped anywhere, recognised by its `.git` and not its name.
      "tmp/clone/.git/HEAD": "ref: refs/heads/main\n",
      "tmp/clone/README.md": broken,
      ".git/notes.md": broken,
      "node_modules/pkg/README.md": broken,
      "dashboard/node_modules/pkg/README.md": broken,
      "dist/README.md": broken,
      "static/dashboard/README.md": broken,
      "dashboard/src/test/markdown/hostile.md": broken,
    });
    const { status, stdout, stderr } = await run(root);
    assert.equal(stderr, "");
    assert.equal(status, 0);
    // Exactly the one page outside every skip was read.
    assert.match(stdout, /^1 mermaid diagram\(s\) in 1 markdown file\(s\) parse\.$/m);
  });

  it("reads a dot-directory that holds no checkout", async (t) => {
    const root = tree(t, {
      ".github/pull_request_template.md": ["# PR", "", fence(DOT_IN_A_DOTTED_EDGE)].join("\n"),
    });
    const { status, stderr } = await run(root);
    assert.equal(status, 1);
    assert.match(stderr, /^\.github\/pull_request_template\.md:3: this mermaid diagram/m);
  });

  it("fails a tree with no diagram rather than passing on nothing", async (t) => {
    const root = tree(t, { "docs/plain.md": "# No diagrams\n\n```go\nfunc main() {}\n```\n" });
    const { status, stderr } = await run(root);
    assert.equal(status, 1);
    assert.match(stderr, /^no mermaid diagram found in 1 markdown file\(s\) under /m);
  });
});
