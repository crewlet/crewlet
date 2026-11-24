/**
 * A time said in words is longer than its short form; the slots that hold
 * one on a line that never wraps still fit a phone's width.
 * A measurement, so it runs in a headless browser (`~/test/layout.tsx`).
 */
import "~/test/layout.tsx";
// Chatlet's own sheets, lazy in the app, after the app's sheet is placed.
import "~/styles/chatlet-frame.css";
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
// Settings › Models & Fleets' own sheet, lazy in the app too.
import "~/styles/models.css";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { Button, Card, DataGrid, LayerHost, List, ListItem } from "@crewlethq/ui";
import { BrainGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { ago } from "~/lib/ago.ts";
import { HealthLine } from "~/routes/settings/models/parts.tsx";
import type { ModelView } from "~/routes/settings/models/data.ts";
import { overflowing, sidewaysScrollers } from "~/test/layout.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

const PHONE = 390;
/** A phone's content width, inside the page's 16px gutters. */
const PHONE_CONTENT = 358;
const NOW = Date.parse("2026-11-16T12:00:00Z");
/** The widest past form: a date in another year. */
const WIDEST = ago("2025-12-31T12:00:00Z", NOW);

function mount(node: ReactNode, width = PHONE): HTMLElement {
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() => root!.render(<LayerHost>{node}</LayerHost>));
  return host;
}

test("Doclet's diaries row keeps its newest reflection inside the tile", () => {
  expect(WIDEST).toBe("on Dec 31, 2025");
  const box = mount(
    <Card as="section" padding="none" className="knowledge-agents">
      <Card.Header divided={false}>
        <Card.Title>For the agents</Card.Title>
      </Card.Header>
      <div className="knowledge-tile-rows">
        <List variant="plain">
          <ListItem
            href="#/doclet/diaries"
            leading={<BrainGlyph size="sm" />}
            trailing={
              <span className="knowledge-tile-meta" data-testid="meta">
                {`12 agents · newest ${WIDEST}`}
              </span>
            }
          >
            Agent diaries
          </ListItem>
        </List>
      </div>
    </Card>,
    PHONE_CONTENT,
  );
  const card = box.querySelector(".knowledge-agents")!;
  expect(sidewaysScrollers(box)).toEqual([]);
  expect(overflowing(card)).toEqual([]);
  const meta = box.querySelector('[data-testid="meta"]')!.getBoundingClientRect();
  expect(meta.right).toBeLessThanOrEqual(card.getBoundingClientRect().right);
});

test("Chatlet's thread card keeps its last reply and Unfollow inside the card", () => {
  // Nested as ThreadsView draws it, its room name behind the room's mark.
  const box = mount(
    <section className="chat-view">
      <div className="chat-view-scroll">
        <article className="chat-thread-card" data-card>
          <header className="chat-thread-card__head">
            <span className="chat-context__where">
              <span className="chat-context__where">
                <NamedGlyph name="hash" size="sm" />
                <span className="chat-context__name">
                  engineering-platform-reliability-and-on-call
                </span>
              </span>
              <span className="chat-muted-text">· 12 replies · last {WIDEST}</span>
            </span>
            <Button size="small" variant="ghost">
              Unfollow
            </Button>
          </header>
        </article>
      </div>
    </section>,
  );
  const card = box.querySelector(".chat-thread-card")!;
  expect(sidewaysScrollers(box)).toEqual([]);
  expect(overflowing(card)).toEqual([]);
  const button = box.querySelector("button")!.getBoundingClientRect();
  expect(button.right).toBeLessThanOrEqual(card.getBoundingClientRect().right);
  // The time never runs under Unfollow: the room's name gives way.
  const muted = box.querySelector(".chat-muted-text")!.getBoundingClientRect();
  expect(muted.right).toBeLessThanOrEqual(button.left);
  const name = box.querySelector(".chat-context__name")!;
  expect(name.scrollWidth).toBeGreaterThan(name.clientWidth);
});

test("a model's Status cell says when it is back without a sideways scroll", () => {
  const until = new Date(Date.now() + 30_000).toISOString();
  const model = {
    key: "claude-sonnet-on-the-company-subscription",
    entry: { health: "sitting_out", sitting_out: { window: "session", until } },
    admin: null,
  } as unknown as ModelView;
  const box = mount(
    <DataGrid<ModelView>
      rows={[model]}
      rowKey={(m) => m.key}
      columns={[
        { key: "model", header: "Model", floor: "12rem", cell: (m) => m.key },
        {
          key: "status",
          header: "Status",
          shrink: true,
          cell: (m) => <HealthLine model={m} compact />,
        },
      ]}
    />,
    PHONE_CONTENT,
  );
  const detail = box.querySelector(".models-health__detail")!;
  expect(detail.textContent).toBe("back in less than a minute");
  expect(sidewaysScrollers(box)).toEqual([]);
  expect(detail.getBoundingClientRect().right).toBeLessThanOrEqual(
    box.getBoundingClientRect().right,
  );
});
