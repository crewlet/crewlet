// AgoTime: a relative time inside a sentence, in words, on the one clock. The suite runs in Europe/Berlin.
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";

import { AgoTime } from "./AgoTime.tsx";
import { fmtDateTime } from "~/lib/format.ts";

// 13:00 in Berlin on Monday, November 16, 2026.
const NOW = Date.parse("2026-11-16T12:00:00Z");

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("is a time element carrying the exact instant", () => {
  const at = "2026-11-16T09:00:00Z";
  const { container } = render(<AgoTime value={at} now={NOW} />);
  const time = container.querySelector("time");
  expect(time?.getAttribute("datetime")).toBe(at);
  expect(time?.getAttribute("title")).toBe(fmtDateTime(at));
  expect(time?.textContent).toBe("3 hours ago");
});

test("advances with the one clock", async () => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
  const { container } = render(<AgoTime value={new Date(NOW - 3 * 60_000).toISOString()} />);
  expect(container.textContent).toBe("3 minutes ago");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(container.textContent).toBe("4 minutes ago");
});

test("hands since and clock to ago", () => {
  const { container, rerender } = render(<AgoTime value="2026-11-16T11:59:30Z" now={NOW} since />);
  expect(container.textContent).toBe("a moment ago");
  rerender(<AgoTime value="2026-11-15T16:40:00Z" now={NOW} clock />);
  expect(container.textContent).toBe("yesterday at 17:40");
});

test("draws the empty value for an unset time", () => {
  const { container } = render(<AgoTime value={undefined} now={NOW} />);
  expect(container.textContent).toBe(EMPTY_VALUE);
  expect(container.querySelector("time")).toBeNull();
});
