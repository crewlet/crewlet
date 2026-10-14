/**
 * The palette's fixed destinations say, before a press, which of them need a
 * token.
 *
 * A destination is DERIVED from `nav.ts`'s table, so the hint follows the
 * table's `guarded` — and a workspace that drew its own refusal without
 * marking itself guarded offered its pages here with no word that they would
 * refuse the reader who pressed them.
 */

// @vitest-environment node

import { describe, expect, test, vi } from "vitest";

import { DESTINATIONS } from "../nav.ts";
import type { Navigator } from "../router.tsx";
import { destinationHits } from "./hits.ts";

const NAV = { to: vi.fn() } as unknown as Navigator;

/** Every destination the palette offers for an empty phrase, keyed by its path. */
function offered(): Map<string, string> {
  const hits = destinationHits("", NAV, DESTINATIONS.length);
  expect(hits.length).toBe(DESTINATIONS.length);
  return new Map(
    hits.map((hit, i) => [
      DESTINATIONS.find((d) => `nav-${d.key}` === hit.id)?.path.join("/") ?? `?${i}`,
      hit.hint,
    ]),
  );
}

describe("a destination's hint", () => {
  test("says a guarded destination needs a token, and an open one does not", () => {
    const hints = offered();
    for (const d of DESTINATIONS) {
      expect(hints.get(d.path.join("/"))?.endsWith("· needs a token"), d.key).toBe(
        d.guarded === true,
      );
    }
  });

  // THE THREE CHAT DESTINATIONS, by address: every chat read is a person's,
  // so each of them refuses a reader with no token.
  test.each(["chat", "chat/mentions", "chat/search"])("#/%s needs a token", (path) => {
    expect(offered().get(path)).toMatch(/· needs a token$/);
  });
});
