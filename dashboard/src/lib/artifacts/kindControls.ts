/**
 * A kind's own controls, published to the artifact's bar.
 *
 * A deck's Edit, Grid and Present, a doc's Edit and its Print belong to the
 * kind's view, which knows when they apply (only the head is edited, a grid
 * has nothing to show while a deck is still read), and they are drawn in the
 * page's one bar rather than a second row over the artifact. So the view
 * PUBLISHES them, as data, and the page lays them out with every artifact's
 * own controls: the views in the middle of the bar, the tools at its end, and
 * the rows in More. Data rather than elements because the bar draws each one
 * twice, inline and folded into More on a narrow bar.
 *
 * A HOOK, AS `usePageMenu` IS: every press goes through the controls the view
 * published LAST, so a callback that closes over newer state is the one that
 * runs, and what is laid out is published again only when something drawn
 * moves (a label, the view on show, a pressed or held tool). Published before
 * the paint, so the bar never draws a frame without them.
 *
 * Only the primary drawing publishes: a side of a comparison is a second
 * drawing of the same kind, and its controls would fight the page's.
 */

import { createContext, useContext, useLayoutEffect, useRef, type MutableRefObject } from "react";
import type { ArtifactBarModes, ArtifactBarTool, MenuEntry } from "@crewlethq/ui";

export interface KindControls {
  /** The kind's views: a deck's Edit, Grid and Present. */
  modes?: ArtifactBarModes;
  /** Its own tools, before every artifact's: a doc's Edit. */
  tools?: readonly ArtifactBarTool[];
  /** Its rows in More: Print, the presenter's window. */
  menu?: readonly MenuEntry[];
}

/** Set by the artifact's page; a no-op where a view is drawn without one. */
export const KindControlsSink = createContext<(controls: KindControls | null) => void>(() => {});

/** Publish this view's controls to the page's bar while it is mounted; `null` publishes none. */
export function useKindControls(controls: KindControls | null): void {
  const publish = useContext(KindControlsSink);
  const latest = useRef(controls);
  useLayoutEffect(() => {
    latest.current = controls;
  });
  const key = drawnKey(controls);
  useLayoutEffect(() => {
    publish(latest.current === null ? null : throughLatest(latest));
    return () => publish(null);
    // The dependency is what is DRAWN, for the reason `usePageMenu` gives.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, publish]);
}

/** What the bar draws from a set of controls, as one comparable string. */
function drawnKey(c: KindControls | null): string {
  if (c === null) return "null";
  return JSON.stringify([
    c.modes ? [c.modes.label, c.modes.value, c.modes.options.map((o) => [o.value, o.label])] : null,
    (c.tools ?? []).map((t) => [
      t.key,
      t.label,
      t.pressed ?? false,
      t.disabledReason ?? "",
      t.labelled ?? false,
      t.badge ?? "",
      t.keys ?? [],
    ]),
    (c.menu ?? []).map((e) =>
      e.kind === "separator"
        ? [e.key]
        : [e.key, e.label, e.disabled ?? false, e.checked ?? null, e.danger ?? false],
    ),
  ]);
}

/** The controls as published, every press reaching the newest of them. */
function throughLatest(latest: MutableRefObject<KindControls | null>): KindControls {
  const now = latest.current!;
  const tool = (key: string) => latest.current?.tools?.find((t) => t.key === key);
  const row = (key: string) => latest.current?.menu?.find((e) => e.key === key);
  return {
    ...(now.modes
      ? {
          modes: {
            ...now.modes,
            onValueChange: (value: string) => latest.current?.modes?.onValueChange(value),
          },
        }
      : {}),
    ...(now.tools
      ? { tools: now.tools.map((t) => ({ ...t, onSelect: () => tool(t.key)?.onSelect() })) }
      : {}),
    ...(now.menu
      ? {
          menu: now.menu.map((e) =>
            e.kind === "separator"
              ? e
              : {
                  ...e,
                  onSelect: () => {
                    const live = row(e.key);
                    if (live && live.kind !== "separator") live.onSelect();
                  },
                },
          ),
        }
      : {}),
  };
}
