/**
 * The phone's tab bar: a person's day along the bottom of the window, under
 * a thumb.
 *
 * # Derived from the one table, never a list beside it
 *
 * The tabs are the workspaces `nav.ts` places as a phone TAB — Today, Inbox
 * and My work — then Search, then More; More holds the workspaces it places
 * in MORE (Work, Team, Knowledge), the console for an admin, and the way to
 * the rest of the sidebar (projects, favourites, who you are), which the
 * drawer already draws. A workspace added to the table is placed on a phone
 * by the same edit, and `router.test.ts` holds every person-shell workspace
 * to exactly one of the two.
 *
 * # The person shell's alone
 *
 * Nobody runs an engine from a phone's tab bar, and the console's screens are
 * tables and charts a phone scrolls through rather than works in: the console
 * keeps its drawer. Drawn only under `breakpoint.phone` (`frame.css`), where
 * the sidebar is a drawer and the bottom of the window is the one place a
 * thumb reaches without a second hand.
 */

import { Menu, useAppShell, type MenuEntry } from "@crewlethq/ui";
import { EllipsisGlyph, KeyGlyph, MenuGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useRoute } from "./router.tsx";
import { adminPathOf, phoneMore, phoneTabs, workspaceOf } from "./nav.ts";
import { glyphFor } from "~/ui/glyph.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { inboxFigure, useInboxCounts, waitingWords } from "~/lib/useInboxCounts.ts";

export function PhoneBar({ onSearch }: { onSearch: () => void }) {
  const route = useRoute();
  const nav = useNavigator();
  const viewer = useViewer();
  const shell = useAppShell();
  const inbox = useInboxCounts();
  const here = workspaceOf(route.path);
  const more: MenuEntry[] = [
    ...phoneMore().map((ws) => {
      const Glyph = glyphFor(ws.icon);
      return {
        key: ws.key,
        label: ws.label,
        icon: <Glyph size="sm" />,
        onSelect: () => nav.to(ws.path),
      };
    }),
    // THE DOOR, an admin's alone, as the sidebar's foot draws it.
    ...(viewer.admin
      ? [
          { kind: "separator" as const, key: "admin" },
          {
            key: "admin-console",
            label: "Admin console",
            icon: <KeyGlyph size="sm" />,
            onSelect: () => {
              const twin = adminPathOf(route.path);
              nav.to(twin.path, twin.query);
            },
          },
        ]
      : []),
    { kind: "separator", key: "rest" },
    {
      key: "navigation",
      label: "Projects, favourites and you",
      icon: <MenuGlyph size="sm" />,
      onSelect: shell.openDrawer,
    },
  ];
  return (
    <nav className="phone-bar" aria-label="Your day">
      {phoneTabs().map((ws) => {
        const Glyph = glyphFor(ws.icon);
        const badge = ws.key === "inbox" && inbox.waiting !== null && inbox.waiting > 0;
        return (
          <a
            key={ws.key}
            className="phone-tab"
            href={href(ws.path)}
            aria-current={here === ws.key ? "page" : undefined}
          >
            <Glyph size="md" aria-hidden="true" />
            <span className="phone-tab-label">{ws.label}</span>
            {badge && (
              <span className="phone-tab-badge t-num">
                {inboxFigure(inbox)}
                <span className="sr-only"> {waitingWords(inbox)}</span>
              </span>
            )}
          </a>
        );
      })}
      <button type="button" className="phone-tab" onClick={onSearch}>
        <SearchGlyph size="md" aria-hidden="true" />
        <span className="phone-tab-label">Search</span>
      </button>
      {/* THE KIT'S MENU BUTTON, wearing a tab's face: its glyph over its word,
          as every tab beside it. */}
      <Menu
        label="More"
        className="phone-more"
        trigger={
          <span className="phone-tab">
            <EllipsisGlyph size="md" aria-hidden="true" />
            <span className="phone-tab-label">More</span>
          </span>
        }
        triggerVariant="ghost"
        align="end"
        items={more}
      />
    </nav>
  );
}
