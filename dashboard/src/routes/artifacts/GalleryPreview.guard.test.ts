// @vitest-environment node
/**
 * A PREVIEW NEVER CHANGES WHAT OTHERS SEE: a gallery card's live preview joins
 * no artifact room, publishes no presence, attaches no page host (which is
 * what answers a page's datastore writes and identity), opens no datastore
 * copy and files no ready report; and the preview host it attaches instead
 * holds no write path, no store, no room and no presence. Held on the source,
 * since each of those is an import a later change could add.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const read = (file: string) => readFileSync(fileURLToPath(new URL(file, import.meta.url)), "utf8");
const source = read("./GalleryPreview.tsx");
const previewHost = read("./previewHost.ts");

test.each([
  "lib/artifacts/room",
  "useArtifactRoom",
  "./presence",
  "./host",
  "PageHost",
  "useDataStore",
  "reportReady",
])("a preview never reaches %s", (forbidden) => {
  expect(source).not.toContain(forbidden);
});

test("every content read a preview makes is asked as a preview", () => {
  const calls = [...source.matchAll(/\b(mintFrame|readBoard)\(([^;]*?)\)/gs)];
  expect(calls.map((c) => c[1]).sort()).toEqual(["mintFrame", "readBoard"]);
  for (const call of calls) expect(call[2], call[1]).toContain("preview: true");
  expect(source).not.toMatch(/\b(readDoc|readDeck|readSource|versionText|versionFile)\(/);
});

test.each([
  "keyedWrite",
  "writeState",
  "writeData",
  "useDataStore",
  "ArtifactDataStore",
  "lib/artifacts/room",
  "presence",
  "setPresence",
  "reportReady",
  "./host",
  "PageHost",
  "identity.me",
])("the preview host never reaches %s", (forbidden) => {
  expect(previewHost).not.toContain(forbidden);
});
