/**
 * A screenful of link previews asks one wave of the socket's queries at a
 * time.
 *
 * A transcript asks one `chat_unfurls` per message that carries links, and a
 * room's Shared list one per link. Unbounded, a screenful of them filled the
 * socket past what it runs at once and held the room's own re-reads behind
 * every one, so the previews take at most `MAX_IN_FLIGHT_QUERIES` reads in
 * flight, and each answer lets the next one ask.
 */

import { act, cleanup, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { MAX_IN_FLIGHT_QUERIES } from "~/contract/wire.ts";
import { SERVED } from "~/test/chatAccess.ts";

import { mountInFrame } from "./frameKit.tsx";
import { MessageUnfurls } from "./MessageLinks.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

test("a screenful of previews asks one wave at a time, and the next as each answers", async () => {
  const pending: (() => void)[] = [];
  const messages = MAX_IN_FLIGHT_QUERIES + 3;
  const { socket } = mountInFrame(
    <>
      {Array.from({ length: messages }, (_, n) => (
        <MessageUnfurls key={n} links={[`https://example.com/${n}`]} />
      ))}
    </>,
    {
      answer: (what) => {
        if (what === "chat_channels") {
          return Promise.resolve({ ...SERVED, read_state: true, channels: [] });
        }
        if (what === "chat_unfurls") {
          return new Promise((resolve) => pending.push(() => resolve({ ...SERVED, unfurls: [] })));
        }
        return Promise.reject(new Error(`unasked: ${what}`));
      },
    },
  );
  const asked = () => socket.query.mock.calls.filter(([what]) => what === "chat_unfurls").length;
  await waitFor(() => expect(asked()).toBe(MAX_IN_FLIGHT_QUERIES));
  await new Promise((r) => setTimeout(r, 20));
  expect(asked()).toBe(MAX_IN_FLIGHT_QUERIES);

  await act(async () => pending.shift()!());
  await waitFor(() => expect(asked()).toBe(MAX_IN_FLIGHT_QUERIES + 1));

  await act(async () => {
    while (pending.length > 0) pending.shift()!();
  });
  await waitFor(() => expect(asked()).toBe(messages));
});
