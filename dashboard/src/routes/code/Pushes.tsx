/**
 * Code › Pushes: one repository's pushes, newest first — who pushed, what
 * became of its refs, what it brought, and the turn behind a seat's push.
 *
 * # One repository at a time
 *
 * The engine keeps a push history per repository (`code_pushes`, newest first
 * in the order the pushes landed on the code log) and no listing that spans
 * them: a company-wide log here would be every repository's history merged in
 * the browser, page by page, which could only ever be right for the pages it
 * happened to hold. So the section is narrowed to one repository, `repo=`,
 * chosen in its bar or arriving from a link, and a name no repository can
 * have is said in place rather than asked about (`lib/code.ts`). A
 * repository's own page draws the same history on its Pushes tab
 * ([PushHistory]).
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
 * # A push is drawn in brief, and read whole on demand
 *
 * A push carries up to a thousand ref updates (`code.MaxCommandsPerPush`, what
 * `crewlet code import` sends at a time), each a line, and the commits it
 * introduced, which this screen never draws. So the history is asked IN BRIEF
 * (`brief`): five updates of a push at most — its refused ones first, each
 * with its reason — and the rest COUNTED by what became of each, which the row
 * says; the whole push is one read of it away (`push`), made when the reader
 * asks for every ref. Read whole, a page of a freshly imported repository was
 * up to four mebibytes, sent again by every poll, and twenty thousand lines a
 * render.
 *
 * AND THE CLOCK RE-RENDERS ONE CELL, the time a push landed: the refs are the
 * tallest thing a row draws, memoised on the row, so a tick of the second
 * hand walks none of them.
 *
 * # What a row says is what the engine stamped
 *
 * Who pushed is the row's actor — a seat, or the credential a person pushed
 * through, with the seat it acts as — and a seat's turn, its work item and its
 * model are what the engine stamped from the call that made the push, never
 * what a commit says about itself.
 */

import { Fragment, memo, useId, useMemo, useState } from "react";
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
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import {
  commandWord,
  isRepoName,
  omittedOf,
  refName,
  shortOID,
  type OutcomeCount,
} from "~/lib/code.ts";
import { fmtBytes, fmtCount, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import {
  queryErrorCode,
  type CodePushCommand,
  type CodePushRow,
  type CodePushesAnswer,
  type CodeRepoSummary,
} from "~/protocol/index.ts";
import { spellPosition } from "~/protocol/session.ts";
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
  merge: "a change merged",
  expiry: "an idle seat branch expired",
  queue: "a merge queue landed it",
};

export function Pushes() {
  // THE REPOSITORY, by its name. A filter, so choosing another replaces the
  // history entry rather than adding one per repository looked at.
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const named = repo !== "" && isRepoName(repo);
  // THE REPOSITORIES THE READER MAY READ, for the bar's picker: the listing's
  // first page, by name, which every other one is a press of Repositories'
  // Load more away.
  const choices = useCodeQuery("code_repos");

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
      ) : (
        <PushHistory repo={repo} />
      )}
    </>
  );
}

/**
 * One repository's push history, newest first, a page at a time — what Code ›
 * Pushes draws for the repository its bar names, and a repository's own page
 * draws on its Pushes tab. `repo` is a name a repository can have
 * (`isRepoName`); the caller says in place what one that is not is.
 */
export function PushHistory({ repo }: { repo: string }) {
  const seatBadge = useSeatBadgeOf();
  // IN BRIEF, every page of it: see the file's doc.
  const params = useMemo(() => ({ name: repo, brief: true }), [repo]);
  // ANOTHER REPOSITORY IS ANOTHER HISTORY: its first page is never followed by
  // the last one's second, and the first page is held still from the press of
  // Load older (see the file's doc).
  const hold = usePageHold<CodePushesAnswer>(params);
  const first = useCodeQuery("code_pushes", params, {
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
      {first.error === "not_found" ? (
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
                  cell: (p) => <LandedAt at={p.at} />,
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
                  cell: (p) => <PushRefs push={p} repo={repo} />,
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
            <div className="row gap-2 wrap">
              {more ? (
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
              {/* BESIDE THE CONTROL, which asks the page again. */}
              {paged.pageError && <PageFailed error={paged.pageError} />}
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
      <span
        className="mono"
        title={
          push.actor_kind === "system"
            ? "The engine's own account"
            : "A credential bound to no seat"
        }
      >
        {push.actor}
      </span>
    );
  }
  // THE CREDENTIAL A PERSON PUSHED THROUGH is the record's own name for the
  // push; the seat beside it is who it acts as.
  const through = push.actor_kind === "agent" ? undefined : `through ${push.actor}`;
  return <TableEntityCell {...plainSeat({ ...seatBadge(seat) })} title={through} />;
}

/**
 * When a push landed, against the one clock — read HERE rather than by the
 * screen, so its tick re-renders this cell and nothing else of the grid.
 */
function LandedAt({ at }: { at: string }) {
  const now = useNow();
  return <TableDateCell value={at} now={now} />;
}

/** Where the whole of one push is: not asked yet, on its way, read, not to be had, or refused. */
type WholePush =
  | { state: "unread" }
  | { state: "reading" }
  | { state: "read"; commands: readonly CodePushCommand[] }
  | { state: "gone" }
  | { state: "failed"; error: QueryErrorCode };

const UNREAD: WholePush = { state: "unread" };

/**
 * A push's refs as its row draws them: those the history in brief carries —
 * its refused ones first, each with its reason — and how many it left out and
 * what became of them, with every ref one read of the push away. The push is
 * read once, the first time it is opened, and kept while the row is drawn.
 *
 * MEMOISED ON ITS ROW: the refs are the tallest thing a row draws, and only a
 * newer reading of the row changes them — never the clock, which the When
 * cell reads alone, nor a page landing below it.
 */
const PushRefs = memo(function PushRefs({ push, repo }: { push: CodePushRow; repo: string }) {
  const [whole, setWhole] = useState<WholePush>(UNREAD);
  const [open, setOpen] = useState(false);
  const listId = useId();
  const shown = push.commands ?? [];
  const omitted = omittedOf(push.omitted);
  if (omitted.total === 0) return <Commands commands={shown} />;

  const read = async () => {
    setWhole({ state: "reading" });
    try {
      const answer = await codeRest.query("code_pushes", {
        name: repo,
        push: push.push_id,
        // AT THE PUSH'S OWN POSITION, which every copy holding it has
        // reached: a copy behind the page this row came from waits for it,
        // rather than answering that the history holds no such push.
        read_level: "session",
        min_position: [spellPosition(push.position)],
      });
      const row = answer.pushes?.find((p) => p.push_id === push.push_id);
      setWhole(row ? { state: "read", commands: row.commands ?? [] } : { state: "gone" });
    } catch (err) {
      setWhole({
        state: "failed",
        error: queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed",
      });
    }
  };

  const showing = open && whole.state === "read";
  return (
    <span className="col gap-1">
      <span id={listId}>
        <Commands commands={showing ? whole.commands : shown} />
      </span>
      {!showing && <Omitted outcomes={omitted.outcomes} total={omitted.total} />}
      {whole.state === "gone" && (
        <span className="t-caption">The history no longer holds this push.</span>
      )}
      {/* SAID BESIDE THE CONTROL, which asks again: a push the reader asked
          for once is asked again by the reader alone, so no banner that
          promises a retry on its own. */}
      {whole.state === "failed" && (
        <span className="t-caption" role="alert">
          {`Could not read this push (${whole.error}). Try again.`}
        </span>
      )}
      {whole.state !== "gone" && (
        <span>
          <Button
            size="small"
            variant="ghost"
            aria-expanded={showing}
            aria-controls={listId}
            loading={whole.state === "reading"}
            disabled={whole.state === "reading"}
            onClick={() => {
              if (showing) {
                setOpen(false);
                return;
              }
              setOpen(true);
              if (whole.state !== "read") void read();
            }}
          >
            {showing ? "Show fewer" : `Show all ${plural(shown.length + omitted.total, "ref")}`}
          </Button>
        </span>
      )}
    </span>
  );
});

/**
 * What a push read in brief left out, in a line: how many, and what became of
 * them — the refusals first, in the tone every failure mark wears.
 */
function Omitted({ outcomes, total }: { outcomes: readonly OutcomeCount[]; total: number }) {
  return (
    <span className="t-caption">
      and {fmtCount(total)} more:{" "}
      {outcomes.map(({ outcome, count }, i) => (
        <Fragment key={outcome}>
          {i > 0 && ", "}
          {outcome === "refused" ? (
            <Tag variant="danger">{`${fmtCount(count)} refused`}</Tag>
          ) : (
            `${fmtCount(count)} ${outcome}`
          )}
        </Fragment>
      ))}
    </span>
  );
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
