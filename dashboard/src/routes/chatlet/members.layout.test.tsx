/**
 * An agent's member row with its follow switch and Remove, at the members
 * pane's width: the switch's words and the follow rule it sets are drawn
 * whole; the team gives way.
 */
import "~/test/layout.tsx";
import "~/styles/chatlet-frame.css";
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { IconButton, PersonRow, Switch, VisuallyHidden } from "@crewlethq/ui";
import { XGlyph } from "@crewlethq/icons/glyphs";
import { followingWords, memberDetail } from "./Details.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

// 366px is the members list in the room's details pane.
test("an agent's follow rule is whole beside its switch and Remove", async () => {
  await page.viewport(1440, 900);
  host = document.createElement("div");
  host.style.cssText = "width: 366px;";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <ul className="chat-member-list">
        <li>
          <PersonRow
            avatar={<span style={{ display: "inline-block", width: 32, height: 32 }} />}
            name="Agent PM"
            detail={memberDetail("Product Management", followingWords(false))}
            onSelect={() => {}}
            action={
              <span className="row gap-1">
                <Switch
                  size="sm"
                  label={
                    <>
                      Every message <VisuallyHidden>to Agent PM</VisuallyHidden>
                    </>
                  }
                  checked={false}
                />
                <IconButton
                  variant="ghost-danger"
                  size="sm"
                  icon={<XGlyph />}
                  label="Remove Agent PM from #release-planning"
                />
              </span>
            }
          />
        </li>
      </ul>,
    ),
  );
  await document.fonts.ready;
  const detail = host.querySelector<HTMLElement>(".crewlet-person-row__detail")!;
  const text = detail.firstChild!;
  const rule = followingWords(false);
  const start = (text.textContent ?? "").indexOf(rule);
  expect(start).toBeGreaterThanOrEqual(0);
  const range = document.createRange();
  range.setStart(text, start);
  range.setEnd(text, start + rule.length);
  expect(range.getBoundingClientRect().right).toBeLessThanOrEqual(
    detail.getBoundingClientRect().right,
  );
  const words = host.querySelector<HTMLElement>(".crewlet-switch__label")!;
  expect(words.scrollWidth).toBeLessThanOrEqual(words.clientWidth);
  expect(words.getBoundingClientRect().right).toBeLessThanOrEqual(
    host.getBoundingClientRect().right,
  );
});
