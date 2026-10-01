/**
 * One row of the event log, and the row a card draws.
 *
 * Live's Activity card cut every sentence to "Organization…" while it spent
 * two columns on the actor — `harness-0` as who and again as where — and a
 * third on a category chip. The actor is said once now, the tail names a
 * source only where it differs, and a card's row gives the sentence every
 * pixel the time leaves.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { EventRow } from "./common.tsx";
import type { FeedRow } from "~/protocol/index.ts";

afterEach(cleanup);

const row = (over: Partial<FeedRow> = {}): FeedRow =>
  ({
    id: "e-1",
    type: "org.started",
    timestamp: "2026-09-28T10:00:00Z",
    source: "harness-0",
    actor: "harness-0",
    summary: "Organization 'Nimbus' started",
    category: "lifecycle",
    ...over,
  }) as FeedRow;

// Mutation: print `event.source` unconditionally, and `harness-0` is drawn twice.
test("a source that is the actor is not drawn again in the tail", () => {
  const { container } = render(<EventRow event={row()} />);
  expect(container.textContent?.match(/harness-0/g)).toHaveLength(1);
  const other = render(<EventRow event={row({ id: "e-2", source: "webhook" })} />);
  expect(other.container.querySelector(".feed-tail")?.textContent).toContain("webhook");
});

test("a card's row is the time and one line: the actor, then what happened", () => {
  const { container } = render(<EventRow event={row()} compact />);
  const link = container.querySelector("a.feed-row.compact")!;
  expect(link.getAttribute("href")).toBe("#/live/events/e-1");
  // TWO TRACKS: the clock, and the sentence with its actor inside it.
  expect([...link.children].map((c) => c.className)).toEqual(["feed-time", "feed-what truncate"]);
  expect(link.querySelector(".feed-what")?.textContent).toBe(
    "harness-0 Organization 'Nimbus' started",
  );
  expect(link.querySelector(".feed-tail")).toBeNull();
  // THE HOVER TEXT IS THE LINE DRAWN, character for character — the full
  // sentence of a line the width cut, not a differently punctuated one.
  const what = link.querySelector(".feed-what")!;
  expect(what.getAttribute("title")).toBe(what.textContent);
});

// A SENTENCE THAT OPENS WITH ITS ACTOR IS NOT PREFIXED WITH IT AGAIN: the
// engine writes "Agent PM finished reflecting", and the card read "Agent PM
// Agent PM finished reflecting".
test("a card's row does not repeat an actor the sentence already opens with", () => {
  const { container } = render(
    <EventRow
      event={row({
        actor: "Agent PM",
        source: "Agent PM",
        summary: "Agent PM finished reflecting",
      })}
      compact
    />,
  );
  const what = container.querySelector(".feed-what")!;
  expect(what.textContent).toBe("Agent PM finished reflecting");
  expect(what.querySelector(".feed-actor")?.textContent).toBe("Agent PM");
  expect(what.getAttribute("title")).toBe(what.textContent);
});
