/**
 * The adopted brand and its files, as a screen reads them.
 *
 * # One read, every surface
 *
 * The brand chip on the Make row, the line under Ask an agent, the slide
 * theme, the Details fact, the brand tokens a doc is drawn in and Company ›
 * Brand all read ONE question, `brand{}` ([useBrand]); the socket shares it
 * between them.
 *
 * # Pictures and faces the one way the policy allows
 *
 * A logo or a face is the bytes of a file, fetched with the reader's own
 * credential from the API (an attachment, an SVG as text), shown through an
 * object URL typed from the brand's own record of the file (never from the
 * response, and only as one of the three logo types the format admits), and
 * a face registered as a `FontFace` under a name this page chose, so a family
 * the page has not loaded never resolves to a face of the same name the
 * reader's system holds. Nothing is loaded from an address a document names.
 * Every object URL is revoked and every face removed when the brand changes
 * or the screen closes.
 */

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "../useQuery.ts";
import { versionFile } from "../artifacts/api.ts";
import { imageType } from "../artifacts/versionImages.ts";
import type { BrandDocument } from "~/protocol/index.ts";
import { brandFile } from "./api.ts";
import {
  BRAND_FACE_ROLES,
  BRAND_LOGO_SLOTS,
  type Adoption,
  type BrandFaceRole,
  type BrandLogoSlot,
  type LogoDigests,
} from "./brand.ts";

/** The company's adopted brand: null while none is adopted, undefined while nothing has answered. */
export interface BrandRead {
  adoption: Adoption | null | undefined;
  loading: boolean;
  refetch: () => void;
}

/**
 * The adopted brand, read once per screen and asked again when the reader
 * comes back to the tab: an adoption is made on another page, often in
 * another tab, and nothing pushes it.
 */
export function useBrand(options: { enabled?: boolean } = {}): BrandRead {
  const read = useQuery("brand", {}, { refetchOnFocus: true, enabled: options.enabled ?? true });
  return {
    adoption: read.data ? read.data.brand : read.error ? null : undefined,
    loading: read.loading,
    refetch: read.refetch,
  };
}

/** The logo types the brand format admits, by content type. */
const LOGO_TYPES: ReadonlySet<string> = new Set(["image/svg+xml", "image/png", "image/webp"]);

/** One file a brand's document names: where its bytes come from, and what it is. */
interface BrandFileSpec {
  path: string;
  /** The type it is shown or loaded as. */
  type: string;
  /** Its digest, where the source knows it. */
  hash?: string;
}

/**
 * Where a brand's files are read from: the adopted snapshot (by digest,
 * `GET /brand/files/{hash}`), or one version of a kit (by path).
 */
export interface BrandSource {
  /** What the files are of, so an equal source reads nothing again. */
  key: string;
  document: BrandDocument;
  /** Every file the document names, by its path. */
  files: ReadonlyMap<string, BrandFileSpec>;
  read: (file: BrandFileSpec, signal: AbortSignal) => Promise<Blob>;
}

/** Every path a document names, with the slot each fills, logos first. */
function namedPaths(doc: BrandDocument): { path: string; slot: string; kind: "logo" | "font" }[] {
  const out: { path: string; slot: string; kind: "logo" | "font" }[] = [];
  for (const { slot, fileSlot } of BRAND_LOGO_SLOTS) {
    const path = doc.logos[slot];
    if (path) out.push({ path, slot: fileSlot, kind: "logo" });
  }
  for (const { role, fileSlot } of BRAND_FACE_ROLES) {
    (doc.type[role].files ?? []).forEach((f, i) => {
      out.push({ path: f.path, slot: `${fileSlot}.${i}`, kind: "font" });
    });
  }
  return out;
}

/**
 * The adopted brand's files already read in this tab, by digest. A digest
 * names its bytes for ever, so a logo read for the page is not read again for
 * the preview, a save or the next visit. Bounded, the oldest forgotten first.
 */
const adoptedBlobs = new Map<string, Promise<Blob>>();
const ADOPTED_BLOBS_KEPT = 32;

function adoptedBlob(hash: string): Promise<Blob> {
  const held = adoptedBlobs.get(hash);
  if (held) return held;
  // THE READ OUTLIVES THE SCREEN that asked for it, so a screen closed
  // mid-read leaves the next one a read to reuse; a failed read is
  // forgotten, so it is asked again.
  const read = brandFile(hash);
  read.catch(() => adoptedBlobs.delete(hash));
  adoptedBlobs.set(hash, read);
  if (adoptedBlobs.size > ADOPTED_BLOBS_KEPT)
    adoptedBlobs.delete(adoptedBlobs.keys().next().value!);
  return read;
}

/** The adopted brand as a source of files. */
export function adoptionSource(adoption: Adoption): BrandSource {
  const bySlot = new Map(adoption.files.map((f) => [f.slot, f]));
  const files = new Map<string, BrandFileSpec>();
  for (const named of namedPaths(adoption.document)) {
    const served = bySlot.get(named.slot);
    if (served) {
      files.set(named.path, {
        path: named.path,
        type: served.content_type,
        hash: served.object.hash,
      });
    }
  }
  return {
    key: `adopted:${adoption.css_sha}`,
    document: adoption.document,
    files,
    read: (file) =>
      file.hash
        ? adoptedBlob(file.hash)
        : Promise.reject(new Error(`${file.path} is not served by the adopted brand`)),
  };
}

/**
 * One version of a kit as a source of files. `digests` are the version's
 * manifest's, by path, where the caller read it (a review that compares logos
 * by their bytes).
 */
export function versionSource(
  kit: string,
  version: number,
  document: BrandDocument,
  digests: ReadonlyMap<string, string> = new Map(),
): BrandSource {
  const files = new Map<string, BrandFileSpec>();
  for (const named of namedPaths(document)) {
    const type = named.kind === "logo" ? imageType(named.path) : "font/woff2";
    if (!type) continue;
    const hash = digests.get(named.path);
    files.set(named.path, { path: named.path, type, ...(hash ? { hash } : {}) });
  }
  return {
    key: `${kit}:${version}`,
    document,
    files,
    read: (file, signal) => versionFile(kit, version, file.path, signal),
  };
}

/** Each logo's digest in a source, by slot: what a review compares two versions' logos by. */
export function logoDigests(source: BrandSource | null): LogoDigests {
  const out: Partial<Record<BrandLogoSlot, string>> = {};
  if (!source) return out;
  for (const { slot } of BRAND_LOGO_SLOTS) {
    const path = source.document.logos[slot];
    const hash = path ? source.files.get(path)?.hash : undefined;
    if (hash) out[slot] = hash;
    else if (path) out[slot] = `path:${path}`;
  }
  return out;
}

/** A brand's logos and faces, loaded: an object URL per logo slot, a registered family per face. */
export interface BrandAssets {
  /** The logo in `slot`, or null while it loads, when it failed, or where none is set. */
  logo: (slot: BrandLogoSlot) => string | null;
  /** The family an uploaded face was registered under in this page, or undefined. */
  face: (role: BrandFaceRole) => string | undefined;
}

/** A family name for a face this page registers: unique to the source, never a name a system holds. */
function familyFor(source: BrandSource, role: BrandFaceRole): string {
  let hash = 0;
  for (const ch of source.key) hash = (hash * 31 + ch.charCodeAt(0)) >>> 0;
  return `crewlet-brand-${role}-${hash.toString(36)}`;
}

/**
 * Loads a brand's logos and uploaded faces from `source` (null: nothing to
 * load). A kit face or a system stack loads nothing.
 */
export function useBrandAssets(source: BrandSource | null): BrandAssets {
  const [urls, setUrls] = useState<ReadonlyMap<BrandLogoSlot, string>>(new Map());
  const [faces, setFaces] = useState<ReadonlyMap<BrandFaceRole, string>>(new Map());
  const key = source?.key ?? "";

  useEffect(() => {
    if (!source) return undefined;
    const ctl = new AbortController();
    const made: string[] = [];
    void Promise.all(
      BRAND_LOGO_SLOTS.map(async ({ slot }) => {
        const path = source.document.logos[slot];
        const file = path ? source.files.get(path) : undefined;
        if (!file || !LOGO_TYPES.has(file.type)) return null;
        try {
          const blob = await source.read(file, ctl.signal);
          const url = URL.createObjectURL(new Blob([blob], { type: file.type }));
          made.push(url);
          return [slot, url] as const;
        } catch {
          return null;
        }
      }),
    ).then((entries) => {
      if (ctl.signal.aborted) return;
      setUrls(new Map(entries.filter((e): e is readonly [BrandLogoSlot, string] => e !== null)));
    });
    return () => {
      ctl.abort();
      for (const url of made) URL.revokeObjectURL(url);
      setUrls(new Map());
    };
    // THE SOURCE'S KEY names its files, so an equal source rebuilt each
    // render reads nothing again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  useEffect(() => {
    if (!source || typeof FontFace === "undefined") return undefined;
    const ctl = new AbortController();
    const added: FontFace[] = [];
    for (const { role } of BRAND_FACE_ROLES) {
      const face = source.document.type[role];
      const entries = face.family ? (face.files ?? []) : [];
      if (entries.length === 0) continue;
      const family = familyFor(source, role);
      void Promise.all(
        entries.map(async (entry) => {
          const file = source.files.get(entry.path);
          if (!file) return false;
          try {
            const bytes = await (await source.read(file, ctl.signal)).arrayBuffer();
            const loaded = await new FontFace(family, bytes, {
              weight: String(entry.weight),
              style: entry.style,
            }).load();
            if (ctl.signal.aborted) return false;
            document.fonts.add(loaded);
            added.push(loaded);
            return true;
          } catch {
            // A FACE THAT DID NOT LOAD falls back to the interface's own type,
            // as the specimen's family list ends in it.
            return false;
          }
        }),
      ).then((loaded) => {
        if (ctl.signal.aborted || !loaded.some(Boolean)) return;
        setFaces((was) => new Map(was).set(role, family));
      });
    }
    return () => {
      ctl.abort();
      for (const f of added) document.fonts.delete(f);
      setFaces(new Map());
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  return useMemo(
    () => ({
      logo: (slot: BrandLogoSlot) => urls.get(slot) ?? null,
      face: (role: BrandFaceRole) => faces.get(role),
    }),
    [urls, faces],
  );
}

/** The adopted brand and its loaded assets, for the surfaces that only show it. */
export function useAdoptedBrand(): BrandRead & { assets: BrandAssets } {
  const read = useBrand();
  const source = useMemo(
    () => (read.adoption ? adoptionSource(read.adoption) : null),
    [read.adoption],
  );
  const assets = useBrandAssets(source);
  return { ...read, assets };
}

/**
 * The adopted brand and its mark alone, for the small places that name the
 * brand (the Make row's chip, the note under Ask an agent): the square mark,
 * or the primary logo where the brand has no mark, as the format says those
 * places show it.
 */
export function useAdoptedMark(): { adoption: Adoption | null | undefined; mark: string | null } {
  const { adoption } = useBrand();
  const file = adoption
    ? (adoption.files.find((f) => f.slot === "logo.mark") ??
      adoption.files.find((f) => f.slot === "logo.primary"))
    : undefined;
  const hash = file && LOGO_TYPES.has(file.content_type) ? file.object.hash : "";
  const type = file?.content_type ?? "";
  const [mark, setMark] = useState<string | null>(null);
  useEffect(() => {
    if (hash === "") return undefined;
    let live = true;
    let made = "";
    adoptedBlob(hash)
      .then((blob) => {
        if (!live) return;
        made = URL.createObjectURL(new Blob([blob], { type }));
        setMark(made);
      })
      .catch(() => {
        // A MARK THAT DID NOT LOAD is drawn as the brand glyph, which is
        // what the chip draws with no mark.
      });
    return () => {
      live = false;
      if (made) URL.revokeObjectURL(made);
      setMark(null);
    };
  }, [hash, type]);
  return { adoption, mark };
}

/** A brand drawn with no file read: no logo, and every face in its fallback. */
export const NO_BRAND_ASSETS: BrandAssets = { logo: () => null, face: () => undefined };
