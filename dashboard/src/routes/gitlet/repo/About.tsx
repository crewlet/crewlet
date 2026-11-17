/**
 * What a repository is, beside its code: its facts as sentences, each led by its glyph. Every value
 * is the page's row but the default branch's protection; the rail's other sections read their own.
 */

import { createContext, useContext, useEffect, type MouseEvent, type ReactNode } from "react";
import { IconButtonLink, Skeleton } from "@crewlethq/ui";
import {
  ActivityGlyph,
  BookOpenGlyph,
  ClockGlyph,
  FileTextGlyph,
  HardDriveGlyph,
  KeyGlyph,
  LockGlyph,
  NetworkGlyph,
  ScaleGlyph,
  SearchGlyph,
  SettingsGlyph,
  ShieldCheckGlyph,
  ShieldGlyph,
  UploadGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { ago } from "~/lib/ago.ts";
import { fmtBytes, fmtDateTime, plural } from "~/lib/format.ts";
import { importStanding } from "~/lib/repoWrites.ts";
import { indexOrg, seatPath, unitByKey, unitPlace, useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";
import { repoHref } from "../paths.ts";
import { Face } from "../ui.tsx";
import { protectionWords, useDefaultBranch } from "./Branches.tsx";
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

/** The root's documents the README card holds (`readme`, `license`), reported up to About. */
export const RootDocs = createContext<(docs: string) => void>(() => {});

/** Reports the documents a root holds, comma-separated, to the rail beside it. */
export function useReportDocs(docs: string): void {
  const report = useContext(RootDocs);
  useEffect(() => report(docs), [report, docs]);
}

/** One fact: its glyph, then its sentence. */
function Fact({
  glyph,
  title,
  children,
}: {
  glyph: ReactNode;
  title?: string;
  children: ReactNode;
}) {
  return (
    <li className="code-repo-rail__fact" title={title}>
      <span className="code-repo-rail__glyph">{glyph}</span>
      <span className="code-repo-rail__words">{children}</span>
    </li>
  );
}

/** A document's link: its tab of the README card chosen and focused, else the address. */
function DocLink({ repo, doc, label }: { repo: string; doc: "readme" | "license"; label: string }) {
  const open = (e: MouseEvent) => {
    const tab = [...document.querySelectorAll<HTMLElement>('.code-readme [role="tab"]')].find(
      (t) => t.textContent === (doc === "license" ? "LICENSE" : "README"),
    );
    if (!tab) return;
    e.preventDefault();
    tab.click();
    tab.focus();
  };
  return (
    <a
      className="t-link"
      href={repoHref(repo, doc === "license" ? { doc: "license" } : {})}
      onClick={open}
    >
      {label}
    </a>
  );
}

/** The default branch's protection in its rules' words, else how many rules the repository holds. */
function Protection({ repo }: { repo: CodeRepoDetail }) {
  const found = useDefaultBranch(repo);
  const branch = <strong>{repo.default_branch}</strong>;
  const settings = repoHref(repo.name, { tab: "settings" });
  if (found.loading) {
    return (
      <li className="code-repo-rail__fact">
        <Skeleton variant="text" rows={1} label="Reading the default branch's rules" />
      </li>
    );
  }
  if (found.row?.protection) {
    return (
      <Fact glyph={<ShieldCheckGlyph size="sm" />}>
        {branch} is protected: {protectionWords(found.row.protection)}
      </Fact>
    );
  }
  if (found.row) {
    return (
      <Fact glyph={<ShieldGlyph size="sm" />}>
        <a className="t-link" href={settings}>
          {branch} has no protection rule
        </a>
      </Fact>
    );
  }
  const rules = repo.protections?.length ?? 0;
  return (
    <Fact glyph={<ShieldGlyph size="sm" />}>
      <a className="t-link" href={settings}>
        {rules === 0 ? "No protection rule" : plural(rules, "protection rule")}
      </a>
    </Fact>
  );
}

/** Who owns it: a unit by its name off the chart (its key where the chart has none), or a seat. */
function Owner({ repo }: { repo: CodeRepoDetail }) {
  const org = useOrg();
  const seatBadge = useSeatBadgeOf();
  const { owner } = repo;
  if (owner.kind === "seat") {
    return (
      <Fact glyph={<Face who={owner.ref} />}>
        Owned by{" "}
        <a className="prose-link" href={href(seatPath({ handle: owner.ref, name: owner.ref }))}>
          <strong>{seatBadge(owner.ref).name || owner.ref}</strong>
        </a>
      </Fact>
    );
  }
  const unit = unitByKey(indexOrg(org), owner.ref);
  return (
    <Fact glyph={<NetworkGlyph size="sm" />}>
      Owned by{" "}
      {unit ? (
        <a className="prose-link" href={href(unitPlace(unit).path, unitPlace(unit).query)}>
          <strong>{unit.name}</strong>
        </a>
      ) : (
        <strong>{owner.ref}</strong>
      )}
    </Fact>
  );
}

/** About: its documents, its protection, who owns and reads it, its search, size and times. */
export function About({ repo, docs }: { repo: CodeRepoDetail; docs: string }) {
  const now = useNow();
  const has = docs.split(",");
  const standing = importStanding(repo);
  return (
    <section className="code-repo-rail__sec" aria-labelledby="code-about-title">
      <div className="code-repo-rail__head">
        <h2 id="code-about-title" className="code-repo-rail__about">
          About
        </h2>
        <span className="code-repo-rail__end">
          <IconButtonLink
            href={repoHref(repo.name, { tab: "settings" })}
            icon={<SettingsGlyph />}
            label="Open the repository's settings"
            size="sm"
            variant="ghost"
          />
        </span>
      </div>
      <ul className="code-repo-rail__facts">
        {has.includes("readme") && (
          <Fact glyph={<BookOpenGlyph size="sm" />}>
            <DocLink repo={repo.name} doc="readme" label="Readme" />
          </Fact>
        )}
        {has.includes("license") && (
          <Fact glyph={<ScaleGlyph size="sm" />}>
            <DocLink repo={repo.name} doc="license" label="LICENSE" />
          </Fact>
        )}
        <Protection repo={repo} />
        <Owner repo={repo} />
        <Fact
          glyph={repo.visibility === "unit" ? <LockGlyph size="sm" /> : <UsersGlyph size="sm" />}
          title="Every person reads every repository."
        >
          {readByWord(repo.visibility)} {repo.visibility === "unit" ? "read" : "reads"} it
        </Fact>
        <Fact glyph={<SearchGlyph size="sm" />}>
          {repo.search === "indexed" ? "Indexed for code search" : searchWord(repo.search)}
        </Fact>
        <Fact glyph={<HardDriveGlyph size="sm" />} title={plural(repo.live_objects, "object")}>
          <strong>{fmtBytes(repo.live_bytes)}</strong> in {plural(repo.live_packs, "pack")}
        </Fact>
        <Fact glyph={<ClockGlyph size="sm" />}>
          Created{" "}
          <time dateTime={repo.created_at} title={fmtDateTime(repo.created_at)}>
            {ago(repo.created_at, now)}
          </time>
        </Fact>
        <Fact glyph={<UploadGlyph size="sm" />}>
          {repo.last_push_at ? (
            <>
              Last pushed{" "}
              <time dateTime={repo.last_push_at} title={fmtDateTime(repo.last_push_at)}>
                {ago(repo.last_push_at, now)}
              </time>
            </>
          ) : (
            "Never pushed"
          )}
        </Fact>
        {repo.kind === "scratch" && repo.last_activity_at && (
          <Fact glyph={<ActivityGlyph size="sm" />}>
            Last active{" "}
            <time dateTime={repo.last_activity_at} title={fmtDateTime(repo.last_activity_at)}>
              {ago(repo.last_activity_at, now)}
            </time>
          </Fact>
        )}
        {repo.managed_by === "config" && (
          <Fact glyph={<FileTextGlyph size="sm" />}>Declared in the company document</Fact>
        )}
        {repo.access && (
          <Fact glyph={<KeyGlyph size="sm" />}>
            Your access: <strong>{repo.access}</strong>
          </Fact>
        )}
        {standing && <Fact glyph={<UploadGlyph size="sm" />}>{standing}</Fact>}
      </ul>
    </section>
  );
}
