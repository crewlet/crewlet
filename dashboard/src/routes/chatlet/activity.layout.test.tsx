/**
 * AN OPEN ACTIVITY FILLS ITS COLUMN AND SCROLLS INSIDE IT. Activity's chosen
 * item is a chat pane drawn as a list's detail: it takes the detail column's
 * whole box, never the pane's resting width, and its own body scrolls, so the
 * last reply and the reply box are in reach however long the thread.
 *
 * Measured with the stylesheets where a built page puts them (`~/test/layout.tsx`):
 * Chatlet's sheets are a lazy chunk's, linked before the app's, so a Chatlet
 * rule sizing the kit's panel loses there, and only the kit's own rule holds.
 */
import "~/test/layout.tsx";
// Chatlet's own sheets, lazy in the app, after the app's sheet is placed.
import "~/styles/chatlet-frame.css";
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
import { afterEach, describe, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { ChatThread, LayerHost, ListDetail, ToastProvider } from "@crewlethq/ui";

import { sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";

import { ChatDetailPlacement, ChatSidePanel, DETAIL_PANE_CLASS } from "./PanePanel.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

const REPLY =
  "Fixed the slot picker on narrow screens: the confirm button is pinned to the foot of the sheet, and the calendar collapses to a week strip under 400 pixels.";

/** Activity's open item as the view draws it: the list beside a chat pane in the detail. */
function OpenItem({ replies }: { replies: number }) {
  return (
    <ListDetail list={<p>The activity list.</p>} showing="detail" listWidth="half" flush>
      <ChatDetailPlacement back="Back to Activity">
        <ChatSidePanel
          title="Thread"
          subtitle="#engineering"
          flush
          flow
          onClose={() => {}}
          footer={
            <div className="chat-pane-foot" data-testid="reply-box" style={{ height: 120 }}>
              Reply…
            </div>
          }
        >
          <ChatThread label="Thread" className="chat-pane-transcript" flush>
            {Array.from({ length: replies }, (_, i) => (
              <p key={i} data-testid="reply" style={{ margin: 0, padding: "8px 20px" }}>
                {i + 1}. {REPLY}
              </p>
            ))}
          </ChatThread>
        </ChatSidePanel>
      </ChatDetailPlacement>
    </ListDetail>
  );
}

async function mount(node: ReactNode, width: number, height: number): Promise<HTMLElement> {
  host = document.createElement("div");
  // A column of the window's height, as the shell's fill screen gives a view.
  host.style.cssText = `display: flex; flex-direction: column; width: ${width}px; height: ${height}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <ToastProvider>
        <LayerHost>{node}</LayerHost>
      </ToastProvider>,
    ),
  );
  await eventually(() => expect(host!.querySelector(`.${DETAIL_PANE_CLASS}`)).not.toBeNull());
  await document.fonts.ready;
  await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
  return host;
}

const box = (el: Element) => el.getBoundingClientRect();

/** The element inside the pane that scrolls its replies. */
function scroller(pane: HTMLElement): HTMLElement {
  const all = [pane, ...pane.querySelectorAll<HTMLElement>("*")];
  const found = all.find((el) => {
    const overflow = getComputedStyle(el).overflowY;
    return (overflow === "auto" || overflow === "scroll") && el.scrollHeight > el.clientHeight;
  });
  if (!found) throw new Error("nothing inside the pane scrolls its replies");
  return found;
}

describe.each([
  { width: 1200, height: 700, label: "split beside the list" },
  { width: 700, height: 800, label: "split at a tablet's width" },
  { width: 390, height: 760, label: "folded to one pane on a phone" },
])("an open thread, $label ($width px)", ({ width, height }) => {
  test("fills the detail column's box, with nothing scrolling sideways", async () => {
    const at = await mount(<OpenItem replies={3} />, width, height);
    const pane = at.querySelector<HTMLElement>(`.${DETAIL_PANE_CLASS}`)!;
    const column = at.querySelector<HTMLElement>(".crewlet-list-detail__detail")!;
    const [p, c] = [box(pane), box(column)];
    expect(Math.abs(p.left - c.left)).toBeLessThanOrEqual(1);
    expect(Math.abs(p.width - c.width)).toBeLessThanOrEqual(1);
    expect(Math.abs(p.top - c.top)).toBeLessThanOrEqual(1);
    expect(Math.abs(p.height - c.height)).toBeLessThanOrEqual(1);
    expect(sidewaysScrollers(at)).toEqual([]);
  });

  test("a long thread scrolls inside the pane to its last reply, with the reply box in reach", async () => {
    const at = await mount(<OpenItem replies={40} />, width, height);
    const pane = at.querySelector<HTMLElement>(`.${DETAIL_PANE_CLASS}`)!;
    const column = at.querySelector<HTMLElement>(".crewlet-list-detail__detail")!;
    // The pane is held to its column, never as tall as its replies.
    expect(Math.abs(box(pane).height - box(column).height)).toBeLessThanOrEqual(1);
    const log = scroller(pane);
    log.scrollTop = log.scrollHeight;
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
    const replies = at.querySelectorAll('[data-testid="reply"]');
    const last = box(replies[replies.length - 1]!);
    const reply = box(at.querySelector('[data-testid="reply-box"]')!);
    const p = box(pane);
    expect(last.bottom).toBeLessThanOrEqual(box(log).bottom + 1);
    expect(last.top).toBeGreaterThanOrEqual(p.top);
    expect(reply.top).toBeGreaterThanOrEqual(p.top);
    expect(reply.bottom).toBeLessThanOrEqual(p.bottom + 1);
    expect(sidewaysScrollers(at)).toEqual([]);
  });
});
