/**
 * The pane beside the conversation takes focus when it opens and gives it
 * back when it closes.
 *
 * THE INVARIANTS: opening a pane moves focus to its heading, so a keyboard or
 * screen reader reader lands where it opened; Escape inside closes it; closing
 * returns focus to the control that opened it, and when that control was a
 * menu's item, which is gone by the time the pane closes, to the menu's
 * trigger.
 */

import { useState } from "react";
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { InsetToastProvider } from "~/app/toastInset.tsx";
import { actEngine } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED } from "~/test/chatAccess.ts";

import { mountInFrame } from "./frameKit.tsx";
import { useChatNav } from "./nav.tsx";
import { ChatPaneHost } from "./Pane.tsx";

/** A control that opens a profile, a menu whose item opens one, and the pane host. */
function Harness() {
  const nav = useChatNav();
  const [menu, setMenu] = useState(false);
  return (
    <>
      <button type="button" onClick={() => nav.openPane({ kind: "profile", handle: "bo" })}>
        Open
      </button>
      <button type="button" onClick={() => setMenu(true)}>
        More
      </button>
      {menu && (
        <div role="menu">
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              // THE MENU CLOSES AS ITS ITEM IS PRESSED, as the kit's does.
              setMenu(false);
              nav.openPane({ kind: "profile", handle: "bo" });
            }}
          >
            Profile
          </button>
        </div>
      )}
      <ChatPaneHost pane={nav.pane} onClose={nav.closePane} />
    </>
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount() {
  actEngine();
  const mounted = mountInFrame(<Harness />, {
    hash: "#/chatlet",
    answers: {
      chat_channels: {
        ...SERVED,
        read_state: true,
        waiting: [],
        waiting_capped: false,
        channels: [],
      },
    },
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return mounted;
}

test("opening moves focus to the pane's heading, and closing gives it back to the opener", async () => {
  mount();
  const opener = screen.getByRole("button", { name: "Open" });
  opener.focus();
  fireEvent.click(opener);
  const heading = await screen.findByRole("heading", { name: "Profile" });
  await waitFor(() => expect(document.activeElement).toBe(heading));
  fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("heading", { name: "Profile" })).toBeNull());
  expect(document.activeElement).toBe(opener);
});

test("Escape inside the pane closes it", async () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
  const heading = await screen.findByRole("heading", { name: "Profile" });
  fireEvent.keyDown(heading, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("heading", { name: "Profile" })).toBeNull());
});

test("a pane opened from a menu gives focus back to the menu's trigger", async () => {
  mount();
  const trigger = screen.getByRole("button", { name: "More" });
  trigger.focus();
  fireEvent.click(trigger);
  const item = screen.getByRole("menuitem", { name: "Profile" });
  item.focus();
  fireEvent.click(item);
  await screen.findByRole("heading", { name: "Profile" });
  expect(item.isConnected).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("heading", { name: "Profile" })).toBeNull());
  expect(document.activeElement).toBe(trigger);
});

// A TOAST NEVER LIES OVER AN OPEN PANE: the stack stands at the window's foot,
// beside the pane, as far in as the pane's start edge. On Chatlet it stood at
// the top right under the bar, over a room's details. Mutation: drop the
// host's ask, and the stack keeps no inline inset.
test("an open pane keeps the toast stack beside it, and lets go when it closes", async () => {
  const real = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.classList.contains("chat-pane")
      ? ({ left: window.innerWidth - 400, width: 400, height: 700 } as DOMRect)
      : real.call(this);
  });
  actEngine();
  const mounted = mountInFrame(
    <InsetToastProvider>
      <Harness />
    </InsetToastProvider>,
    {
      hash: "#/chatlet",
      answers: {
        chat_channels: {
          ...SERVED,
          read_state: true,
          waiting: [],
          waiting_capped: false,
          channels: [],
        },
      },
    },
  );
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  // THE APP HOST'S STACK: the one any inset is published on.
  const hosted = () =>
    [...document.querySelectorAll<HTMLElement>(".crewlet-toaster")].find((el) => {
      const value = el.style.getPropertyValue("--crewlet-toaster-inset-inline");
      return value !== "" && value !== "0px";
    });
  const inline = () => hosted()?.style.getPropertyValue("--crewlet-toaster-inset-inline") ?? "";
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
  await screen.findByRole("heading", { name: "Profile" });
  await waitFor(() => expect(inline()).toBe("400px"));
  expect(hosted()?.className).toMatch(/bottom-right/);
  fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await waitFor(() => expect(["", "0px"]).toContain(inline()));
  vi.restoreAllMocks();
});
