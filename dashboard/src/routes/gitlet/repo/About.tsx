/**
 * What a repository is, beside its code: the rail every code host draws
 * there. The counts a reader places it by — each a link to the tab that lists
 * them — and its properties: who owns it, which seats read it, what decides
 * its settings, how code search reads it and where it stands with other
 * hosts. Its description is the header's, which every tab draws, and its
 * branches and tags are counted in the bar beside the revision picker.
 *
 * EVERY VALUE IS THE ROW'S (`code_repo`), read by the page every minute: the
 * rail asks nothing of its own. What it held on the old Overview tab — a fact
 * sheet a reader had to pass before reaching a file — is here, read across a
 * gap from the code it describes.
 */

import { EmptyValue } from "@crewlethq/ui";
import { ClockGlyph, HardDriveGlyph, ShieldGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { useNow } from "~/lib/clock.ts";
import { ago } from "~/lib/ago.ts";
import { fmtBytes, fmtDateTime, plural } from "~/lib/format.ts";
import { importStanding, pullMirrorStanding, pushMirrorStanding } from "~/lib/repoWrites.ts";
import type { CodeOwner, CodeRepoDetail } from "~/protocol/index.ts";
import { repoHref } from "../paths.ts";
import { Face } from "../ui.tsx";
import "~/styles/gitlet.css";

/** Which seats read a repository, in words. Every person reads every repository (ADR-0088). */
export function readByWord(visibility: string): string {
  return visibility === "unit" ? "Its owner's seats" : "Every seat";
}

/** How code search reads a repository, in words. */
export function searchWord(search: string): string {
  switch (search) {
    case "indexed":
      return "Indexed from its default branch";
    case "grep":
      return "Searched only by a search naming it";
  }
  return search;
}

/** What decides a repository's settings and its life, in words. */
export function managedWord(managedBy: string): string {
  switch (managedBy) {
    case "config":
      return "The company document";
    case "tools":
      return "The code tools";
  }
  return managedBy;
}

/** The rail's counts: each a glyph, a number and what it counts, linking to the tab that lists them. */
function Stats({ repo }: { repo: CodeRepoDetail }) {
  const now = useNow();
  const rules = repo.protections?.length ?? 0;
  return (
    <ul className="code-about__stats" aria-label="The repository in numbers">
      <li>
        <UploadGlyph size="sm" className="code-about__glyph" />
        <a className="t-link" href={repoHref(repo.name, { tab: "pushes" })}>
          {plural(repo.pushes, "push", "pushes")}
        </a>
      </li>
      <li>
        <ShieldGlyph size="sm" className="code-about__glyph" />
        <a className="t-link" href={repoHref(repo.name, { tab: "settings" })}>
          {rules === 0 ? "No protection rule" : plural(rules, "protection rule")}
        </a>
      </li>
      <li>
        <HardDriveGlyph size="sm" className="code-about__glyph" />
        <span title={`${plural(repo.live_packs, "pack")}, ${plural(repo.live_objects, "object")}`}>
          {fmtBytes(repo.live_bytes)}
        </span>
      </li>
      <li>
        <ClockGlyph size="sm" className="code-about__glyph" />
        {repo.last_push_at ? (
          <time dateTime={repo.last_push_at} title={fmtDateTime(repo.last_push_at)}>
            Pushed {ago(repo.last_push_at, now)}
          </time>
        ) : (
          <span>Never pushed</span>
        )}
      </li>
    </ul>
  );
}

/** Who owns a repository: a seat by its face and name, a unit by its name. */
function Owner({ owner, name }: { owner: CodeOwner; name: string }) {
  if (owner.kind !== "seat") return <span>{name}</span>;
  return (
    <span className="code-about__owner">
      <Face who={owner.ref} />
      <span>{name}</span>
    </span>
  );
}

export function About({
  repo,
  ownerName,
}: {
  repo: CodeRepoDetail;
  ownerName: (owner: CodeOwner) => string;
}) {
  return (
    <section className="code-about" aria-labelledby="code-about-title">
      <h2 id="code-about-title" className="code-about__title">
        About
      </h2>
      <Stats repo={repo} />
      <PropertiesRail
        headings="sentence"
        groups={[
          {
            name: "Repository",
            properties: [
              { label: "Owner", value: <Owner owner={repo.owner} name={ownerName(repo.owner)} /> },
              {
                label: "Read by",
                value: readByWord(repo.visibility),
                title: "Which seats read it. Every person reads every repository.",
              },
              {
                label: "Kind",
                value: repo.kind === "scratch" ? "A seat's scratch repository" : "Project",
              },
              { label: "Managed by", value: managedWord(repo.managed_by) },
              { label: "Declared", value: fmtDateTime(repo.created_at) },
              ...(repo.access ? [{ label: "Your access", value: repo.access }] : []),
              {
                label: "Last activity",
                value: repo.last_activity_at ? (
                  fmtDateTime(repo.last_activity_at)
                ) : (
                  <EmptyValue label="None" />
                ),
              },
            ],
          },
          {
            name: "Search and other hosts",
            properties: [
              { label: "Search", value: searchWord(repo.search) },
              { label: "Import", value: importStanding(repo) ?? undefined },
              { label: "Pull mirror", value: pullMirrorStanding(repo) ?? undefined },
              { label: "Push mirror", value: pushMirrorStanding(repo) ?? undefined },
            ],
          },
        ]}
      />
    </section>
  );
}
