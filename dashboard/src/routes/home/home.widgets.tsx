/**
 * Home's own widgets: the cards Home has always drawn, one figure at a time,
 * and a shortcut to any page of the dashboard.
 *
 * THE CARDS SHARE ONE SET OF QUESTIONS. They read the page's one `HomeData`
 * (`useSharedHomeData`), so placing all eight asks what Home always asked and
 * no more.
 *
 * THE SHORTCUT OFFERS EVERY PAGE THE NAVIGATION HAS, so a section that ships
 * without widgets of its own can still be put on Home the day it ships.
 */

import { useMemo, type ComponentType } from "react";
import { Card, IconTile, type NodeHue } from "@crewlethq/ui";
import {
  ChartColumnGlyph,
  ChartPieGlyph,
  ExternalLinkGlyph,
  FolderKanbanGlyph,
  GaugeGlyph,
  HashGlyph,
  MessageCircleQuestionMarkGlyph,
  RadioGlyph,
  RotateCcwClockGlyph,
  StarGlyph,
  type GlyphProps,
} from "@crewlethq/icons/glyphs";
import { DESTINATIONS } from "~/app/nav.ts";
import { href } from "~/app/router.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { Mark } from "~/ui/glyph.tsx";
import { useSharedHomeData } from "./useHomeData.ts";
import { Figures, SectionView, type SectionId } from "./sections.tsx";
import { KPI_IDS, KPI_LABELS, type KpiId } from "./Kpis.tsx";
import {
  ChoiceNote,
  defineWidgets,
  destinationOf,
  pageName,
  pageOpens,
  type WidgetDef,
  type WidgetSize,
} from "./board/kit.tsx";

interface HomeCard {
  label: string;
  description: string;
  icon: ComponentType<GlyphProps>;
  hue: NodeHue;
  size: WidgetSize;
}

const CARDS: Record<SectionId, HomeCard> = {
  pinned: {
    label: "Pinned",
    description: "Everything you starred",
    icon: StarGlyph,
    hue: "amber",
    size: { w: 12, minW: 4, minH: 6, vertical: true },
  },
  kpis: {
    label: "Figures",
    description: "Agents, decisions, tasks and tokens at a glance",
    icon: GaugeGlyph,
    hue: "blue",
    size: { w: 12, minW: 6, minH: 6, vertical: false },
  },
  decisions: {
    label: "Needs your decision",
    description: "Questions waiting on you, answered in place",
    icon: MessageCircleQuestionMarkGlyph,
    hue: "rose",
    size: { w: 8, minW: 4, minH: 8, vertical: true },
  },
  live: {
    label: "Live now",
    description: "Who is working now, and on what",
    icon: RadioGlyph,
    hue: "green",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
  },
  completed: {
    label: "Tasks completed",
    description: "Tasks finished each day",
    icon: ChartColumnGlyph,
    hue: "cyan",
    size: { w: 4, minW: 3, minH: 10, vertical: true },
  },
  tokens: {
    label: "Tokens by team",
    description: "Token spend by team",
    icon: ChartPieGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
  },
  projects: {
    label: "Projects",
    description: "The busiest projects and their progress",
    icon: FolderKanbanGlyph,
    hue: "blue",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
  },
  feed: {
    label: "Recent activity",
    description: "Work finished, handed off and published",
    icon: RotateCcwClockGlyph,
    hue: "cyan",
    size: { w: 12, minW: 4, minH: 10, vertical: true },
  },
};

const card = ([id, def]: [SectionId, HomeCard]): WidgetDef => ({
  key: `home.${id}`,
  from: "home",
  ...def,
  once: true,
  Component: function CardWidget() {
    return <SectionView id={id} data={useSharedHomeData()} />;
  },
});

const isKpi = (v: string | undefined): v is KpiId => KPI_IDS.includes(v as KpiId);

function OneFigure({ params }: { params: Readonly<Record<string, string>> }) {
  const data = useSharedHomeData();
  if (!isKpi(params.figure)) {
    return (
      <Card padding="none" className="home-card">
        <ChoiceNote thing="figure" />
      </Card>
    );
  }
  return <Figures data={data} figures={[params.figure]} />;
}

/** Every page the reader may open, by the navigation, Home's own aside. */
function usePages() {
  const viewer = useViewer();
  return useMemo(
    () => DESTINATIONS.filter((d) => d.workspace !== "home" && pageOpens(d, viewer.grants)),
    [viewer.grants],
  );
}

function Shortcut({ params }: { params: Readonly<Record<string, string>> }) {
  const dest = params.page ? destinationOf(params.page) : undefined;
  const opens = usePages().some((d) => d.key === dest?.key);
  if (!dest || !opens) {
    return (
      <Card padding="none" className="home-card">
        <ChoiceNote thing="page" />
      </Card>
    );
  }
  return (
    <Card padding="md" interactive asChild>
      <a className="home-shortcut" href={href(dest.path)}>
        <IconTile hue="blue">
          <Mark name={dest.icon} size="sm" />
        </IconTile>
        <span className="home-shortcut-text">
          <span className="home-shortcut-name">{pageName(dest)}</span>
          <span className="home-shortcut-hint">{dest.hint}</span>
        </span>
      </a>
    </Card>
  );
}

export const widgets = defineWidgets([
  ...(Object.entries(CARDS) as [SectionId, HomeCard][]).map(card),
  {
    key: "home.figure",
    from: "home",
    label: "Figure",
    description: "One headline number of your choice",
    icon: HashGlyph,
    hue: "blue",
    size: { w: 3, minW: 2, minH: 6, vertical: false },
    params: [
      {
        key: "figure",
        label: "Figure",
        useChoices: () => ({
          loading: false,
          options: KPI_IDS.map((f) => ({ value: f, label: KPI_LABELS[f] })),
        }),
      },
    ],
    nameOf: (p) => (isKpi(p.figure) ? KPI_LABELS[p.figure] : undefined),
    Component: OneFigure,
  },
  {
    key: "home.shortcut",
    from: "home",
    label: "Page shortcut",
    description: "A card that opens any dashboard page",
    icon: ExternalLinkGlyph,
    hue: "blue",
    size: { w: 4, minW: 2, minH: 4, vertical: false },
    params: [
      {
        key: "page",
        label: "Page",
        helper: "Any page you can open.",
        useChoices: function usePageChoices() {
          const pages = usePages();
          return {
            loading: false,
            options: pages.map((d) => ({ value: d.key, label: pageName(d) })),
          };
        },
      },
    ],
    nameOf: (p) => {
      const dest = p.page ? destinationOf(p.page) : undefined;
      return dest ? `Shortcut to ${dest.label}` : undefined;
    },
    Component: Shortcut,
  },
]);
