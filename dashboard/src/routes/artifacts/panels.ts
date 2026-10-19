/**
 * An open artifact's panels, in its address.
 *
 * `panel=` names the one open at the artifact's side, so a link opens on it
 * and Back closes it: opening one is a SECTION (it pushes), because the reader
 * called it. The keys that belong to a panel (`expand=`, and History's
 * `against=` and `lens=`) leave with it, so closing History does not leave a
 * comparison in the address for the next time it opens.
 *
 * THE ADDRESSES THE TABS HAD STILL OPEN. The page drew View, Data, History
 * and Settings as tabs (`tab=`) and the people as a rail (`rail=people`), and
 * links to them are in chat and in agents' notes. Each is read as the panel
 * it became and rewritten in place, replacing the entry, so Back does not
 * land on the old spelling.
 */

import type { ArtifactPanelSize } from "@crewlethq/ui";

/** Every panel, in the bar's order. */
export const PANELS = ["details", "history", "data", "people", "settings"] as const;
export type PanelName = (typeof PANELS)[number];

/**
 * What a panel is called in its head, and how wide it rests: every panel of
 * text at one width, so moving between them never moves the artifact's edge,
 * and Data at the table's.
 */
export const PANEL_SPECS: Readonly<Record<PanelName, { title: string; size: ArtifactPanelSize }>> =
  {
    details: { title: "Details", size: "md" },
    history: { title: "Versions and history", size: "md" },
    data: { title: "Data", size: "lg" },
    people: { title: "People here", size: "md" },
    settings: { title: "Settings", size: "md" },
  };

/**
 * A panel's name for this artifact: a sheet keeps no versions a reader sees
 * change (its content is its live data), so its History promises none.
 */
export function panelTitle(panel: PanelName, versioned: boolean): string {
  return panel === "history" && !versioned ? "History" : PANEL_SPECS[panel].title;
}

/** The keys a panel owns besides `panel=` itself. */
const PANEL_KEYS = ["expand", "against", "lens"] as const;

/** The keys the tabs and the rail were addressed by. */
const LEGACY_KEYS = ["tab", "rail"] as const;

function isPanel(value: string | null): value is PanelName {
  return value !== null && (PANELS as readonly string[]).includes(value);
}

/**
 * The panel an address opens, if this artifact has it: `panel=`, else the tab
 * or the rail an older link named. Data only where the artifact keeps data.
 */
export function panelOf(query: URLSearchParams, hasData: boolean): PanelName | null {
  const named = query.get("panel");
  const asked = isPanel(named) ? named : legacyPanel(query);
  if (asked === "data" && !hasData) return null;
  return asked;
}

/** The panel an older link's `tab=` or `rail=people` meant; View meant none. */
function legacyPanel(query: URLSearchParams): PanelName | null {
  if (query.get("rail") === "people") return "people";
  const tab = query.get("tab");
  return tab === "data" || tab === "history" || tab === "settings" ? tab : null;
}

/** Whether the address still spells a panel the old way, and needs rewriting. */
export function spelledTheOldWay(query: URLSearchParams): boolean {
  return LEGACY_KEYS.some((k) => query.has(k));
}

/**
 * The address with `next` open (or none, for `null`): the keys of the panel
 * being left dropped, and any old spelling with them.
 */
export function withPanel(query: URLSearchParams, next: PanelName | null): URLSearchParams {
  const out = new URLSearchParams(query);
  const leaving = out.get("panel") !== next;
  if (leaving) for (const k of PANEL_KEYS) out.delete(k);
  for (const k of LEGACY_KEYS) out.delete(k);
  if (next) out.set("panel", next);
  else out.delete("panel");
  return out;
}
