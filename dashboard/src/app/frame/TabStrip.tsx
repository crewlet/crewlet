/**
 * An object's tabs, bound to `tab=` in the URL.
 *
 * TAKES A LIST, never a fixed set. An agent seat has seven tabs and a human
 * seat has three, and a strip with a hard-coded seven rendered four tabs that
 * answered nothing for every person in the company — then left the reader on
 * one of them when they arrived from an agent's page, because the tab survived
 * the navigation and nothing checked it belonged.
 *
 * So the strip also REPORTS what it actually selected: a `tab=` naming a tab
 * this object does not have resolves to the first one, and the caller renders
 * that rather than the value in the URL.
 */

import { useRef } from "react";
import { useParam } from "../router.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { cx } from "~/ui/primitives.tsx";
import { useKeyChords } from "~/lib/keys.ts";

export interface Tab {
  key: string;
  label: string;
  icon?: IconName;
  count?: number;
}

/**
 * The selected tab, resolved against the tabs this object HAS.
 *
 * Returns the key to render and the setter, so no caller has to repeat the
 * "is this tab real" check that `Seat.tsx` got wrong.
 */
export function useTab(tabs: Tab[], fallback?: string): [string, (key: string) => void] {
  const first = fallback ?? tabs[0]?.key ?? "";
  const [asked, setAsked] = useParam("tab", first, "section");
  const shown = tabs.some((t) => t.key === asked) ? asked : first;
  return [shown, setAsked];
}

export function TabStrip({
  tabs,
  value,
  onChange,
  ariaLabel = "Tabs",
}: {
  tabs: Tab[];
  value: string;
  onChange: (key: string) => void;
  ariaLabel?: string;
}) {
  const strip = useRef<HTMLDivElement>(null);

  // `1`–`9` select a tab when nothing has focus, which is the shortcut every
  // tool of this shape has and the one a reader tries first.
  useKeyChords(
    // ONE BINDING PER DIGIT rather than a range check, because the chord list
    // IS the shortcut table: a tab that is not there has no binding, so the
    // digit falls through to whatever else wants it instead of being
    // swallowed by a strip that could not use it.
    tabs.slice(0, 9).map((tab, i) => ({
      key: String(i + 1),
      run: () => onChange(tab.key),
    })),
  );

  // ROVING FOCUS, which is what makes this a tablist rather than a row of
  // buttons: arrow keys move between tabs and only the selected one is in the
  // page's tab order.
  function onKeyDown(e: React.KeyboardEvent): void {
    const at = tabs.findIndex((t) => t.key === value);
    let next = -1;
    if (e.key === "ArrowRight") next = (at + 1) % tabs.length;
    if (e.key === "ArrowLeft") next = (at - 1 + tabs.length) % tabs.length;
    if (e.key === "Home") next = 0;
    if (e.key === "End") next = tabs.length - 1;
    if (next < 0) return;
    e.preventDefault();
    const tab = tabs[next];
    if (!tab) return;
    onChange(tab.key);
    strip.current?.querySelector<HTMLElement>(`[data-tab="${tab.key}"]`)?.focus();
  }

  return (
    <div
      className="tab-strip"
      role="tablist"
      aria-label={ariaLabel}
      ref={strip}
      onKeyDown={onKeyDown}
    >
      {tabs.map((tab) => {
        const selected = tab.key === value;
        return (
          <button
            key={tab.key}
            role="tab"
            data-tab={tab.key}
            aria-selected={selected}
            tabIndex={selected ? 0 : -1}
            className={cx("tab", selected && "active")}
            onClick={() => onChange(tab.key)}
          >
            {tab.icon && <Icon name={tab.icon} size="sm" />}
            <span>{tab.label}</span>
            {tab.count != null && <span className="tab-count t-num">{tab.count}</span>}
          </button>
        );
      })}
    </div>
  );
}
