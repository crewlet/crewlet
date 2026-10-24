/**
 * A brand as four cards: Logos, Colours, Typography and Voice, each with its
 * glyph and title, drawn the same on Company › Brand (the adopted brand) and on
 * a brand kit's own page (one version of the kit).
 *
 * # The card draws; the page decides
 *
 * Each card is handed what to show and, where the reader may change it, its
 * head's control, the editor in its body's place and the footer that saves.
 * The grammar is Company › Principles' (a glyph and a title per card, per-card
 * Edit with Save and Cancel), so the two pages read as siblings. A reader who
 * may not change the brand is handed no control at all, never a disabled one.
 */

import type { ReactNode } from "react";
import {
  Button,
  Card,
  ColorSwatch,
  ContrastBadge,
  List,
  ListItem,
  LogoSlot,
  Text,
  TypeSpecimen,
  downloadBlob,
  useToast,
} from "@crewlethq/ui";
import {
  ArrowDownToLineGlyph,
  BlendGlyph,
  ImageGlyph,
  TextQuoteGlyph,
  TypeGlyph,
} from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  BRAND_COLOR_ROLES,
  BRAND_FACE_ROLES,
  BRAND_LOGO_SLOTS,
  brandContrast,
  faceDrawing,
  isBrandColor,
  type BrandContrast,
} from "~/lib/brand/brand.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";

/** The cards, in the order the page draws them. */
export type BrandCardKey = "logos" | "colours" | "type" | "voice";

/** Each card's title, glyph and what it says under its title. */
export const BRAND_CARDS: Readonly<
  Record<BrandCardKey, { title: string; icon: ReactNode; subtitle?: string }>
> = {
  logos: { title: "Logos", icon: <ImageGlyph size="sm" /> },
  colours: { title: "Colours", icon: <BlendGlyph size="sm" /> },
  type: { title: "Typography", icon: <TypeGlyph size="sm" /> },
  voice: {
    title: "Voice and tone",
    icon: <TextQuoteGlyph size="sm" />,
    subtitle:
      "Agents read this when they write an artifact. To shape every message, add a standing policy in Principles.",
  },
};

/** How one card is changed, as the page hands it: its control, its editor and its footer. */
export interface BrandCardEdit {
  /** The head's own control (Edit), or nothing. */
  actions?: ReactNode;
  /** The card is being changed: its body is this editor. */
  editor?: ReactNode;
  /** The editor's footer: Cancel and Save. */
  footer?: ReactNode;
}

/** One card: its glyph, title and subtitle, its body or editor, and its footer while it is changed. */
export function BrandCard({
  card,
  edit,
  children,
}: {
  card: BrandCardKey;
  edit?: BrandCardEdit | undefined;
  children: ReactNode;
}) {
  const spec = BRAND_CARDS[card];
  const editing = edit?.editor != null;
  return (
    <Card as="section" data-card={card} data-editing={editing ? "" : undefined}>
      <Card.Header
        icon={spec.icon}
        actions={edit?.actions}
        subtitle={spec.subtitle}
        subtitlePlacement="below"
      >
        <Card.Title>{spec.title}</Card.Title>
      </Card.Header>
      <Card.Body>{editing ? edit.editor : children}</Card.Body>
      {editing && edit.footer ? <Card.Footer variant="actions">{edit.footer}</Card.Footer> : null}
    </Card>
  );
}

/**
 * A file's name as a download offers it: the brand's name and the slot, in
 * lowercase words. `downloadBlob` makes it a safe name (`safeFilename`), which
 * keeps the letters of every script, so a brand named in any language keeps
 * its name.
 */
function downloadName(brand: string, slot: string, type: string | undefined): string {
  const ext = type === "image/svg+xml" ? "svg" : type === "image/webp" ? "webp" : "png";
  return `${brand} ${slot} logo`.toLowerCase() + `.${ext}`;
}

/** The logo types a download is named by, from the path the document names it at. */
function typeOfPath(path: string | undefined): string | undefined {
  const ext = path?.split(".").pop()?.toLowerCase();
  return ext === "svg"
    ? "image/svg+xml"
    : ext === "webp"
      ? "image/webp"
      : ext
        ? "image/png"
        : undefined;
}

/** The Logos card's body: the brand's name and tagline, and its three logos on their own grounds. */
export function LogosBody({ document, assets }: { document: BrandDocument; assets: BrandAssets }) {
  const toast = useToast();
  return (
    <div className="brand-logos">
      <Text as="p" variant="body" className="brand-name">
        <strong>{document.name}</strong>
        {document.tagline ? <span className="muted">{` · ${document.tagline}`}</span> : null}
      </Text>
      <div className="brand-logo-slots">
        {BRAND_LOGO_SLOTS.map((spec) => {
          const src = assets.logo(spec.slot);
          const file = assets.logoFile(spec.slot);
          const type = typeOfPath(document.logos[spec.slot]);
          // SAVED AT THE PRESS from the bytes the page holds, never a lasting
          // link: an `<a href="blob:…">` to an SVG on this origin is one a
          // reader could open as a page here.
          const save = () => {
            if (!file) return;
            const { started } = downloadBlob(file, downloadName(document.name, spec.label, type));
            if (!started)
              toast.show({ variant: "warning", title: "The browser did not take the download" });
          };
          return (
            <LogoSlot
              key={spec.slot}
              label={spec.label}
              description={spec.description}
              ground={spec.ground}
              groundColor={
                spec.ground === "dark" ? document.colors.dark_ground : document.colors.light_ground
              }
              shape={spec.shape}
              src={src}
              alt={`${document.name} ${spec.label.toLowerCase()} logo`}
              emptyNote={spec.emptyNote}
              download={
                file ? (
                  <Button
                    size="small"
                    variant="ghost"
                    leadingIcon={<ArrowDownToLineGlyph size="sm" />}
                    aria-label={`Download the ${spec.label.toLowerCase()} logo`}
                    onClick={save}
                  >
                    {type === "image/svg+xml" ? "SVG" : type === "image/webp" ? "WebP" : "PNG"}
                  </Button>
                ) : undefined
              }
            />
          );
        })}
      </div>
    </div>
  );
}

/** A row of the pairs an adoption measures, each a badge with its ratio and level. */
export function ContrastRow({ results }: { results: readonly BrandContrast[] }) {
  return (
    <ul className="brand-contrast" aria-label="Contrast">
      {results.map((r) => (
        <li key={`${r.foreground}/${r.ground}`}>
          <ContrastBadge
            foreground={r.foregroundColor}
            background={r.groundColor}
            label={r.label}
            scale={r.scale}
          />
        </li>
      ))}
    </ul>
  );
}

/** The Colours card's body: each role the brand sets, with its value to copy, and how its pairs read. */
export function ColoursBody({ colors }: { colors: BrandDocument["colors"] }) {
  const set = BRAND_COLOR_ROLES.filter((spec) => isBrandColor(colors[spec.role]));
  return (
    <div className="brand-colours">
      <div className="brand-swatches">
        {set.map((spec) => (
          <ColorSwatch
            key={spec.role}
            name={spec.label}
            value={colors[spec.role]}
            description={spec.description}
          />
        ))}
      </div>
      <ContrastRow results={brandContrast(colors)} />
    </div>
  );
}

/**
 * The line each face is read in: short enough for one line in a specimen half
 * the card wide, where the kit's longer default was cut with an ellipsis even
 * on a wide page.
 */
const SPECIMEN_SAMPLE = "Every page, in your own type.";

/** The Typography card's body: the heading face and the body face, each drawn in itself. */
export function TypographyBody({
  type,
  assets,
}: {
  type: BrandDocument["type"];
  assets: BrandAssets;
}) {
  return (
    <div className="brand-faces">
      {BRAND_FACE_ROLES.map(({ role, label }) => {
        const face = faceDrawing(type[role], assets.face(role));
        return (
          <TypeSpecimen
            key={role}
            label={label}
            family={face.name}
            fontFamily={face.stack || undefined}
            detail={face.detail}
            sample={SPECIMEN_SAMPLE}
            weight={role === "heading" ? "semibold" : "regular"}
          />
        );
      })}
    </div>
  );
}

/** The Voice card's body: the summary, and what to write like and what to avoid. */
export function VoiceBody({ voice }: { voice: BrandDocument["voice"] }) {
  const like = voice.write_like ?? [];
  const avoid = voice.avoid ?? [];
  if (!voice.summary && like.length === 0 && avoid.length === 0) {
    return (
      <Text as="p" variant="body" className="muted">
        No voice yet. Agents write in their own plain words until one is added.
      </Text>
    );
  }
  return (
    <div className="brand-voice">
      {voice.summary ? (
        <Text as="p" variant="body">
          {voice.summary}
        </Text>
      ) : null}
      <div className="brand-voice-lists">
        {like.length > 0 ? <VoiceList title="Write like this" lines={like} /> : null}
        {avoid.length > 0 ? <VoiceList title="Avoid" lines={avoid} /> : null}
      </div>
    </div>
  );
}

function VoiceList({ title, lines }: { title: string; lines: readonly string[] }) {
  return (
    <div className="brand-voice-list">
      <Text as="h4" variant="label">
        {title}
      </Text>
      <List variant="bulleted" aria-label={title}>
        {lines.map((line, i) => (
          <ListItem key={i}>{line}</ListItem>
        ))}
      </List>
    </div>
  );
}
