/**
 * A brand edit as the assistant reads and changes it, for both screens that
 * hold one: the setup wizard and the Brand page's card editors
 * (`useAssistantDraft`).
 *
 * # What it can change, and what it cannot
 *
 * The text and value fields: the name and tagline, the colours, the
 * typefaces from the kit's and the system's own, and the voice. Each value is
 * held to the rule its own field holds a typed one to (`validate.ts`), so a
 * change the assistant proposes is refused in the same sentence. A logo is a
 * file field: a change names one of the person's files in this conversation
 * by reference (the engine checked it is theirs before the card was drawn),
 * and the screen fetches its bytes through the owner's route once the change
 * is approved and keeps them as a picked file, saved with the rest. Uploaded
 * font families are the person's to bring, read as metadata and never set.
 * What the assistant says about "assets" is in the help this module gives
 * the screen, never a special case in its prompt.
 */

import { BRAND_KIT_FACES, BRAND_SYSTEM_STACKS } from "~/contract/brand.ts";
import type { BrandDocument, BrandFace } from "~/protocol/index.ts";
import {
  DraftRefusal,
  refuse,
  type DraftFieldSpec,
  type DraftValue,
} from "~/app/assistant/draft.ts";
import type { AssistantDraftFileRef } from "~/contract/assistant.ts";
import { conversationFile } from "~/protocol/assistant.ts";
import {
  BRAND_COLOR_ROLES,
  BRAND_FACE_ROLES,
  BRAND_LOGO_SLOTS,
  FACE_NAMES,
  faceDrawing,
  isBrandColor,
} from "./brand.ts";
import { logoPath, logoProblem, type BrandEdit } from "./draft.ts";
import type { BrandLogoSlot } from "./brand.ts";
import {
  nameProblem,
  taglineProblem,
  voiceProblems,
  withDocument,
  type BrandPart,
} from "./validate.ts";

/** The screen's word to the assistant about a brand draft. */
export const BRAND_ASSISTANT_HELP =
  "You can propose the name, tagline, colours, the heading and body typefaces from the " +
  "choices listed, and the voice. A logo can be proposed only as a reference to one of the " +
  "person's files in this conversation, never as markup; uploaded font files are theirs to " +
  "add. You make no images here: asked for assets, say that you can propose colours, type " +
  "and voice, and offer an SVG starting point for a logo only if they ask. The page checks " +
  "the colours' contrast. Saving and publishing stay the person's.";

/**
 * The Brand page's button that opens a standing draft's setup, by whether the
 * draft is ready to publish: "Finish draft" while it lacks what publishing
 * needs, "Edit draft" once it has it. One place, read by the button and by
 * the help that names it, so renaming the button never leaves the assistant
 * pointing at one the page no longer shows.
 */
export function brandDraftLabel(ready: boolean): string {
  return ready ? "Edit draft" : "Finish draft";
}

/**
 * The Brand page's word to the assistant about a draft a setup left, which
 * the page shows and does not edit: its values are read here, and changed
 * only in the setup that holds it, which the button [brandDraftLabel] names
 * for the same readiness opens.
 */
export function brandStandingDraftHelp(ready: boolean): string {
  return (
    "This is the company's brand draft, saved and not published. This page shows it and does " +
    "not edit it, so you cannot change it here. To change its name, tagline, colours, " +
    `typefaces or voice, tell the person to choose ${brandDraftLabel(ready)} on this page, ` +
    "which opens the setup where you can propose those changes. Publishing it stays an " +
    "administrator's."
  );
}

/** The logo types the brand format admits, as a file reference names them. */
const LOGO_TYPES: readonly string[] = ["image/svg+xml", "image/png", "image/webp"];

/**
 * A brand edit as a change to it is written: the edit, and the logos an
 * approved change names that are files of the person's conversation, by the
 * path the document names each at. Only between a change and its applying:
 * [applyBrandDraft] fetches each and keeps it as a picked file.
 */
export interface BrandDraft extends BrandEdit {
  incoming?: ReadonlyMap<string, { slot: BrandLogoSlot; ref: AssistantDraftFileRef }>;
}

/**
 * Apply an approved change to a brand edit: everything but its incoming
 * logos at once, through the screen's own setter; then each logo's bytes,
 * fetched as the person through the owner's route (`conversationFile`) and
 * kept as the file a person would have picked, typed from the reference and
 * never from the response. A logo whose bytes cannot be had puts its slot
 * back as it was and says why; one the person changed since is left alone.
 */
export function applyBrandDraft(
  before: BrandEdit,
  next: BrandDraft,
  setEdit: (change: (edit: BrandEdit) => BrandEdit) => void,
  report: (sentence: string) => void,
  fetchFile: (ref: AssistantDraftFileRef) => Promise<Blob> = (ref) => conversationFile(ref),
): void {
  const { incoming, ...edit } = next;
  setEdit(() => edit);
  for (const [path, { slot, ref }] of incoming ?? []) {
    const named = (cur: BrandEdit) => cur.document.logos[slot] === path;
    fetchFile(ref).then(
      (blob) => {
        const file = new File([blob], ref.name, { type: ref.media_type });
        setEdit((cur) =>
          named(cur) ? { ...cur, files: new Map(cur.files).set(path, file) } : cur,
        );
      },
      () => {
        setEdit((cur) =>
          named(cur)
            ? withDocument(cur, (doc) => {
                const logos = { ...doc.logos };
                const was = before.document.logos[slot];
                if (was) logos[slot] = was;
                else delete logos[slot];
                return { ...doc, logos };
              })
            : cur,
        );
        report(`${ref.name} could not be read, so the logo was not changed. Try again.`);
      },
    );
  }
}

/** Every face a value may name: the kit's own, then the system's. */
const FACE_CHOICES: readonly string[] = [...BRAND_KIT_FACES, ...BRAND_SYSTEM_STACKS];

/** A face as its field reads: its choice's key, or an uploaded family by name. */
function faceValue(face: BrandFace): string {
  if (face.kit) return face.kit;
  if (face.stack) return face.stack;
  return face.family ? `${faceDrawing(face).name} (uploaded)` : "";
}

/** A face chosen by its key, or why it cannot be. */
function faceOf(value: string): BrandFace | DraftRefusal {
  const key = value.trim().toLowerCase();
  if ((BRAND_KIT_FACES as readonly string[]).includes(key)) return { kit: key };
  if ((BRAND_SYSTEM_STACKS as readonly string[]).includes(key)) return { stack: key };
  return refuse(
    `Choose one of ${FACE_CHOICES.join(", ")}. An uploaded family is added by the person with its files.`,
  );
}

/** A text value, or why a list is not one. */
function textOf(value: DraftValue): string | null {
  return typeof value === "string" ? value : null;
}

/** A list value's items: a list as it is, a text one per line. */
function linesOf(value: DraftValue): string[] {
  return typeof value === "string" ? value.split("\n") : [...value];
}

/** The edit with its voice changed, or the voice's own refusal. */
function withVoice(
  edit: BrandEdit,
  change: (voice: BrandDocument["voice"]) => BrandDocument["voice"],
  which: "summary" | "like" | "avoid",
) {
  const voice = change(edit.document.voice);
  const wrong = voiceProblems(voice)[which];
  return wrong ? refuse(wrong) : withDocument(edit, (doc) => ({ ...doc, voice }));
}

/**
 * Every field of a brand edit, in the order the setup draws them; a field of
 * a part in `settable` takes a change, and every other is read only.
 */
export function brandDraftFields(settable: ReadonlySet<BrandPart>): DraftFieldSpec<BrandDraft>[] {
  const fields: DraftFieldSpec<BrandDraft>[] = [];
  const add = (part: BrandPart, field: DraftFieldSpec<BrandDraft>) => {
    if (!settable.has(part)) {
      delete field.write;
      delete field.file;
    }
    fields.push(field);
  };

  add("identity", {
    path: "name",
    label: "Name",
    hint: "The company's name as it should appear",
    read: (e) => e.document.name,
    write: (e, value) => {
      const name = textOf(value);
      if (name === null) return refuse("The name is a line of text.");
      const next = withDocument(e, (doc) => ({ ...doc, name }));
      const wrong = nameProblem(next.document);
      return wrong ? refuse(wrong) : next;
    },
  });
  add("identity", {
    path: "tagline",
    label: "Tagline",
    hint: "One short line, no links",
    read: (e) => e.document.tagline ?? "",
    write: (e, value) => {
      const tagline = textOf(value);
      if (tagline === null) return refuse("The tagline is a line of text.");
      const next = withDocument(e, (doc) => ({ ...doc, tagline }));
      const wrong = taglineProblem(next.document);
      return wrong ? refuse(wrong) : next;
    },
  });
  for (const logo of BRAND_LOGO_SLOTS) {
    add("logos", {
      path: `logos.${logo.slot}`,
      label: `${logo.label} logo`,
      hint: `One of your files: ${LOGO_TYPES.join(", ")}; empty where none is chosen`,
      read: (e) => e.document.logos[logo.slot] ?? "",
      file: {
        accepts: LOGO_TYPES,
        ground: (e) => ({
          tone: logo.ground,
          color:
            logo.ground === "dark" ? e.document.colors.dark_ground : e.document.colors.light_ground,
        }),
        write: (e, ref) => {
          // THE PATH THE FILE IS SAVED AT, from its type as the store holds
          // it; the bytes follow once the change is approved ([applyBrandDraft]).
          const stub = new File([], ref.name, { type: ref.media_type });
          const problem = logoProblem(stub);
          if (problem) return refuse(problem);
          const path = logoPath(logo.slot, stub);
          const incoming = new Map(e.incoming ?? []);
          incoming.set(path, { slot: logo.slot, ref });
          return {
            ...withDocument(e, (doc) => ({ ...doc, logos: { ...doc.logos, [logo.slot]: path } })),
            incoming,
          };
        },
      },
    });
  }
  for (const colour of BRAND_COLOR_ROLES) {
    add("colours", {
      path: `colors.${colour.role}`,
      label: `${colour.label} colour`,
      hint: `#rrggbb; ${colour.description}${colour.required ? "; required" : "; empty for the default"}`,
      read: (e) => e.document.colors[colour.role] ?? "",
      write: (e, value) => {
        const text = textOf(value)?.trim().toLowerCase();
        if (text === undefined)
          return refuse(`The ${colour.label.toLowerCase()} colour is one value.`);
        if (text === "" && colour.required) return refuse("Choose the primary colour.");
        if (text !== "" && !isBrandColor(text)) {
          return refuse(`Write the ${colour.label.toLowerCase()} colour as # and six hex digits.`);
        }
        return withDocument(e, (doc) => {
          const colors = { ...doc.colors };
          if (text === "") delete colors[colour.role];
          else colors[colour.role] = text;
          return { ...doc, colors };
        });
      },
    });
  }
  for (const face of BRAND_FACE_ROLES) {
    add("type", {
      path: `type.${face.role}`,
      label: `${face.label} typeface`,
      hint: `One of ${FACE_CHOICES.map((k) => `${k} (${FACE_NAMES[k as keyof typeof FACE_NAMES]})`).join(", ")}`,
      read: (e) => faceValue(e.document.type[face.role]),
      write: (e, value) => {
        const text = textOf(value);
        if (text === null) return refuse("A typeface is one choice.");
        const chosen = faceOf(text);
        if (chosen instanceof DraftRefusal) return chosen;
        return withDocument(e, (doc) => ({ ...doc, type: { ...doc.type, [face.role]: chosen } }));
      },
    });
  }
  add("voice", {
    path: "voice.summary",
    label: "Voice summary",
    hint: "How the company sounds, in a few sentences",
    read: (e) => e.document.voice.summary ?? "",
    write: (e, value) => {
      const summary = textOf(value);
      if (summary === null) return refuse("The voice summary is text.");
      return withVoice(e, (voice) => ({ ...voice, summary }), "summary");
    },
  });
  add("voice", {
    path: "voice.write_like",
    label: "Write like this",
    hint: "Example lines, one per line",
    read: (e) => e.document.voice.write_like ?? [],
    write: (e, value) =>
      withVoice(e, (voice) => ({ ...voice, write_like: linesOf(value) }), "like"),
  });
  add("voice", {
    path: "voice.avoid",
    label: "Avoid",
    hint: "Lines to avoid, one per line",
    read: (e) => e.document.voice.avoid ?? [],
    write: (e, value) => withVoice(e, (voice) => ({ ...voice, avoid: linesOf(value) }), "avoid"),
  });
  return fields;
}
