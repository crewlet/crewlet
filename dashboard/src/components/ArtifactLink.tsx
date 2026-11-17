/**
 * An artifact named in prose or a row, drawn as a work item's key is (the
 * kit's `ArtifactRef`): its kind's glyph and its title, linked to its page.
 * Wherever an artifact is linked, it is drawn by this.
 */

import type { ReactNode } from "react";
import { ArtifactRef, Link } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { artifactPath, resolve } from "~/app/routes.ts";
import { appRoute } from "~/lib/appRoute.ts";
import { useArtifactHeadline, type ArtifactHeadline } from "~/lib/artifacts/heads.ts";
import { KIND_WORDS } from "~/lib/artifacts/words.ts";

/** The artifact an address opens, or null where it opens none. */
export function artifactAddress(
  address: string,
): { id: string; hash: string; version: number } | null {
  const target = appRoute(address);
  if (!target) return null;
  const where = resolve(target.path);
  if (!where.resolved || where.screen !== "artifact") return null;
  const version = Number(target.query["version"]);
  return {
    id: where.id,
    hash: target.hash,
    version: Number.isInteger(version) && version > 0 ? version : 0,
  };
}

/** An artifact's page, at a version where one is named: what a rebuilt reference opens. */
export function artifactHref(id: string, version = 0): string {
  return href(artifactPath(id), version > 0 ? { version: String(version) } : undefined);
}

export function ArtifactLink({
  id,
  known,
  to,
  version = 0,
  words,
  fallback,
  quiet = false,
  open,
  close,
}: {
  id: string;
  /** Its kind and title where the caller holds them (an Inbox notice); read by id where not. */
  known?: ArtifactHeadline | undefined;
  /** Where it opens; left out, it is words. */
  to?: string | undefined;
  /** The version the reference names, said after its title. */
  version?: number;
  /** What is drawn while the artifact is read: the words as written. */
  words: string;
  /** What is drawn where the reader may not see it or it is gone. */
  fallback?: ReactNode;
  /** A row's reference: in a quiet link, as a row's task is (`routes/inbox/TaskLink.tsx`). */
  quiet?: boolean;
  open?: string | undefined;
  close?: string | undefined;
}) {
  const read = useArtifactHeadline(known ? null : id);
  const headline = known ?? read;
  if (headline === null) return <>{fallback ?? `${open ?? ""}${words}${close ?? ""}`}</>;
  const ref = (
    <ArtifactRef
      kind={headline?.kind}
      kindLabel={headline ? KIND_WORDS[headline.kind] : undefined}
      title={headline?.title || words}
      after={version > 0 ? `, version ${version}` : undefined}
      href={quiet ? undefined : to}
      open={open}
      close={close}
    />
  );
  return quiet && to ? (
    <Link href={to} variant="quiet" size="inherit">
      {ref}
    </Link>
  ) : (
    ref
  );
}
