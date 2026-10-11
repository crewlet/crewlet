// Fails when a Mermaid diagram in this repository's markdown does not parse.
//
//   node check.mjs [root]
//
// root defaults to the repository this file sits in, found from the file's
// own location rather than the working directory, so `npm run check` judges
// the same tree from wherever it is started. diagrams.mjs says what is read
// and why.

import { fileURLToPath } from "node:url";
import { check, loadParser } from "./diagrams.mjs";

const root = process.argv[2] ?? fileURLToPath(new URL("../..", import.meta.url));
const { files, blocks, failures } = await check(root, await loadParser());

for (const { file, line, message } of failures) {
  console.error(`${file}:${line}: this mermaid diagram does not parse`);
  console.error(message.replace(/^/gm, "    "));
}

// A run that found no diagram at all passed nothing: the tree holds dozens, so
// none means the walk read the wrong directory or none of it, and a gate that
// asserts an absence must count what it read before its silence means
// anything.
if (blocks === 0) {
  console.error(`no mermaid diagram found in ${files} markdown file(s) under ${root}`);
  process.exitCode = 1;
} else if (failures.length > 0) {
  console.error(
    `${failures.length} of ${blocks} mermaid diagram(s) in ${files} markdown file(s) do not ` +
      "parse: GitHub draws a parse error in place of each, and the documentation site, " +
      "which renders with Mermaid 11, cannot draw them.",
  );
  process.exitCode = 1;
} else {
  console.log(`${blocks} mermaid diagram(s) in ${files} markdown file(s) parse.`);
}
