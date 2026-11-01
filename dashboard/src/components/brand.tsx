/**
 * The company's brand where people make and read artifacts: the chip on the
 * gallery's Make row, the note under Ask an agent, and the Brand fact in an
 * artifact's details.
 *
 * Each names the ADOPTED brand (`brand{}`) and leads to Company › Brand, where
 * it is set, so the brand every new artifact follows is named where the
 * artifact is started, one press from where it is chosen. Nothing here is
 * drawn before the brand read answers, and nothing claims a brand where none
 * is adopted.
 */

import type { ReactNode } from "react";
import { BrandChip, BrandScope, Tag } from "@crewlethq/ui";
import { BadgeCheckGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest } from "~/lib/useRest.ts";
import { readDeck } from "~/lib/artifacts/api.ts";
import { useAdoptedBrand, useAdoptedMark, useBrand } from "~/lib/brand/useBrand.ts";
import { faceDrawing, mayAdoptBrand } from "~/lib/brand/brand.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { fmtDate } from "~/lib/format.ts";

/** Where the company's brand is shown and set. */
export const BRAND_PAGE: readonly string[] = ["company", "brand"];

/** Where the company's brand is set up, step by step. */
export const BRAND_SETUP: readonly string[] = [...BRAND_PAGE, "setup"];

/**
 * The Make row's brand chip: the adopted brand's mark and name, leading to
 * Company › Brand, or, for an administrator while none is adopted, the way
 * to set one up (the setup page itself). Nothing for anyone else while none
 * is.
 */
export function BrandMakeChip() {
  const viewer = useViewer();
  const { adoption, mark } = useAdoptedMark();
  if (adoption === undefined) return null;
  if (adoption === null) {
    return mayAdoptBrand(viewer) ? (
      <BrandChip href={href(BRAND_SETUP)} label="Set up your brand" />
    ) : null;
  }
  return (
    <BrandChip
      href={href(BRAND_PAGE)}
      label={`${adoption.document.name} brand`}
      mark={mark ?? undefined}
    />
  );
}

/** The note under Ask an agent: the brand the agent will follow, where one is adopted. */
export function BrandAgentNote() {
  const { adoption, mark } = useAdoptedMark();
  if (!adoption) return null;
  return (
    <BrandChip
      href={href(BRAND_PAGE)}
      label={`The agent follows the ${adoption.document.name} brand`}
      mark={mark ?? undefined}
    />
  );
}

/** How a kind takes the brand: drawn live, pinned per published version, or copied when made. */
const BRAND_TAKES: Readonly<Record<ArtifactKind, "live" | "pinned" | "copied" | "none">> = {
  doc: "live",
  dashboard: "live",
  html: "pinned",
  design: "pinned",
  slides: "copied",
  sheet: "none",
  brand: "none",
};

/**
 * Whether an artifact's shown version follows the company's brand, in the
 * Details panel's words: the current brand, an older one, or Crewlet's look.
 *
 * - A doc or a dashboard is drawn in the adopted brand, live.
 * - An HTML page or a design keeps the brand it was published with (the
 *   stylesheet's digest its version records) until it is next published.
 * - A deck copies the brand when it is made and never follows it: "Made
 *   with the Acme brand of 24 Oct 2026", the day it was made, since a deck
 *   is never the current brand nor an older one to update.
 * - A sheet, and a brand kit itself, carry no fact.
 */
export function BrandFact({
  id,
  kind,
  version,
  made,
}: {
  id: string;
  kind: ArtifactKind;
  /** The version on show. */
  version: number;
  /** When the artifact was made: the day a deck copied the brand. */
  made: string;
}) {
  const takes = BRAND_TAKES[kind];
  const { adoption } = useBrand({ enabled: takes !== "none" });
  const summary = useQuery(
    "artifact_version",
    { id, n: version },
    { enabled: takes === "pinned" && version > 0 },
  );
  const deck = useRest(
    takes === "copied" && version > 0 ? `brand-fact-deck:${id}:${version}` : null,
    (signal) => readDeck(id, version, signal),
  );
  if (takes === "none" || adoption === undefined) return null;
  if (takes === "pinned" && !summary.data) return null;
  if (takes === "copied" && !deck.data) return null;
  const versionBrand = summary.data?.brand;
  const deckTheme = deck.data?.deck.theme;
  const name = adoption?.document.name ?? "";
  const current = (
    <Tag size="sm" variant="success" leadingIcon={<BadgeCheckGlyph size="xs" />}>
      {`${name} brand, current`}
    </Tag>
  );
  const crewlet = <span>Crewlet's look</span>;
  let shown;
  switch (takes) {
    case "live":
      shown = adoption ? current : crewlet;
      break;
    case "pinned":
      shown = !versionBrand ? (
        crewlet
      ) : adoption && versionBrand === adoption.css_sha ? (
        current
      ) : (
        <>
          <Tag size="sm" variant="warning" leadingIcon={<TriangleAlertGlyph size="xs" />}>
            {adoption ? `An older ${name} brand` : "A brand no longer in use"}
          </Tag>{" "}
          <span className="muted">changes at the next publish</span>
        </>
      );
      break;
    case "copied":
      shown =
        deckTheme === BRAND_DECK_THEME ? (
          <span>{`Made with the ${name ? `${name} brand` : "company brand"} of ${fmtDate(made)}`}</span>
        ) : (
          crewlet
        );
      break;
  }
  return (
    <>
      <dt>Brand</dt>
      <dd>{shown}</dd>
    </>
  );
}

/**
 * An artifact's body drawn in the company's adopted brand, LIVE: a doc's
 * document and a dashboard's board follow the brand an administrator adopts
 * the moment it is adopted (the kinds the dashboard draws itself, FINAL B4).
 *
 * THE BODY, NEVER THE CHROME: the brand's colours and faces restyle the
 * tokens inside this region only (`BrandScope`), so the bar, the panels and
 * every control around the body stay the dashboard's own.
 *
 * ALWAYS THE SAME TREE: with no brand adopted, or none read yet, the region
 * is drawn with no input, which keeps every token the page's own, so the
 * brand arriving restyles the body rather than mounting it again (a board
 * mounted again would read its datasets again).
 */
export function AdoptedBrandScope({
  children,
  className,
}: {
  children: ReactNode;
  className?: string | undefined;
}) {
  const { adoption, assets } = useAdoptedBrand({ logos: false });
  const doc = adoption?.document;
  const heading = doc ? faceDrawing(doc.type.heading, assets.face("heading")).stack : "";
  const body = doc ? faceDrawing(doc.type.body, assets.face("body")).stack : "";
  return (
    <BrandScope
      colors={doc?.colors ?? {}}
      headingFamily={heading || undefined}
      bodyFamily={body || undefined}
      className={className}
    >
      {children}
    </BrandScope>
  );
}

/** The slide theme a deck made in the company's brand names (`slideformat` starters' `brand`). */
export const BRAND_DECK_THEME = "brand";
