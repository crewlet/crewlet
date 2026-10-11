/**
 * The Admin group at the end of an object's ⋯ menu in the person shell: the
 * way from a task or a teammate to the same thing in the admin console.
 *
 * # Why a menu entry, and never a link on the page
 *
 * The person shell draws nothing about how the machine runs a task or a seat
 * — no trace link, no turn count — and an admin reading it as a person is
 * still reading the person's page. The crossing is one deliberate step away:
 * the sidebar's door, `g .`, and this entry, each going to the console TWIN
 * of what is open (`adminPathOf`) — a task to the turns charged to it, a
 * teammate to their seat in Agent setup. The entry wears its key and says it
 * is an admin's, so an admin always knows what a member would not see.
 *
 * # Only for an admin, and only where there is a twin
 *
 * A member has no console, so for them this is no entries at all — never a
 * held one, since the console is not a write a member could be given. And a
 * place whose crossing falls back to the console's first screen has no twin
 * to name, so it offers none: "open in the admin console" that opens the
 * Overview is not about the thing the reader was looking at.
 */

import type { MenuEntry } from "@crewlethq/ui";
import { Kbd } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useRoute } from "./router.tsx";
import { ADMIN_HEAD, adminPathOf, shellOf } from "./nav.ts";
import { capsOf, keyRow } from "./keymap.ts";
import { useViewer } from "~/lib/viewer.ts";

/** The Admin group's entries for the page on screen: none, or a separator and the crossing. */
export function useAdminGroup(): MenuEntry[] {
  const viewer = useViewer();
  const route = useRoute();
  const nav = useNavigator();
  if (!viewer.admin || shellOf(route.path) !== "person") return [];
  const twin = adminPathOf(route.path);
  // THE KEY AS THE LEGEND WRITES IT: the prefix, then the press.
  const swap = keyRow("shell.swap").presses[0]!;
  if (twin.path.length === 1 && twin.path[0] === ADMIN_HEAD && route.path[0] !== "today") {
    return [];
  }
  return [
    { kind: "separator", key: "admin-group" },
    {
      key: "admin-console",
      label: "Open in the admin console",
      description: "Admin — what a member does not see",
      icon: <KeyGlyph size="sm" />,
      hint: <Kbd keys={[...(swap.after ? [swap.after] : []), ...capsOf(swap)]} subtle />,
      onSelect: () => nav.to(twin.path, twin.query),
    },
  ];
}
