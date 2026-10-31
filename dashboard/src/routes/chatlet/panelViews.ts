/**
 * The views the Rooms panel lists above the rooms, and the figure each one
 * carries.
 *
 * FROM THE CHAT ROW'S OWN SECTIONS (`app/nav.ts`), so the panel, the trail,
 * the tab title and the command palette call each view by one name: a view
 * the row declares as a tab is a row here, under the label and glyph the row
 * gives it, and a view the row reaches another way (search, browse, a new
 * message) is not.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";

import { samePath } from "~/app/router.tsx";
import { workspaceRow } from "~/app/nav.ts";
import { CHAT_VIEWS, type ChatView } from "~/app/routes.ts";

import type { ChatDrafts } from "./drafts.ts";
import type { OutboxHandle } from "./outbox.ts";

/** A view the panel lists. */
export interface PanelView {
  kind: Extract<ChatView, "activity" | "threads" | "drafts" | "saved">;
  label: string;
  icon: GlyphName;
}

const PANEL_KINDS: readonly PanelView["kind"][] = ["activity", "threads", "drafts", "saved"];

const CHATLET = workspaceRow("chatlet");

/** The row's sections that are panel views, in the row's order. */
export const PANEL_VIEWS: readonly PanelView[] = (CHATLET?.sections ?? []).flatMap((section) => {
  if (!CHATLET || samePath(section.path, CHATLET.path) || section.tab === false) return [];
  const kind = CHAT_VIEWS.find((view) => samePath(section.path, ["chatlet", view]));
  return kind && (PANEL_KINDS as readonly string[]).includes(kind)
    ? [{ kind: kind as PanelView["kind"], label: section.label, icon: section.icon }]
    : [];
});

/** What waits on the reader in each view, as the reader's own state says. */
export interface ReaderCounts {
  /** Activity items the reader has not seen: mentions, replies, reactions. */
  activity: number;
  /** New replies in the threads the reader follows. */
  threads: number;
  /** Messages the reader saved for later. */
  saved: number;
}

/** A figure at the end of a view's row. */
export interface ViewFigure {
  value: number;
  /** The word read after the figure, as part of the row's name. */
  label: string;
  /**
   * `attention` is the accent badge: something waits on the reader and asks
   * them to act. `quiet` is a count of what the view holds.
   */
  tone: "attention" | "quiet";
}

/**
 * Unsent words, as Drafts and sent counts them: every box holding a draft,
 * and every message the engine refused or never answered, which waits on the
 * reader to send again or discard. A message still on its way waits on
 * nobody.
 */
export function unsentCount(drafts: Pick<ChatDrafts, "count">, outbox: Pick<OutboxHandle, "rows">) {
  return (
    drafts.count +
    outbox.rows.filter((row) => row.state === "unknown" || row.state === "refused").length
  );
}

/** The figure a view's row carries, or null for none. */
export function viewCount(
  kind: PanelView["kind"],
  held: {
    counts: ReaderCounts | null;
    drafts: Pick<ChatDrafts, "count">;
    outbox: Pick<OutboxHandle, "rows">;
  },
): ViewFigure | null {
  const figure = (value: number, label: string, tone: ViewFigure["tone"]) =>
    value > 0 ? { value, label, tone } : null;
  switch (kind) {
    case "activity":
      return held.counts && figure(held.counts.activity, "unread", "attention");
    case "threads":
      return held.counts && figure(held.counts.threads, "new replies", "quiet");
    case "drafts":
      return figure(unsentCount(held.drafts, held.outbox), "unsent", "quiet");
    case "saved":
      return held.counts && figure(held.counts.saved, "saved", "quiet");
  }
}
