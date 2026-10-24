/**
 * The widget tray: everything a reader can add to their Home, by where it
 * comes from, and the ready-made boards to start from.
 *
 * THE SIDEBAR'S SECTIONS, IN THE SIDEBAR'S ORDER. After the tray's own layout
 * widgets come the widgets each workspace offers (`registry.tsx`), under the
 * workspace's name and sentence, so a reader looks for a widget where they
 * look for its page. A workspace that offers a widget next year appears here
 * with nothing in this file changing.
 *
 * WHAT THE READER CANNOT OPEN IS NOT OFFERED. A widget whose page their access
 * does not open is left out, and the tray says how many were, and why.
 *
 * A PRESS ADDS, and the tray stays open: a reader building a group adds a
 * heading, a note and three figures in a row without reopening anything.
 * Each added widget lands at the end of the page, pulses once, and scrolls
 * into view; a reader drags it into a group from there.
 */

import { useState, type ReactNode } from "react";
import {
  Button,
  Card,
  EmptyState,
  IconTile,
  Input,
  Modal,
  Tabs,
  Tag,
  type NodeHue,
} from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import { useViewer } from "~/lib/viewer.ts";
import { plural } from "~/lib/format.ts";
import { ENTRIES, LAYOUT_CATEGORIES, TEMPLATES, summary, type LayoutCategory } from "./catalog.tsx";
import { AREAS, WIDGETS, canUse } from "./registry.tsx";
import { widgetDescription, widgetShown } from "./kit.tsx";
import { useServicesRun } from "~/lib/services.ts";
import { contrib, widgetsOn, type Board, type Widget } from "./model.ts";

const TAB_PANEL = "home-tray-panel";

interface TileSpec {
  key: string;
  label: string;
  description: string;
  icon: ReactNode;
  hue?: NodeHue | undefined;
  placed: boolean;
  make: () => Widget;
}

interface TraySection {
  key: string;
  label: string;
  hint: string;
  tiles: TileSpec[];
}

export function WidgetTray({
  board,
  onAdd,
  onTemplate,
  onClose,
}: {
  board: Board;
  onAdd: (widget: Widget) => void;
  onTemplate: (board: Board, name: string) => void;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<"widgets" | "templates">("widgets");
  const [q, setQ] = useState("");
  const viewer = useViewer();
  // A WIDGET WITH NOTHING TO SHOW where the company does not run its service
  // is not offered, and not counted among those the reader lacks access to.
  const runs = useServicesRun();
  const on = widgetsOn(board);
  const query = q.trim().toLowerCase();
  const matches = (...words: string[]) =>
    query === "" || words.join(" ").toLowerCase().includes(query);

  const layout: TraySection[] = (Object.keys(LAYOUT_CATEGORIES) as LayoutCategory[]).map((c) => ({
    key: c,
    ...LAYOUT_CATEGORIES[c],
    tiles: ENTRIES.filter(
      (e) => e.category === c && matches(e.label, e.description, LAYOUT_CATEGORIES[c].label),
    ).map((e) => ({ ...e, placed: false })),
  }));
  const offered: TraySection[] = AREAS.map((area) => ({
    key: area.key,
    label: area.label,
    hint: area.hint,
    tiles: area.widgets
      .filter(
        (d) =>
          widgetShown(d, {}, runs) &&
          canUse(d, viewer.grants) &&
          matches(d.label, widgetDescription(d, runs), area.label),
      )
      .map((d) => ({
        key: d.key,
        label: d.label,
        description: widgetDescription(d, runs),
        icon: <d.icon size="sm" />,
        hue: d.hue,
        placed: d.once === true && on.has(d.key),
        make: () => contrib(d.key),
      }))
      // What can still be added first; what is on the page after.
      .sort((a, b) => Number(a.placed) - Number(b.placed)),
  }));
  const sections = [...layout, ...offered].filter((s) => s.tiles.length > 0);
  const locked = viewer.loading
    ? 0
    : [...WIDGETS.values()].filter((d) => widgetShown(d, {}, runs) && !canUse(d, viewer.grants))
        .length;

  return (
    <Modal
      open
      variant="sheet"
      backdrop="clear"
      stackBody
      title="Add to Home"
      subtitle="Pick widgets to add. The tray stays open."
      onClose={onClose}
      showCloseButton={false}
      headerActions={
        <Button variant="primary" size="small" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div className="home-tray-tabs">
        <Tabs
          ariaLabel="Add to Home"
          panelId={TAB_PANEL}
          value={tab}
          onValueChange={(v) => setTab(v as "widgets" | "templates")}
          items={[
            { value: "widgets", label: "Widgets" },
            { value: "templates", label: "Templates" },
          ]}
        />
      </div>
      <div id={TAB_PANEL} role="tabpanel" className="home-tray">
        {tab === "widgets" ? (
          <>
            <Input
              type="search"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              onClear={() => setQ("")}
              aria-label="Search widgets"
              placeholder="Search widgets"
              leading={<SearchGlyph size="sm" />}
              width="full"
            />
            {sections.length === 0 ? (
              <EmptyState
                size="compact"
                title={`No widgets match “${q.trim()}”`}
                description="Try another word."
              />
            ) : (
              sections.map((section) => (
                <section
                  key={section.key}
                  className="home-tray-group"
                  aria-labelledby={`home-tray-${section.key}`}
                >
                  <div className="home-tray-head">
                    <h3 id={`home-tray-${section.key}`} className="home-tray-title">
                      {section.label}
                    </h3>
                    <p className="home-tray-hint">{section.hint}</p>
                  </div>
                  <ul className="home-tray-grid">
                    {section.tiles.map((tile) => (
                      <li key={tile.key}>
                        <Tile tile={tile} onAdd={() => onAdd(tile.make())} />
                      </li>
                    ))}
                  </ul>
                </section>
              ))
            )}
            {locked > 0 ? (
              <p className="home-tray-hint">
                {`${plural(locked, "widget")} ${locked === 1 ? "needs" : "need"} access you don’t have. Ask an admin for access.`}
              </p>
            ) : null}
          </>
        ) : (
          <ul className="home-tray-templates">
            {TEMPLATES.map((t) => {
              const preview = t.make();
              return (
                <li key={t.id}>
                  <Card padding="md" className="home-tray-template">
                    <div className="home-tray-template-text">
                      <span className="home-tray-tile-name">{t.name}</span>
                      <span className="home-tray-tile-desc">{t.description}</span>
                      <Tag size="xs" variant="neutral">
                        {summary(preview)}
                      </Tag>
                    </div>
                    <Button
                      variant="secondary"
                      size="small"
                      onClick={() => onTemplate(t.make(), t.name)}
                    >
                      Use
                    </Button>
                  </Card>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </Modal>
  );
}

function Tile({ tile, onAdd }: { tile: TileSpec; onAdd: () => void }) {
  const stateId = `${tile.key}-placed`;
  return (
    <Card
      interactive
      padding="sm"
      asChild
      className={tile.placed ? "home-tray-tile is-placed" : "home-tray-tile"}
    >
      <button
        type="button"
        aria-disabled={tile.placed || undefined}
        aria-describedby={tile.placed ? stateId : undefined}
        onClick={() => {
          if (!tile.placed) onAdd();
        }}
      >
        <span className="home-tray-tile-inner">
          <IconTile hue={tile.hue}>{tile.icon}</IconTile>
          <span className="home-tray-tile-text">
            <span className="home-tray-tile-name">{tile.label}</span>
            <span className="home-tray-tile-desc">{tile.description}</span>
            {tile.placed ? (
              <span id={stateId} className="home-tray-tile-state">
                On your Home
              </span>
            ) : null}
          </span>
        </span>
      </button>
    </Card>
  );
}
