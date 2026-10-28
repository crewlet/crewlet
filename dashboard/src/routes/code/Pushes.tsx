/**
 * Code › Pushes: one repository's pushes, newest first — who pushed, what
 * became of each ref, what it brought, and the turn behind a seat's push.
 *
 * # One repository at a time
 *
 * The engine keeps a push history per repository (`code_pushes`, newest first
 * in the order the pushes landed on the code log) and no listing that spans
 * them: a company-wide log here would be every repository's history merged in
 * the browser, page by page, which could only ever be right for the pages it
 * happened to hold. So the section is narrowed to one repository, `repo=`,
 * chosen in its bar or arriving from a Repositories row, and a name no
 * repository can have is said in place rather than asked about
 * (`lib/code.ts`).
 *
 * # A log reaches its older rows by fetching them
 *
 * A page boundary is not a window, so a history that fills its page offers
 * Load older and holds the pages it has, discarding them together when the
 * repository changes. And from the press of Load older the first page is HELD
 * STILL — no poll, no read on the tab's return or the socket's — because the
 * cursor was minted from its last row: read again under newer pushes, it
 * would push that row out, and the pushes between the two pages would belong
 * to neither. The pager is the one every paged history shares
 * (`lib/usePaged.ts`).
 *
 * # What a row says is what the engine stamped
 *
 * Who pushed is the row's actor — a seat, or the credential a person pushed
 * through, with the seat it acts as — and a seat's turn, its work item and its
 * model are what the engine stamped from the call that made the push, never
 * what a commit says about itself.
 */

import { useMemo } from "react";
import {
  Button,
  Callout,
  EmptyState,
  Select,
  Skeleton,
  TableDateCell,
  TableEntityCell,
  TableNumberCell,
  TableTextCell,
  Tag,
} from "@crewlethq/ui";
import { UploadGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { commandWord, isRepoName, refName, shortOID } from "~/lib/code.ts";
import { fmtBytes, fmtCount, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type {
  CodePushCommand,
  CodePushRow,
  CodePushesAnswer,
  CodeRepoSummary,
} from "~/protocol/index.ts";
import { REPO_PARAM } from "./paths.ts";

/**
 * How often the newest page is read again while nothing older is held: a
 * MINUTE, the cadence this dashboard reads the other change log nothing
 * pushes to it at (Work's History). The code log has no push to the
 * dashboard, so another person's push reaches the open history by this read
 * or not at all.
 */
const PUSHES_POLL_MS = 60_000;

/** A history page's pushes, each push's id, and the cursor to the page after. */
const pushesOf = (a: CodePushesAnswer) => a.pushes ?? [];
const pushIdOf = (p: CodePushRow) => p.push_id;
const pushesAfter = (a: CodePushesAnswer) => a.next ?? "";

/** Every way a push's commit and pack came to be, in words (`code.PushVias`). */
const VIA_WORDS: Readonly<Record<string, string>> = {
  git: "git push",
  engine: "built by the engine",
};

export function Pushes() {
  const now = useNow();
  const seatBadge = useSeatBadgeOf();
  // THE REPOSITORY, by its name. A filter, so choosing another replaces the
  // history entry rather than adding one per repository looked at.
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const named = repo !== "" && isRepoName(repo);
  // THE REPOSITORIES THE READER MAY READ, for the bar's picker: the listing's
  // first page, by name, which every other one is a press of Repositories'
  // Load more away.
  const choices = useCodeQuery("code_repos");
  const params = useMemo(() => ({ name: repo }), [repo]);
  // ANOTHER REPOSITORY IS ANOTHER HISTORY: its first page is never followed by
  // the last one's second, and the first page is held still from the press of
  // Load older (see the file's doc).
  const hold = usePageHold<CodePushesAnswer>(params);
  const first = useCodeQuery("code_pushes", params, {
    enabled: named,
    pollMs: PUSHES_POLL_MS,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_pushes", { ...params, cursor }),
    pushesOf,
    pushIdOf,
    pushesAfter,
  );
  // ONE ROW PER PUSH across pages, by its id.
  const rows = paged.items;
  const more = paged.more;

  return (
    <>
      <div className="toolbar">
        <RepoPicker repo={repo} onChoose={setRepo} listing={choices.data?.repos ?? []} />
        {choices.data?.next && (
          <span className="t-caption">
            The first {plural(choices.data.repos?.length ?? 0, "repository", "repositories")} by
            name; open any other from{" "}
            <a className="prose-link" href={href(["code", "repos"])}>
              Repositories
            </a>
            .
          </span>
        )}
      </div>

      {repo === "" ? (
        <EmptyState
          icon={<UploadGlyph size={32} />}
          title="Choose a repository"
          description="A push history is kept with its repository: choose one above, or open one from Repositories."
        />
      ) : !named ? (
        <Callout variant="warning">
          “{repo}” is not a repository’s name: one is lowercase letters, digits and hyphens,
          beginning and ending with a letter or a digit, at most 63 in all.
        </Callout>
      ) : first.error === "not_found" ? (
        <EmptyState
          icon={<UploadGlyph size={32} />}
          title={`No repository named “${repo}”`}
          description={first.detail ?? "It is not declared, or it has been purged."}
        />
      ) : (
        <>
          {first.loading && rows.length === 0 && (
            <Skeleton variant="text" rows={6} label={`Loading the pushes to ${repo}`} />
          )}
          <QueryState
            error={first.error}
            refusal={first.refusal}
            detail={first.detail}
            loading={first.loading}
            empty={
              rows.length > 0 || more
                ? undefined
                : {
                    title: `Nobody has pushed to ${repo} yet`,
                    hint: "A push is listed once its record applies: the refs it moved, who pushed and what it brought.",
                  }
            }
          >
            <DataGrid<CodePushRow>
              rows={rows}
              rowKey={(p) => p.push_id}
              // NEWEST FIRST IN LOG ORDER, the engine's: a head that sorted the
              // pages loaded would answer a different question.
              serverSorted
              aria-label={`Pushes to ${repo}`}
              columns={[
                {
                  key: "at",
                  header: "When",
                  shrink: true,
                  cell: (p) => <TableDateCell value={p.at} now={now} />,
                },
                {
                  key: "actor",
                  header: "Pushed by",
                  shrink: true,
                  cell: (p) => <PushedBy push={p} seatBadge={seatBadge} />,
                },
                {
                  key: "refs",
                  header: "Refs",
                  floor: "14rem",
                  phoneLead: true,
                  cell: (p) => <Commands commands={p.commands ?? []} />,
                },
                {
                  key: "commits",
                  header: "Commits",
                  shrink: true,
                  align: "right",
                  // ABSENT IS NONE HERE: the count is omitted at zero, a push
                  // that only moved or removed refs.
                  cell: (p) => (
                    <TableNumberCell value={p.introduced_count ?? 0} format={fmtCount} />
                  ),
                },
                {
                  key: "turn",
                  header: "Turn",
                  shrink: true,
                  drop: 3,
                  cell: (p) => <RunCell push={p} />,
                },
                {
                  key: "via",
                  header: "How",
                  shrink: true,
                  drop: 1,
                  cell: (p) => <TableTextCell>{VIA_WORDS[p.via] ?? p.via}</TableTextCell>,
                },
                {
                  key: "size",
                  header: "Pack",
                  shrink: true,
                  align: "right",
                  drop: 2,
                  cell: (p) => (
                    <TableNumberCell
                      value={p.pack_bytes ?? null}
                      format={fmtBytes}
                      emptyLabel="No pack: nothing new to store"
                    />
                  ),
                },
              ]}
            />
          </QueryState>
          {(rows.length > 0 || more || paged.pageError) && (
            <div className="row gap-2">
              {paged.pageError ? (
                <QueryState error={paged.pageError} refusal={null} loading={false} />
              ) : more ? (
                <Button
                  size="small"
                  variant="secondary"
                  onClick={paged.older}
                  disabled={paged.paging}
                  loading={paged.paging}
                >
                  Load older
                </Button>
              ) : (
                <span className="t-caption">That is every push the repository keeps.</span>
              )}
              {rows.length > 0 && (
                <span className="t-caption">{plural(rows.length, "push", "pushes")} listed</span>
              )}
              {/* SAID ONLY ONCE IT IS TRUE: until then the first page is read
                  again every minute. */}
              {paged.held && <span className="t-caption">Paused while you page back.</span>}
            </div>
          )}
        </>
      )}
    </>
  );
}

/**
 * The repository the history is narrowed to, chosen from the ones the reader
 * may read. The chosen name stays an option where the listing's first page
 * does not hold it, so an address naming any repository reads as itself.
 */
function RepoPicker({
  repo,
  onChoose,
  listing,
}: {
  repo: string;
  onChoose: (name: string) => void;
  listing: readonly CodeRepoSummary[];
}) {
  const options = listing.map((r) => ({ value: r.name, label: r.name }));
  if (repo !== "" && !options.some((o) => o.value === repo)) {
    options.unshift({ value: repo, label: repo });
  }
  return (
    <Select
      width="auto"
      searchable
      value={repo}
      onChange={(value) => onChoose(String(value))}
      options={options}
      ariaLabel="Repository"
      placeholder="Choose a repository"
      emptyMessage="No repository matches"
      active={repo !== ""}
    />
  );
}

/**
 * Who pushed: the seat — a seat's own push, or the seat a person's credential
 * acts as — with the credential on the title; or, for a credential bound to no
 * seat, that credential.
 */
function PushedBy({
  push,
  seatBadge,
}: {
  push: CodePushRow;
  seatBadge: ReturnType<typeof useSeatBadgeOf>;
}) {
  const seat = push.actor_seat || (push.actor_kind === "agent" ? push.actor : "");
  if (!seat) {
    return (
      <span className="mono" title="A credential bound to no seat">
        {push.actor}
      </span>
    );
  }
  // THE CREDENTIAL A PERSON PUSHED THROUGH is the record's own name for the
  // push; the seat beside it is who it acts as.
  const through = push.actor_kind === "agent" ? undefined : `through ${push.actor}`;
  return <TableEntityCell {...plainSeat({ ...seatBadge(seat) })} title={through} />;
}

/** Every ref a push asked to move, and what became of each, one per line. */
function Commands({ commands }: { commands: readonly CodePushCommand[] }) {
  return (
    <span className="col">
      {commands.map((c) => {
        const ref = refName(c.ref);
        const word = commandWord(c);
        // THE IDS A REF MOVED BETWEEN, where it moved: the one it was made at,
        // the one it was removed from, or both.
        const ids =
          c.result !== "ok"
            ? null
            : word === "created"
              ? [c.new]
              : word === "deleted"
                ? [c.old]
                : [c.old, c.new];
        return (
          <span key={c.ref} className="row gap-1 nowrap" title={c.ref}>
            <span className="mono truncate">
              {ref.kind === "tag" ? `tag ${ref.name}` : ref.name}
            </span>
            {c.result === "ng" ? (
              // A REFUSAL IS THE DANGER TONE, the one every failure mark
              // wears, its reason the engine's own word.
              <Tag variant="danger" title={c.detail || c.reason || undefined}>
                {c.reason ? `refused: ${c.reason}` : word}
              </Tag>
            ) : (
              <span className="muted">{word}</span>
            )}
            {ids && (
              <span className="mono muted" title={ids.join("..")}>
                {ids.map(shortOID).join("..")}
              </span>
            )}
          </span>
        );
      })}
    </span>
  );
}

/**
 * A seat's push: the turn that made it and the work item it is charged to, as
 * the engine stamped them; nothing for a person's, which carries none.
 *
 * THE WORK ITEM IS ITS KEY, NOT A LINK. The stamp names the item by the key its
 * tracker showed and by no id, and a task is addressed by its key only where
 * no other task claimed that key first (`lib/work.ts`'s `itemAddress`), which
 * nothing here can tell. The turn's own page names the item as its record
 * does, one press away.
 */
function RunCell({ push }: { push: CodePushRow }) {
  const run = push.run;
  if (!run?.turn_id) return null;
  return (
    <span className="row gap-1 nowrap" title={run.model ? `Model ${run.model}` : undefined}>
      {run.work_key && (
        <span className="mono t-caption item-key" title={run.work_backend}>
          {run.work_key}
        </span>
      )}
      <a className="t-link" href={href(["live", "turns", run.turn_id])}>
        turn →
      </a>
    </span>
  );
}
