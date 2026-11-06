/**
 * The row a card draws for one event (Live › Now, Home): the time and one line,
 * the actor then what happened, with the sentence given every pixel the time
 * leaves. The event log itself is a table (EventGrid.test.tsx).
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

test("a card's row is the time and one line: the actor, then what happened", () => {
  const { container } = render(<EventRow event={row()} />);
  const link = container.querySelector("a.feed-row")!;
  expect(link.getAttribute("href")).toBe("#/live/events/e-1");
  // TWO TRACKS: the clock, and the sentence with its actor inside it.
  expect([...link.children].map((c) => c.className)).toEqual(["feed-time", "feed-what truncate"]);
  expect(link.querySelector(".feed-what")?.textContent).toBe(
    "harness-0 Organization 'Nimbus' started",
  );
  // THE HOVER TEXT IS THE LINE DRAWN, character for character.
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
    />,
  );
  const what = container.querySelector(".feed-what")!;
  expect(what.textContent).toBe("Agent PM finished reflecting");
  expect(what.querySelector(".feed-actor")?.textContent).toBe("Agent PM");
  expect(what.getAttribute("title")).toBe(what.textContent);
});
