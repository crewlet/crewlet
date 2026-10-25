/**
 * The company's brand as the dashboard reads, draws and writes it: the roles
 * of a kit's `brand.json` (`brandformat.Brand`), the contrast every adoption is
 * held to, the faces it names, the document a save publishes, and what changed
 * between two versions of it.
 *
 * # One brand, adopted
 *
 * A brand KIT is an artifact of kind `brand`: a `brand.json` and the logo and
 * font files it names, versioned like any artifact, which agents and people
 * may draft. The company's brand is the one version an administrator ADOPTED
 * (`GET /brand`), snapshotted by the engine and served by its bytes. Every
 * screen that shows "the brand" reads that adoption; a kit's own page shows the
 * kit's version.
 *
 * # The contrast is the engine's, measured the same way here
 *
 * An adoption is refused when the text does not read on the light ground, and
 * every other pair is listed as a warning ([BRAND_CONTRAST_PAIRS]). The edit
 * cards show the same pairs while a colour is being chosen, so a Save the
 * engine would refuse is held here with the engine's own reason. The pairs,
 * the fallbacks an unset role takes and the ratio's maths are the contract's
 * and `@crewlethq/tokens`' own, each held to the engine's by a gate, so the
 * badge an administrator reads and the refusal the engine writes never
 * disagree.
 */

import { contrastRatio, formatContrastRatio } from "@crewlethq/ui";
import {
  BRAND_COLOR_FALLBACKS,
  BRAND_CONTRAST_PAIRS,
  BRAND_FACE_FAMILIES,
  BRAND_KIT_FACES,
  BRAND_SYSTEM_STACKS,
} from "~/contract/brand.ts";
import type { BrandAnswer, BrandDocument, BrandFace, BrandFileRef } from "~/protocol/index.ts";
import { plural } from "../format.ts";

/** The adopted brand, as `GET /brand` answers it when one is adopted. */
export type Adoption = NonNullable<BrandAnswer["brand"]>;

/** A colour role of `brand.json`. */
export type BrandColorRole = keyof BrandDocument["colors"];

/** A logo of `brand.json`. */
export type BrandLogoSlot = keyof BrandDocument["logos"];

/** A face of `brand.json`. */
export type BrandFaceRole = keyof BrandDocument["type"];

/** What a colour role is called and what it colours, in the order the Colours card lists them. */
export interface BrandColorSpec {
  role: BrandColorRole;
  label: string;
  description: string;
  required: boolean;
}

export const BRAND_COLOR_ROLES: readonly BrandColorSpec[] = [
  {
    role: "primary",
    label: "Primary",
    description: "Links, buttons and accents",
    required: true,
  },
  {
    role: "primary_on_dark",
    label: "Primary on dark",
    description: "The accent on a dark ground, where the primary is too dark to read",
    required: false,
  },
  {
    role: "secondary",
    label: "Secondary",
    description: "Highlights and the second stop of a gradient",
    required: false,
  },
  {
    role: "text",
    label: "Text",
    description: "Body text on the light ground",
    required: false,
  },
  {
    role: "light_ground",
    label: "Light ground",
    description: "The page in light mode",
    required: false,
  },
  {
    role: "dark_ground",
    label: "Dark ground",
    description: "The page in dark mode",
    required: false,
  },
];

/** What a logo slot is called, the file slot the adoption serves it under, and the ground it is drawn on. */
export interface BrandLogoSpec {
  slot: BrandLogoSlot;
  /** The adoption's file slot (`brandformat.SlotLogo*`). */
  fileSlot: string;
  label: string;
  /** What adopting calls a change to it, naming it as a logo: "Primary logo". */
  changeLabel: string;
  description: string;
  ground: "light" | "dark";
  shape: "wide" | "square";
  /** Said under the slot while it is empty, for a reader who cannot fill it. */
  emptyNote: string;
  /** Where the logo appears, as the setup says it. */
  purpose: string;
  /** Whether an adoption needs it (the primary), or it is only recommended. */
  required: boolean;
}

export const BRAND_LOGO_SLOTS: readonly BrandLogoSpec[] = [
  {
    slot: "primary",
    fileSlot: "logo.primary",
    label: "Primary",
    changeLabel: "Primary logo",
    description: "On light grounds",
    ground: "light",
    shape: "wide",
    emptyNote: "No logo yet.",
    purpose: "Doc headers, light slides and pages",
    required: true,
  },
  {
    slot: "on_dark",
    fileSlot: "logo.on_dark",
    label: "For dark grounds",
    changeLabel: "Logo for dark grounds",
    description: "On dark grounds and dark mode",
    ground: "dark",
    shape: "wide",
    emptyNote:
      "Add a version for dark grounds. Until then, dark slides show the primary logo on a light plate.",
    purpose: "Dark slides and dark mode",
    required: false,
  },
  {
    slot: "mark",
    fileSlot: "logo.mark",
    label: "Mark",
    changeLabel: "Mark",
    description: "For chips, slide corners and favicons",
    ground: "light",
    shape: "square",
    emptyNote: "Nothing is drawn where a mark would go until one is added.",
    purpose: "Chips, slide corners and the brand chip",
    required: false,
  },
];

/** The logo slot an adoption's file slot (`logo.on_dark`) serves, or undefined for any other file. */
export function logoSlotOf(fileSlot: string): BrandLogoSlot | undefined {
  return BRAND_LOGO_SLOTS.find((s) => s.fileSlot === fileSlot)?.slot;
}

/** What each face is for, in the order the Typography card shows them. */
export const BRAND_FACE_ROLES: readonly { role: BrandFaceRole; label: string; fileSlot: string }[] =
  [
    { role: "heading", label: "Headings", fileSlot: "font.heading" },
    { role: "body", label: "Body", fileSlot: "font.body" },
  ];

/** Whether a value is a colour as `brand.json` admits it: `#` and six lowercase hex digits. */
export function isBrandColor(value: unknown): value is string {
  return typeof value === "string" && /^#[0-9a-f]{6}$/.test(value);
}

// ---------------------------------------------------------------------------
// Contrast
// ---------------------------------------------------------------------------

const WHITE = "#ffffff";
const BLACK = "#000000";

/**
 * Every role and pseudo-role a contrast pair names, as the engine fills it:
 * an unset optional role takes its Crewlet fallback, the accent on a dark
 * ground falls back to the primary, the label on the primary is whichever of
 * white and black reads better on it (what the kit's label on an accent
 * picks), and dark mode keeps the theme's own text.
 */
export function effectiveColors(colors: BrandDocument["colors"]): Readonly<Record<string, string>> {
  const primary = isBrandColor(colors.primary) ? colors.primary : "";
  const onPrimary =
    primary === ""
      ? ""
      : (contrastRatio(WHITE, primary) ?? 0) >= (contrastRatio(BLACK, primary) ?? 0)
        ? WHITE
        : BLACK;
  return {
    primary,
    primary_on_dark: isBrandColor(colors.primary_on_dark) ? colors.primary_on_dark : primary,
    secondary: isBrandColor(colors.secondary) ? colors.secondary : "",
    text: isBrandColor(colors.text) ? colors.text : BRAND_COLOR_FALLBACKS.text,
    light_ground: isBrandColor(colors.light_ground)
      ? colors.light_ground
      : BRAND_COLOR_FALLBACKS.light_ground,
    dark_ground: isBrandColor(colors.dark_ground)
      ? colors.dark_ground
      : BRAND_COLOR_FALLBACKS.dark_ground,
    dark_text: BRAND_COLOR_FALLBACKS.dark_text,
    on_primary: onPrimary,
  };
}

/** One contrast pair, measured on a brand's effective colours. */
export interface BrandContrast {
  /** The pair's roles, as the engine names them. */
  foreground: string;
  ground: string;
  /** The pair in words: "Text on light ground". */
  label: string;
  foregroundColor: string;
  groundColor: string;
  /** The measured ratio, unrounded; null while a colour it needs is not chosen. */
  ratio: number | null;
  /** The least it should reach. */
  min: number;
  /** Below `min`, the engine refuses the adoption rather than warning. */
  refuses: boolean;
  /** It reaches `min`, compared unrounded. */
  passes: boolean;
  /** How the badge reads it: body text at 4.5:1, large text and marks at 3:1. */
  scale: "body" | "large";
}

/** The words a pair is said in, by its roles. */
const PAIR_WORDS: Readonly<Record<string, string>> = {
  "text/light_ground": "Text on light ground",
  "dark_text/dark_ground": "Text on dark ground",
  "primary/light_ground": "Primary on light ground",
  "primary_on_dark/dark_ground": "Primary on dark ground",
};

/** A role's name in a pair the table above does not word. */
const ROLE_WORDS: Readonly<Record<string, string>> = {
  primary: "Primary",
  primary_on_dark: "Primary on dark",
  secondary: "Secondary",
  text: "Text",
  dark_text: "Text",
  light_ground: "light ground",
  dark_ground: "dark ground",
};

function pairLabel(foreground: string, ground: string, colors: Readonly<Record<string, string>>) {
  if (foreground === "on_primary") {
    return `${colors.on_primary === BLACK ? "Black" : "White"} on primary`;
  }
  return (
    PAIR_WORDS[`${foreground}/${ground}`] ??
    `${ROLE_WORDS[foreground] ?? foreground} on ${(ROLE_WORDS[ground] ?? ground).toLowerCase()}`
  );
}

/** Every pair an adoption measures, in the engine's order, on these colours. */
export function brandContrast(colors: BrandDocument["colors"]): BrandContrast[] {
  const effective = effectiveColors(colors);
  return BRAND_CONTRAST_PAIRS.map((pair) => {
    const foregroundColor = effective[pair.foreground] ?? "";
    const groundColor = effective[pair.ground] ?? "";
    const ratio = contrastRatio(foregroundColor, groundColor);
    return {
      foreground: pair.foreground,
      ground: pair.ground,
      label: pairLabel(pair.foreground, pair.ground, effective),
      foregroundColor,
      groundColor,
      ratio,
      min: pair.min,
      refuses: pair.refuses,
      passes: ratio !== null && ratio >= pair.min,
      scale: pair.min >= 4.5 ? "body" : "large",
    };
  });
}

/** The pair that would refuse an adoption of these colours, if one does. */
export function refusingPair(results: readonly BrandContrast[]): BrandContrast | undefined {
  return results.find((r) => r.refuses && r.ratio !== null && !r.passes);
}

/** The pairs an adoption of these colours would be warned about. */
export function warningPairs(results: readonly BrandContrast[]): BrandContrast[] {
  return results.filter((r) => !r.refuses && r.ratio !== null && !r.passes);
}

/** A ratio as the dialogs print it, the badge's way: cut down, never rounded up past a floor it misses. */
export function ratioWords(ratio: number): string {
  return `${formatContrastRatio(ratio)}:1`;
}

/**
 * Why a pair refuses an adoption, and what to do: the sentence under the
 * Colours card and in the adopt dialog.
 */
export function refusalWords(pair: BrandContrast): string {
  const measured = pair.ratio === null ? "" : ` It measures ${ratioWords(pair.ratio)}.`;
  if (pair.foreground === "text" && pair.ground === "light_ground") {
    return `Text on the light ground must reach ${pair.min}:1 so every branded doc stays readable.${measured} Try a darker text colour or a lighter ground.`;
  }
  return `${pair.label} must reach ${pair.min}:1.${measured}`;
}

/** A warning pair in the adopt dialog's words: "Primary on light ground 2.8:1, below 3:1". */
export function warningWords(pair: BrandContrast): string {
  return pair.ratio === null
    ? pair.label
    : `${pair.label} ${ratioWords(pair.ratio)}, below ${pair.min}:1`;
}

// ---------------------------------------------------------------------------
// Faces
// ---------------------------------------------------------------------------

/** What each kit face and system stack is called where a person chooses one. */
export const FACE_NAMES: Readonly<
  Record<(typeof BRAND_KIT_FACES)[number] | (typeof BRAND_SYSTEM_STACKS)[number], string>
> = {
  geist: "Geist",
  "geist-mono": "Geist Mono",
  "system-sans": "System sans serif",
  "system-serif": "System serif",
  "system-mono": "System monospace",
};

/** How a face is drawn and described: its name, its CSS family list, and one fact about it. */
export interface FaceDrawing {
  name: string;
  /** The family list to draw it in. */
  stack: string;
  detail: string;
}

/**
 * A face as the Typography card shows it. An uploaded family is drawn under
 * the name its bytes were registered by in this page (`registered`), never by
 * the name the kit gives it, so a family the page has not loaded falls back to
 * the interface's type rather than to a face of the same name the reader's
 * system happens to hold.
 */
export function faceDrawing(face: BrandFace, registered?: string): FaceDrawing {
  if (face.kit && face.kit in BRAND_FACE_FAMILIES) {
    const kit = face.kit as keyof typeof FACE_NAMES;
    return {
      name: FACE_NAMES[kit],
      stack: BRAND_FACE_FAMILIES[kit as keyof typeof BRAND_FACE_FAMILIES],
      detail: "The kit's own face",
    };
  }
  if (face.stack && face.stack in BRAND_FACE_FAMILIES) {
    const stack = face.stack as keyof typeof FACE_NAMES;
    return {
      name: FACE_NAMES[stack],
      stack: BRAND_FACE_FAMILIES[stack as keyof typeof BRAND_FACE_FAMILIES],
      detail: "The reader's own system face",
    };
  }
  const files = face.files?.length ?? 0;
  return {
    name: face.family ?? "",
    // AS THE GENERATED STYLESHEET WRITES IT: the family, then the system's
    // own sans for any character it does not draw.
    stack: registered ? `"${registered}", ${BRAND_FACE_FAMILIES["system-sans"]}` : "",
    detail: `Uploaded, ${plural(files, "file")}${face.licensed ? ", licensed to embed" : ""}`,
  };
}

/** The face a brand starts with: the kit's own, which needs no file. */
export const DEFAULT_FACE: BrandFace = { kit: "geist" };

// ---------------------------------------------------------------------------
// The document
// ---------------------------------------------------------------------------

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function strings(value: unknown): string[] | undefined {
  return Array.isArray(value) && value.every((v) => typeof v === "string") ? value : undefined;
}

function faceOf(value: unknown): BrandFace | null {
  if (!isObject(value)) return null;
  const face: BrandFace = {};
  if (typeof value.kit === "string") face.kit = value.kit;
  if (typeof value.stack === "string") face.stack = value.stack;
  if (typeof value.family === "string") face.family = value.family;
  if (Array.isArray(value.files)) {
    face.files = value.files.flatMap((f) =>
      isObject(f) &&
      typeof f.path === "string" &&
      typeof f.weight === "number" &&
      (f.style === "normal" || f.style === "italic")
        ? [{ path: f.path, weight: f.weight, style: f.style }]
        : [],
    );
  }
  if (value.licensed === true) face.licensed = true;
  if (face.kit || face.stack || face.family) return face;
  // AN EMPTY FACE (`{}`) is one a draft has not chosen: the kit's own, as the
  // engine reads it at publish and at adoption.
  return face.files?.length || face.licensed ? null : DEFAULT_FACE;
}

/**
 * A kit's `brand.json` as read from one of its versions, or null for text that
 * is not one. The engine holds every published version to the format, so this
 * only refuses what no published version holds; it keeps nothing the format
 * does not name, and never a colour that is not `#rrggbb`.
 */
export function parseBrandDocument(text: string): BrandDocument | null {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isObject(raw) || raw.v !== 1 || typeof raw.name !== "string") return null;
  const logos = isObject(raw.logos) ? raw.logos : {};
  const colors = isObject(raw.colors) ? raw.colors : {};
  const type = isObject(raw.type) ? raw.type : {};
  const voice = isObject(raw.voice) ? raw.voice : {};
  // A FACE A DRAFT HAS NOT CHOSEN is the kit's own, which is what the kit
  // draws with no input.
  const heading = type.heading === undefined ? DEFAULT_FACE : faceOf(type.heading);
  const body = type.body === undefined ? DEFAULT_FACE : faceOf(type.body);
  if (!heading || !body) return null;
  const doc: BrandDocument = {
    v: 1,
    name: raw.name,
    logos: {},
    colors: {},
    type: { heading, body },
    voice: {},
  };
  if (typeof raw.tagline === "string" && raw.tagline !== "") doc.tagline = raw.tagline;
  for (const { slot } of BRAND_LOGO_SLOTS) {
    const path = logos[slot];
    if (typeof path === "string" && path !== "") doc.logos[slot] = path;
  }
  for (const { role } of BRAND_COLOR_ROLES) {
    const value = colors[role];
    if (isBrandColor(value)) doc.colors[role] = value;
  }
  if (typeof voice.summary === "string" && voice.summary !== "") doc.voice.summary = voice.summary;
  const writeLike = strings(voice.write_like);
  const avoid = strings(voice.avoid);
  if (writeLike?.length) doc.voice.write_like = writeLike;
  if (avoid?.length) doc.voice.avoid = avoid;
  return doc;
}

/** A face with only its source's fields, in the format's order. */
function canonicalFace(face: BrandFace): BrandFace {
  if (face.kit) return { kit: face.kit };
  if (face.stack) return { stack: face.stack };
  return {
    family: face.family ?? "",
    files: (face.files ?? []).map((f) => ({ path: f.path, weight: f.weight, style: f.style })),
    licensed: face.licensed === true,
  };
}

/**
 * The `brand.json` a save publishes: the format's keys in the format's order,
 * an empty optional left out rather than written empty, and a trailing line
 * break, so two saves of the same brand are the same bytes and a version's
 * file reads like its neighbours.
 */
export function serializeBrandDocument(doc: BrandDocument): string {
  const colors: Record<string, string> = {};
  for (const { role } of BRAND_COLOR_ROLES) {
    const value = doc.colors[role];
    if (isBrandColor(value)) colors[role] = value;
  }
  const voice: BrandDocument["voice"] = {};
  if (doc.voice.summary?.trim()) voice.summary = doc.voice.summary.trim();
  const writeLike = (doc.voice.write_like ?? []).map((l) => l.trim()).filter((l) => l !== "");
  const avoid = (doc.voice.avoid ?? []).map((l) => l.trim()).filter((l) => l !== "");
  if (writeLike.length) voice.write_like = writeLike;
  if (avoid.length) voice.avoid = avoid;
  const out = {
    v: 1,
    name: doc.name.trim(),
    ...(doc.tagline?.trim() ? { tagline: doc.tagline.trim() } : {}),
    logos: {
      ...(doc.logos.primary ? { primary: doc.logos.primary } : {}),
      ...(doc.logos.on_dark ? { on_dark: doc.logos.on_dark } : {}),
      ...(doc.logos.mark ? { mark: doc.logos.mark } : {}),
    },
    colors,
    type: { heading: canonicalFace(doc.type.heading), body: canonicalFace(doc.type.body) },
    voice,
  };
  return `${JSON.stringify(out, null, 2)}\n`;
}

/** The file an adoption serves under `slot`, if it serves one. */
export function adoptedFile(adoption: Adoption | null, slot: string): BrandFileRef | undefined {
  return adoption?.files.find((f) => f.slot === slot);
}

// ---------------------------------------------------------------------------
// What changed
// ---------------------------------------------------------------------------

/** One change between two versions of a brand, as the adopt dialog lists it. */
export interface BrandChange {
  /** Which part changed, unique among the changes: `name`, `logo.primary`, `color.primary`. */
  key: string;
  /**
   * What changed, said so it names one part alone: "Primary logo", "Primary
   * colour", "Headings typeface", "Voice". A logo and a colour share their
   * role's word, so the card is part of the name.
   */
  label: string;
  /** What it was, where a value says it. */
  was?: string;
  /** What it is now, or what happened to it ("Replaced"). */
  now: string;
}

/** Where a logo's bytes are, by slot: the digest of the file each version names. */
export type LogoDigests = Readonly<Partial<Record<BrandLogoSlot, string>>>;

const NOT_SET = "Not set";

function lineChanges(before: readonly string[], after: readonly string[]): number {
  const was = new Set(before);
  const now = new Set(after);
  const added = after.filter((l) => !was.has(l)).length;
  const removed = before.filter((l) => !now.has(l)).length;
  return Math.max(added, removed);
}

function voiceLines(doc: BrandDocument | null): string[] {
  if (!doc) return [];
  return [
    ...(doc.voice.summary ? [`summary:${doc.voice.summary}`] : []),
    ...(doc.voice.write_like ?? []).map((l) => `like:${l}`),
    ...(doc.voice.avoid ?? []).map((l) => `avoid:${l}`),
  ];
}

function faceKey(face: BrandFace | undefined): string {
  return face ? JSON.stringify(canonicalFace(face)) : "";
}

/**
 * What adopting `after` would change from `before` (null: no brand is adopted
 * yet), card by card: the name and tagline, each colour with both values, each
 * logo by its bytes, each face by its name, and how many lines of the voice.
 */
export function brandChanges(
  before: BrandDocument | null,
  after: BrandDocument,
  digests: { before: LogoDigests; after: LogoDigests },
): BrandChange[] {
  const out: BrandChange[] = [];
  if ((before?.name ?? "") !== after.name) {
    out.push({
      key: "name",
      label: "Name",
      ...(before ? { was: before.name } : {}),
      now: after.name,
    });
  }
  if ((before?.tagline ?? "") !== (after.tagline ?? "")) {
    out.push({
      key: "tagline",
      label: "Tagline",
      ...(before?.tagline ? { was: before.tagline } : {}),
      now: after.tagline || NOT_SET,
    });
  }
  for (const { slot, changeLabel } of BRAND_LOGO_SLOTS) {
    const was = digests.before[slot] ?? "";
    const now = digests.after[slot] ?? "";
    if (was === now) continue;
    out.push({
      key: `logo.${slot}`,
      label: changeLabel,
      now: was === "" ? "Added" : now === "" ? "Removed" : "Replaced",
    });
  }
  for (const { role, label } of BRAND_COLOR_ROLES) {
    const was = before?.colors[role] ?? "";
    const now = after.colors[role] ?? "";
    if (was === now) continue;
    out.push({
      key: `color.${role}`,
      label: `${label} colour`,
      was: was || NOT_SET,
      now: now || NOT_SET,
    });
  }
  for (const { role, label } of BRAND_FACE_ROLES) {
    if (faceKey(before?.type[role]) === faceKey(after.type[role])) continue;
    const was = before ? faceDrawing(before.type[role]).name : "";
    out.push({
      key: `face.${role}`,
      label: `${label} typeface`,
      ...(was ? { was } : {}),
      now: faceDrawing(after.type[role]).name,
    });
  }
  const lines = lineChanges(voiceLines(before), voiceLines(after));
  if (lines > 0)
    out.push({ key: "voice", label: "Voice", now: `${plural(lines, "line")} changed` });
  return out;
}

/**
 * Whether a reader is offered adopting, changing or stopping the company's
 * brand: an administrator, and a person (`artifacts.brand.adopt` and
 * `.clear` are admin, human-only, with a recent proof). An agent never holds
 * admin, and a person signed in as no seat is told by the engine. The engine
 * decides every write; this only decides which controls are drawn.
 */
export function mayAdoptBrand(viewer: { admin: boolean; kind: string }): boolean {
  return viewer.admin && viewer.kind === "human";
}
