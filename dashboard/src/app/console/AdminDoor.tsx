/**
 * The person sidebar's door into the console — an admin's, and nobody else's.
 *
 * # Where it goes
 *
 * To the CONSOLE TWIN of what is open (`adminPathOf`): a teammate's profile
 * to their seat in Agent setup, a task to its turns in Live, About to the
 * Charter — and the Overview where the place has no twin. The door keeps the
 * reader's place, as `g .` does, because an admin crossing to fix something
 * is crossing to fix the thing they were looking at.
 *
 * # What its figure counts
 *
 * The conditions an ADMIN decides (`conditionsToDecide`): a seat the engine
 * stopped, a budget refusing, a run nobody answered. A NEUTRAL count — not
 * the accent, which is the Inbox's, the one thing in the chrome waiting on the
 * reader as a person — with a dot in the worst state among them, and a title
 * naming each, so a reader deciding whether to cross knows what is on the
 * other side.
 *
 * IN THE CONSOLE'S CHUNK: its read (`useAttention`, which asks for the
 * durable coding runs) is an admin's question, and a member's browser never
 * downloads it — the person sidebar draws this for an admin only.
 */

import { SidebarNav, StatusDot } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useRoute } from "~/app/router.tsx";
import { adminPathOf } from "~/app/nav.ts";
import { conditionsToDecide, type Attention, type Severity } from "~/lib/attention.ts";
import { useAttention } from "~/lib/useAttention.ts";
import { plural } from "~/lib/format.ts";
import { NavRow } from "~/app/sidebar/NavRow.tsx";

/** The dot's tone for the worst condition: colour carries the state. */
const TONE: Record<Severity, "danger" | "warning" | "info"> = {
  critical: "danger",
  caution: "warning",
  info: "info",
};

const RANK: Record<Severity, number> = { critical: 0, caution: 1, info: 2 };

/** What the door's figure says, decided once and testable without a render. */
export function doorFigure(
  conditions: readonly Attention[],
): { value: number; tone: "danger" | "warning" | "info"; title: string } | undefined {
  if (conditions.length === 0) return undefined;
  const worst = conditions.reduce((a, b) => (RANK[b.severity] < RANK[a.severity] ? b : a));
  return {
    value: conditions.length,
    tone: TONE[worst.severity],
    title: `${plural(conditions.length, "thing")} need${conditions.length === 1 ? "s" : ""} an admin: ${conditions
      .map((c) => c.title)
      .join(" · ")}`,
  };
}

export function AdminDoor() {
  const route = useRoute();
  const figure = doorFigure(conditionsToDecide(useAttention()));
  const crossing = adminPathOf(route.path);
  return (
    <SidebarNav label="Admin console">
      <NavRow
        label="Admin console"
        glyph={<KeyGlyph size="sm" />}
        path={crossing.path}
        query={crossing.query}
        current={false}
        title={figure?.title}
        count={
          figure
            ? {
                value: figure.value,
                label: figure.value === 1 ? "needs an admin" : "need an admin",
                mark: <StatusDot tone={figure.tone} />,
              }
            : undefined
        }
      />
    </SidebarNav>
  );
}
