/**
 * A repository's rail beside its code at the root: About, its open changes, who pushes to it and
 * its pushes by day, its mirror and its newest tag, each section reading its own question.
 */

import { useEffect, useMemo, useRef } from "react";
import { Skeleton, Tag } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { GitPullRequestDraftGlyph, GitPullRequestGlyph, TagGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { changePath, refName, shortOID } from "~/lib/code.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { describeLink } from "~/lib/inlineTokens.ts";
import { useLinkHosts } from "~/lib/linkHosts.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeSummary,
  CodeCountsAnswer,
  CodeRepoDetail,
  CodeTagRow,
} from "~/protocol/index.ts";
import { commitHref, repoHref } from "../paths.ts";
import { Note, PushActivity, Section, unreadLine, usePushActivity } from "../railParts.tsx";
import { readinessNext } from "../readinessWords.ts";
import { Face, useWhoName } from "../ui.tsx";
import { About } from "./About.tsx";
import { mirrorLink, placingWord } from "./rail.ts";
import "~/styles/gitlet.css";

/** The open changes the rail lists, open ones first, then drafts. */
const CHANGE_ROWS = 5;

export function RepoRail({
  repo,
  counts,
  docs,
  pollMs,
}: {
  repo: CodeRepoDetail;
  /** The page's `code_counts` read, which the Changes tab counts by too. */
  counts: QueryResult<CodeCountsAnswer>;
  /** The root's documents the README card holds, comma-separated. */
  docs: string;
  /** The page's cadence on the code log. */
  pollMs: number;
}) {
  const activity = usePushActivity(repo.name, pollMs);
  return (
    <div className="code-repo-rail">
      <About repo={repo} docs={docs} />
      <OpenChanges repo={repo} counts={counts} pollMs={pollMs} />
      <PushActivity activity={activity} />
      <MirrorCard repo={repo} />
      <LatestTag repo={repo} />
    </div>
  );
}

/** The live changes: the open ones by last move, then the drafts, at most CHANGE_ROWS. */
function OpenChanges({
  repo,
  counts,
  pollMs,
}: {
  repo: CodeRepoDetail;
  counts: QueryResult<CodeCountsAnswer>;
  pollMs: number;
}) {
  const open = useCodeQuery(
    "code_changes",
    { repo: repo.name, state: "open", sort: "updated", limit: CHANGE_ROWS },
    { pollMs },
  );
  const tally = counts.data?.counts?.changes;
  const openRows = open.data?.changes ?? [];
  // THE DRAFTS WHERE THE OPEN PAGE LEFT ROOM and the count says some are, or no count was read.
  const room = open.data && !open.error ? CHANGE_ROWS - openRows.length : 0;
  const askDrafts = room > 0 && (tally ? tally.draft > 0 : !counts.loading);
  const drafts = useCodeQuery(
    "code_changes",
    { repo: repo.name, state: "draft", sort: "updated", limit: room },
    { enabled: askDrafts, pollMs },
  );
  const rows = [...openRows, ...(askDrafts ? (drafts.data?.changes ?? []).slice(0, room) : [])];
  const draftsPending =
    room > 0 && ((!tally && counts.loading) || (askDrafts && drafts.loading && !drafts.data));
  const draftsLine =
    askDrafts && drafts.error && !drafts.data ? (
      <Note>{unreadLine(drafts, "Drafts are unavailable", "Drafts could not be read.")}</Note>
    ) : null;
  const end = (
    <a
      className="t-link"
      href={repoHref(repo.name, { tab: "changes" })}
      aria-label="View all changes"
    >
      View all
    </a>
  );
  return (
    <Section title="Open changes" count={tally?.live} end={end}>
      {open.loading && !open.data ? (
        <Skeleton variant="text" rows={3} label="Loading the open changes" />
      ) : open.error && !open.data ? (
        <Note>
          {unreadLine(open, "Open changes are unavailable", "Open changes could not be read.")}
        </Note>
      ) : rows.length === 0 && draftsPending ? (
        <Skeleton variant="text" rows={3} label="Loading the open changes" />
      ) : rows.length === 0 ? (
        (draftsLine ?? <Note>No change is open.</Note>)
      ) : (
        <>
          <ul className="code-repo-rail__list">
            {rows.map((c) => (
              <li key={c.id}>
                <MiniChange change={c} />
              </li>
            ))}
          </ul>
          {draftsLine}
        </>
      )}
    </Section>
  );
}

/** One change in a line: its state glyph, its title, and its reference, placing word and owner. */
function MiniChange({ change }: { change: CodeChangeSummary }) {
  const reader = useViewer().handle || "";
  const whoName = useWhoName();
  const placing = placingWord(change, reader);
  const Glyph = change.state === "draft" ? GitPullRequestDraftGlyph : GitPullRequestGlyph;
  return (
    <a className="code-mini" href={href(changePath(change.repo, change.number))}>
      <span className="code-mini__state" data-state={change.state}>
        <Glyph size="sm" />
      </span>
      <span className="code-mini__body">
        <span className="code-mini__title" title={change.title}>
          {change.title}
        </span>
        <span
          className="code-mini__meta"
          title={
            change.readiness
              ? readinessNext(change.readiness, { change, nameOf: (h) => whoName(h) })
              : undefined
          }
        >
          <span className="code-mini__place">
            <span className="mono">{change.ref}</span>
            <span aria-hidden="true">·</span>
            <span className="code-mini__word" data-tone={placing.tone}>
              {placing.word}
            </span>
          </span>
          <span className="code-mini__who">
            <Face who={change.owner} />
            <span className="code-mini__name">{whoName(change.owner)}</span>
          </span>
        </span>
      </span>
    </a>
  );
}

/** Where the repository is mirrored: its push mirror, then its pull mirror; none, no section. */
function MirrorCard({ repo }: { repo: CodeRepoDetail }) {
  const to = repo.remotes?.mirror_to;
  const from = repo.remotes?.mirror_from;
  const whoName = useWhoName();
  const now = useNow();
  if (!to && !from) return null;
  const synced = repo.remotes?.mirror_synced_at;
  return (
    <Section title="Mirror">
      {to && (
        <div className="code-repo-rail__mirror">
          <MirrorAddress url={to.url} />
          <p className="code-repo-rail__note">
            Publishes{" "}
            {to.refs?.length ? (
              to.refs.map((r, i) => (
                <span key={r}>
                  {i > 0 && " and "}
                  <strong className="mono">{r}</strong>
                </span>
              ))
            ) : (
              <>
                <strong>{repo.default_branch}</strong> and every tag
              </>
            )}
            , as{" "}
            <span className="code-repo-rail__as">
              <Face who={to.as} />
              <strong>{whoName(to.as)}</strong>
            </span>
            .
          </p>
        </div>
      )}
      {from && (
        <div className="code-repo-rail__mirror">
          <MirrorAddress url={from.url} />
          <p className="code-repo-rail__note">
            Follows it;{" "}
            {synced ? (
              <>
                last synced{" "}
                <time dateTime={synced} title={fmtDateTime(synced)}>
                  {ago(synced, now)}
                </time>
              </>
            ) : (
              "not synced yet"
            )}
            .
          </p>
        </div>
      )}
    </Section>
  );
}

/** A mirror's address: a link with its host's mark where the host is a vendor's, else its text. */
function MirrorAddress({ url }: { url: string }) {
  const hosts = useLinkHosts();
  const link = mirrorLink(url);
  if (!link.href) {
    return (
      <span className="code-repo-rail__address">
        <span className="code-repo-rail__link mono" title={link.text}>
          {link.text}
        </span>
      </span>
    );
  }
  const vendor = describeLink(link.href, hosts)?.vendor ?? null;
  return (
    <span className="code-repo-rail__address">
      {vendor && <VendorMark vendor={vendor} size="sm" data-vendor={vendor} />}
      <a
        className="t-link code-repo-rail__link"
        href={link.href}
        target="_blank"
        rel="noopener noreferrer"
        title={link.href}
      >
        {link.text}
      </a>
    </span>
  );
}

/** The newest tag, Latest, at its commit and when it was tagged; read again as the count moves. */
function LatestTag({ repo }: { repo: CodeRepoDetail }) {
  const read = useCodeQuery(
    "code_tags",
    useMemo(() => ({ name: repo.name, limit: 1 }), [repo.name]),
    { enabled: repo.tags > 0 },
  );
  const seen = useRef(repo.tags);
  const { refetch } = read;
  useEffect(() => {
    if (seen.current === repo.tags) return;
    seen.current = repo.tags;
    if (repo.tags > 0) refetch();
  }, [repo.tags, refetch]);
  const tag = read.data?.tags?.tags?.[0];
  const end = (
    <a
      className="t-link"
      href={repoHref(repo.name, { tab: "branches", view: "tags" })}
      aria-label="View all tags"
    >
      View all
    </a>
  );
  return (
    <Section title="Tags" count={read.data?.tags?.total ?? repo.tags} end={end}>
      {repo.tags === 0 ? (
        <Note>No tag yet.</Note>
      ) : read.loading && !read.data ? (
        <Skeleton variant="text" rows={1} label="Loading the newest tag" />
      ) : read.error && !read.data ? (
        <Note>{unreadLine(read, "Tags are unavailable", "Tags could not be read.")}</Note>
      ) : !tag ? (
        <Note>No tag is read yet.</Note>
      ) : (
        <NewestTag repo={repo.name} tag={tag} />
      )}
    </Section>
  );
}

function NewestTag({ repo, tag }: { repo: string; tag: CodeTagRow }) {
  const now = useNow();
  const name = refName(tag.ref).name;
  const id = shortOID(tag.peeled);
  return (
    <div className="code-repo-rail__tag">
      <span className="code-repo-rail__tag-name">
        <TagGlyph size="sm" className="code-repo-rail__glyph" />
        <a
          className="t-link code-repo-rail__link"
          href={repoHref(repo, { ref: tag.ref })}
          title={name}
        >
          <strong>{name}</strong>
        </a>
        <Tag appearance="outline" size="xs">
          Latest
        </Tag>
      </span>
      <p className="code-repo-rail__note">
        at{" "}
        {tag.peeled_type === "commit" ? (
          <a className="prose-link mono" href={commitHref(repo, tag.peeled)}>
            {id}
          </a>
        ) : (
          <span className="mono">{id}</span>
        )}
        ,{" "}
        {tag.date ? (
          <>
            tagged{" "}
            <time dateTime={tag.date} title={fmtDateTime(tag.date)}>
              {ago(tag.date, now)}
            </time>
          </>
        ) : tag.date_unix !== undefined ? (
          "tagged after the year 9999"
        ) : (
          <span title="A lightweight tag of a tree or a blob, or an annotated tag naming no tagger, has no date of its own.">
            undated
          </span>
        )}
      </p>
    </div>
  );
}
