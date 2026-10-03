/**
 * An object's own tabs — a seat's Overview, Work, Turns, Memory, Schedules and
 * Settings — that FOLD what does not fit into a "More" menu at the strip's end.
 *
 * # Why not the kit's row alone
 *
 * `@crewlethq/ui`'s underline `Tabs` scrolls a row wider than its box, with the
 * scrollbar hidden — and on a phone that hid a whole tab with nothing to say it
 * was there: a seat's strip ended at 364px, Settings began at 369, and the row
 * read as five complete tabs. A fade is too quiet for the same reason
 * (`SectionTabs` measured it cutting "Checklist 0" at 1280 with nothing past
 * it), so the object strip takes the section strip's answer: the tabs that FIT
 * are drawn in order, the rest are a menu, and the tab the reader is on is
 * always drawn — it takes the last place that fits rather than disappearing
 * into the menu. The arithmetic is the section strip's own (`foldTabs`).
 *
 * # Measured on a twin
 *
 * The kit draws its row whole or not at all — it takes no per-tab attribute to
 * fold one out of the flow — so the widths are read off a TWIN of it holding
 * every tab: the same component with the same items, laid out at its natural
 * width, out of the flow, invisible, inert and hidden from assistive
 * technology. It is re-measured whenever the strip or the twin changes size: a
 * count arriving, a web font replacing its fallback, the density preference.
 *
 * # Still the ARIA tabs pattern
 *
 * The drawn row is the kit's tablist, one tab stop with the arrows inside it,
 * and "More" is the button after it; a tab picked from the menu is selected
 * exactly as a click on it would select it, and it is then drawn on the strip.
 */

import { useLayoutEffect, useRef, useState, type RefObject } from "react";
import { Menu, Tabs, cx } from "@crewlethq/ui";
import { ChevronDownGlyph } from "@crewlethq/icons/glyphs";
import { foldTabs } from "./tabFit.ts";
import { fmtExact } from "~/lib/format.ts";

export interface ObjectTab {
  value: string;
  label: string;
  /** How many things are behind the tab, where the object counted them. */
  count?: number;
}

export function ObjectTabs({
  ariaLabel,
  items,
  value,
  onValueChange,
  panelId,
  className,
}: {
  /** Names the row — and, as "More of …", the menu that holds the rest. */
  ariaLabel: string;
  items: readonly ObjectTab[];
  value: string;
  onValueChange: (value: string) => void;
  /** The panel the row controls — see the kit's `Tabs`. */
  panelId?: string;
  className?: string;
}) {
  const box = useRef<HTMLDivElement>(null);
  const twin = useRef<HTMLDivElement>(null);
  const folded = useTwinFit(
    box,
    twin,
    items.map((t) => t.value),
    value,
  );
  const kit = (t: ObjectTab) => ({
    value: t.value,
    label: t.label,
    ...(t.count !== undefined ? { count: t.count } : {}),
  });
  const out = items.filter((t) => folded.includes(t.value));
  const trigger = (
    // THE CHEVRON SAYS IT OPENS, as the section strip's does: a bare "More"
    // after a row of tabs read as one more tab.
    <span className="row gap-1">
      More
      <ChevronDownGlyph size="xs" aria-hidden="true" />
    </span>
  );
  return (
    <div ref={box} className={cx("object-tabs", className)}>
      <Tabs
        ariaLabel={ariaLabel}
        variant="underline"
        className="object-tabs-strip"
        value={value}
        onValueChange={onValueChange}
        {...(panelId ? { panelId } : {})}
        items={items.filter((t) => !folded.includes(t.value)).map(kit)}
      />
      {out.length > 0 && (
        <span className="object-tabs-more">
          <Menu
            label={`More of ${ariaLabel}`}
            trigger={trigger}
            triggerVariant="ghost"
            align="end"
            items={out.map((t) => ({
              key: t.value,
              label: t.label,
              ...(t.count !== undefined ? { hint: fmtExact(t.count) } : {}),
              onSelect: () => onValueChange(t.value),
            }))}
          />
        </span>
      )}
      {/* THE TWIN every width is read from: every tab, and the trigger, at
          their natural widths. `inert` and `aria-hidden` both, because it is
          a second copy of a control and must be reachable by nothing. Its
          box is empty and clips (`.object-tabs-twin`), so a row wider than a
          phone lays out at its own width without widening the page. */}
      <div className="object-tabs-twin" aria-hidden="true" inert>
        <div ref={twin} className="object-tabs-twin-row">
          <Tabs variant="underline" value={value} items={items.map(kit)} />
          <span className="object-tabs-more" data-twin-more="">
            <Menu label="More" trigger={trigger} triggerVariant="ghost" items={[]} />
          </span>
        </div>
      </div>
    </div>
  );
}

/**
 * The values of the tabs that fold, measured off the twin after layout and
 * again whenever the strip or the twin changes size.
 */
function useTwinFit(
  box: RefObject<HTMLElement | null>,
  twin: RefObject<HTMLElement | null>,
  keys: readonly string[],
  current: string,
): string[] {
  const [folded, setFolded] = useState<string[]>([]);
  const signature = keys.join("\n");
  useLayoutEffect(() => {
    const node = box.current;
    const copy = twin.current;
    if (!node || !copy) return;
    const measure = () => {
      const style = getComputedStyle(node);
      const space =
        node.clientWidth -
        (parseFloat(style.paddingLeft) || 0) -
        (parseFloat(style.paddingRight) || 0);
      const strip = copy.querySelector('[role="tablist"]');
      const gap = strip ? parseFloat(getComputedStyle(strip).columnGap) || 0 : 0;
      const width = (el: Element | null) => (el ? el.getBoundingClientRect().width : 0);
      const widths = [...copy.querySelectorAll('[role="tab"]')].map(width);
      const next = foldTabs({
        widths,
        current: keys.indexOf(current),
        space,
        gap,
        more: width(copy.querySelector("[data-twin-more]")),
      })
        .map((i) => keys[i]!)
        .filter(Boolean);
      setFolded((was) => (was.join("\n") === next.join("\n") ? was : next));
    };
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    watch.observe(copy);
    return () => watch.disconnect();
    // `keys` is read through its signature: a new array with the same tabs is
    // the same strip.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [box, twin, signature, current]);
  return folded;
}
