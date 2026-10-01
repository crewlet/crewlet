/**
 * The one timer a screen's hand-rolled REST read asks itself again on, and the
 * one event — the socket coming back.
 *
 * Two properties hold every caller up: an answer's wait REPLACES whatever an
 * earlier answer armed, so two answers never become two reads, and a screen
 * that has gone asks nothing more — a read nobody will render is waste against
 * a node that may already be struggling.
 */

import { cleanup, render } from "@testing-library/react";
import { act } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useReread, useRereadOnReconnect, type Reread } from "./reread.ts";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

let held: Reread | null = null;

function Holder() {
  held = useReread();
  return null;
}

beforeEach(() => {
  vi.useFakeTimers();
  held = null;
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("an answer's wait replaces the one an earlier answer armed", () => {
  render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  held!.after(12_000, read);
  vi.advanceTimersByTime(11_999);
  expect(read).not.toHaveBeenCalled();
  vi.advanceTimersByTime(1);
  expect(read).toHaveBeenCalledTimes(1);
  vi.advanceTimersByTime(60_000);
  expect(read).toHaveBeenCalledTimes(1);
});

test("null arms nothing, and disarms what was armed", () => {
  render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  held!.after(null, read);
  vi.advanceTimersByTime(60_000);
  expect(read).not.toHaveBeenCalled();
});

test("a read that is starting cancels the one that was due", () => {
  render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  held!.cancel();
  vi.advanceTimersByTime(60_000);
  expect(read).not.toHaveBeenCalled();
});

test("a screen that has gone asks nothing more", () => {
  const view = render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  view.unmount();
  vi.advanceTimersByTime(60_000);
  expect(read).not.toHaveBeenCalled();
});

/**
 * THE SOCKET COMING BACK IS A TRANSITION, not a state: a read is asked once
 * when a socket that was down is up again, and never because one was up all
 * along, nor because the read itself changed identity — every render of a
 * loader makes a new one, and a re-read per render would be a loop.
 */
function Reconnecting({ read, enabled = true }: { read: () => void; enabled?: boolean }) {
  useRereadOnReconnect(read, enabled);
  return null;
}

function connectedClient(connected: boolean): { store: Store; socket: LiveSocket } {
  const store = new Store();
  store.setConnected(connected);
  return { store, socket: new LiveSocket(store) };
}

test("a socket that comes back asks once, and one that was up all along never", () => {
  const client = connectedClient(true);
  const read = vi.fn();
  const view = render(
    <ClientContext.Provider value={client}>
      <Reconnecting read={read} />
    </ClientContext.Provider>,
  );
  // A NEW READ, as every render of a loader hands over: asks nothing.
  view.rerender(
    <ClientContext.Provider value={client}>
      <Reconnecting read={() => read()} />
    </ClientContext.Provider>,
  );
  expect(read).not.toHaveBeenCalled();

  act(() => client.store.setConnected(false));
  expect(read).not.toHaveBeenCalled();
  act(() => client.store.setConnected(true));
  expect(read).toHaveBeenCalledTimes(1);
});

test("a read that is not enabled is not asked when the socket comes back", () => {
  const client = connectedClient(false);
  const read = vi.fn();
  render(
    <ClientContext.Provider value={client}>
      <Reconnecting read={read} enabled={false} />
    </ClientContext.Provider>,
  );
  act(() => client.store.setConnected(true));
  expect(read).not.toHaveBeenCalled();
});
