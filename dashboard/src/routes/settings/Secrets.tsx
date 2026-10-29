/**
 * The company's credentials — names, provenance and what reads them, never
 * values.
 *
 * This surface existed on the engine and had ZERO references in the previous
 * dashboard: `/secrets` was reachable, guarded, and unreachable from any
 * screen. Reads are guarded here too, deliberately — the list of what a
 * company holds a credential for is itself worth guarding.
 *
 * There is no reveal button. The one route that returns a value needs an
 * explicit flag and logs the access, and putting that behind a click in a
 * dashboard that anyone signed in can open is not a trade worth making;
 * `crewlet secrets get` is the deliberate path. Storing, editing and removing
 * a row need no such trade: none of them reads a value back. THE PEEK INHERITS
 * THAT and may never grow one either — a rail is a smaller screen, not a
 * looser one.
 *
 * WHAT READS A NAME IS PART OF THE LIST, and it is the reason this screen
 * makes a second request. The company configuration keeps `${VAR}` POINTERS,
 * so removing or renaming a row fails nowhere: each pointer at it resolves to
 * the empty string at the next activation, and the surfaces holding one start
 * refusing deliveries with nothing naming the row that went away. The engine
 * answers that question at `GET /config/references` rather than the client
 * deriving it, because deriving it means a second copy of the ${VAR} grammar
 * and the two the engine already retired had both drifted into bugs.
 */

import { useCallback, useMemo, useState } from "react";
import {
  Button,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  IconButton,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useToast,
  TableKeyCell,
  TableDateCell,
  TableTextCell,
  type DataGridActivationEvent,
  peekClickHandler,
} from "@crewlethq/ui";
import {
  PlusGlyph,
  XGlyph,
  LinkGlyph,
  PencilGlyph,
  KeyGlyph,
  ShieldGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";

import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { SecretDialog } from "./SecretDialog.tsx";
import { RemoveSecretDialog } from "./RemoveSecretDialog.tsx";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { authorLabel, throughOf } from "~/lib/attribution.ts";
import { needsWords } from "~/lib/access.ts";
import { isAbort, rest, RestError } from "~/protocol/index.ts";
import { restErrorCode, useRest } from "~/lib/useRest.ts";
import type { ConfigReference, LogRefusal, QueryRefusal, SecretRow } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";

/**
 * A refusal as the code [QueryState] renders, with this surface's one 404.
 *
 * The loader maps every status (`restErrorCode`); what it cannot know is that
 * a 404 on THIS list route is the whole surface being unregistered —
 * `secretsapi.Routes` does that on a process that cannot reach the fleet's
 * coordination store — rather than a record that is not there.
 */
function refusalCode(err: RestError | null): QueryErrorCode | null {
  if (err?.status === 404) return "unknown_query";
  return restErrorCode(err);
}

/**
 * How a read that did not answer is reported IN PROSE.
 *
 * Only the reference index needs this: its refusal is a caption under the
 * caution banner in the removal confirmation, where there is no `QueryState`
 * to key a code on and the engine's own words are what an operator acts on.
 * The list's refusal goes through [refusalCode] instead — see there for why a
 * sentence must never reach the banner.
 */
function refusal(err: unknown): string {
  if (!(err instanceof RestError)) return String(err);
  if (err.unauthorized) {
    // THE GRANT THE REFUSAL NAMED: "needs an operator token" sent a
    // signed-in person to find a credential they already held.
    return needsWords("This", err.grants);
  }
  return err.detail || err.code || "the engine refused the read";
}

interface Credentials {
  /** Null until the first answer — never an empty list standing in for one. */
  rows: SecretRow[] | null;
  loading: boolean;
  /** The refusal as a machine code, which is what [QueryState] renders from. */
  error: QueryErrorCode | null;
  /**
   * What the refusal said beyond its code — the grants that would have
   * admitted the reader, or the state log's refusal — for [QueryState].
   */
  refusal: QueryRefusal | LogRefusal | null;
  /** Why the reference index is unknown, when it is. */
  unknown: string | null;
  /** The config fields naming one credential, or null where the check did not answer. */
  readersOf: (name: string) => string[] | null;
  reload: () => Promise<void>;
}

/**
 * The two reads this surface is made of, as one hook.
 *
 * ONE COPY, because the screen and the rail both need them and a second
 * spelling of "what does `/secrets` answer" is a second spelling of what a
 * refusal MEANS — which is the distinction the whole file turns on. The rail
 * is mounted by the shell and cannot reach the screen's state even when the
 * two are on top of each other, so sharing has to be the hook rather than a
 * prop.
 *
 * `enabled` is the peek's guard: a rail opened on no name asks nothing.
 */
function useCredentials(enabled = true): Credentials {
  // GET /secrets, over REST, because no question in the registry answers it.
  //
  // This screen asked `config_entities {kind: "secrets"}`, and that kind does
  // not exist: the entity kinds are roles, units, llm-providers and
  // mcp-servers, so the answer was always an ErrUnknownEntityKind folded to
  // a bad-params error and the table could never hold a row. The socket is
  // still the data channel for everything it answers; this surface is simply
  // not one of them.
  //
  // ONE READ FOR THE PAIR, because the list and its reference index are one
  // refresh: two loaders would let the second answer land beside a first
  // that a newer refresh already superseded.
  const answer = useRest(enabled ? "/secrets+/config/references" : null, async (signal) => {
    const [list, index] = await Promise.all([
      rest.get("/secrets", signal) as Promise<{ secrets?: SecretRow[] } | null>,
      readReferences(signal),
    ]);
    return { rows: list?.secrets ?? [], ...index };
  });

  // Grouped by name, because one credential routinely has several readers: a
  // seat's bot_token and its mcp_env entry are two pointers at one row, and
  // both have to be visible before it goes.
  const references = answer.data?.references ?? null;
  const readers = useMemo(() => {
    if (references === null) return null;
    const byName = new Map<string, string[]>();
    for (const ref of references) {
      byName.set(ref.name, [...(byName.get(ref.name) ?? []), ref.path]);
    }
    return byName;
  }, [references]);

  const readersOf = useCallback(
    (name: string): string[] | null => readers?.get(name) ?? (readers ? [] : null),
    [readers],
  );

  const { reload } = answer;
  return {
    rows: answer.data?.rows ?? null,
    loading: answer.loading,
    error: refusalCode(answer.error),
    refusal: answer.refusal,
    unknown: answer.data?.unknown ?? null,
    readersOf,
    reload: useCallback(() => reload(), [reload]),
  };
}

/**
 * The reference index, THREE-VALUED — and collapsing it to two is the one
 * mistake this screen must not make. `references` null means the question
 * was not answered, never that the answer was "nothing", and the removal
 * confirmation branches on exactly that. So its refusal is caught HERE rather
 * than failing the pair: a list that answered is still the list, and only the
 * check beside each row becomes "unknown".
 */
async function readReferences(
  signal: AbortSignal,
): Promise<{ references: ConfigReference[] | null; unknown: string | null }> {
  try {
    const body = (await rest.get("/config/references", signal)) as {
      references?: ConfigReference[];
    } | null;
    return { references: body?.references ?? [], unknown: null };
  } catch (err) {
    // The pair's own abort is the loader's to recognise, not an answer.
    if (isAbort(err)) throw err;
    // A 404 IS AN ANSWER. A deployment before its first config import has
    // no active document, so nothing can be pointing at anything, and
    // treating that as a failed check would put a warning in front of
    // every removal on a new install.
    if (err instanceof RestError && err.status === 404) {
      return { references: [], unknown: null };
    }
    return { references: null, unknown: refusal(err) };
  }
}

/**
 * What each provenance means, in the words of the path that writes it.
 *
 * `source` IS WHICH PATH LAST WROTE THE ROW, never where the value lives: every
 * row `/secrets` lists is sealed in the fleet's store whatever wrote it. Each
 * path stamps its own word and no request can name one (internal/secrets'
 * `Source`): `PUT /secrets/{name}` stamps `api` whoever calls it — this
 * screen, a `crewlet` command through a running node, any client — an
 * integration's setup `setup`, and an integration's provisioning pass
 * `provision`. A `crewlet` command run while its node's engine is stopped
 * writes the store directly and keeps its own word: `cli` for
 * `crewlet secrets set`, `llm-login` for a coding CLI's captured token or
 * exported login, `provision` for a provisioning run.
 *
 * `iam` marks the engine's own keys, which the listing never names, and a
 * rekey keeps the word it found. A word outside the table is a newer engine's,
 * said as one this build does not know rather than guessed at.
 */
const PROVENANCE: Record<string, string> = {
  api: "stored through the API: this console, the CLI or another client",
  setup: "written by an integration's setup",
  provision: "minted by an integration's provisioning pass",
  cli: "set with crewlet secrets set while its node's engine was stopped",
  "llm-login": "a coding CLI's login, stored by crewlet llm while its node's engine was stopped",
  models:
    "a model's credential, stored from Settings › Models once it signed the model in on a test call",
};

/**
 * A row's provenance in a word or two, for the list's Source column. Quiet
 * words, never a pill: a pill on every row of a list whose rows mostly share
 * one source was a column a reader learned to skip. The sentence is on the
 * pointer and in the peek.
 */
const SOURCE_WORDS: Record<string, string> = {
  api: "API",
  setup: "Integration setup",
  provision: "Provisioning pass",
  cli: "CLI",
  "llm-login": "CLI login",
  models: "Models",
};

/** A source in words, or the engine's own word for one this build does not know. */
export function sourceWords(source: string): string {
  return SOURCE_WORDS[source] ?? source;
}

/** What a row's provenance says, as a sentence a reader can act on. */
export function provenance(source: string): string {
  return PROVENANCE[source] ?? `a source this build does not know (“${source}”)`;
}

/**
 * One credential's provenance, and what would break if it went.
 *
 * `flush` is the peek: the rail is the panel already, so the sections run on
 * as plain blocks rather than growing a second frame inside the first — the
 * same split `ChannelBody` makes between a channel's page and its peek.
 */
function CredentialBody({
  row,
  paths,
  unknown,
  flush,
}: {
  row: SecretRow;
  paths: string[] | null;
  unknown: string | null;
  flush?: boolean;
}) {
  const Wrap = flush
    ? ({ title, children }: { title: string; children: React.ReactNode }) => (
        <section className="col gap-2">
          <div className="t-label">{title}</div>
          {children}
        </section>
      )
    : ({ title, children }: { title: string; children: React.ReactNode }) => (
        <Card>
          <Card.Header>{title}</Card.Header>
          {children}
        </Card>
      );

  return (
    <>
      <Wrap title="What reads it">
        {/* THE THREE-VALUED ANSWER, laid out rather than counted. The table
            can only afford the count and hangs the paths off a title; this is
            where there is room for the list, and it is the list an operator
            needs before they touch the row. */}
        {paths === null ? (
          <Callout variant="warning">
            <span className="col" style={{ gap: 4 }}>
              <span>Couldn’t check whether anything uses it.</span>
              {unknown && <span className="t-caption">{unknown}</span>}
            </span>
          </Callout>
        ) : paths.length === 0 ? (
          <p className="t-body">Nothing uses it.</p>
        ) : (
          <div className="col gap-2">
            <p className="t-body">
              {paths.length === 1
                ? "One field uses it. Removing it breaks that field."
                : `${paths.length} fields use it. Removing it breaks them.`}
            </p>
            <span className="col" style={{ gap: 2 }}>
              {paths.map((path) => (
                <InlineCode key={path}>{path}</InlineCode>
              ))}
            </span>
          </div>
        )}
      </Wrap>

      {/* EVERY PROPERTY ONCE. The header above passes no facts: stacked in
          one column, a fact line and this group were the same four answers
          twice before the reader reached the value's note. */}
      <Wrap title="Where it came from">
        <PropertiesRail
          groups={[
            {
              properties: [
                {
                  label: "Source",
                  // THE WORD AND WHAT IT MEANS, in the body rather than a
                  // title: which path wrote a credential is the question this
                  // row answers, and a hover is not a place a keyboard or a
                  // phone reaches.
                  value: (
                    <span className="col" style={{ gap: 2 }}>
                      <span>{sourceWords(row.source)}</span>
                      <span className="t-caption">{provenance(row.source)}</span>
                    </span>
                  ),
                },
                { label: "Key id", value: <span className="mono">{row.key_id}</span> },
                { label: "Set by", value: row.updated_by || undefined },
                {
                  // THE CREDENTIAL IT WAS STORED THROUGH, beside the author
                  // rather than folded into the name: a machine token acting
                  // as its owner writes under the owner's login, and which of
                  // their credentials stored a value is the question an
                  // incident asks of this row.
                  label: "Through",
                  value: throughOf(row.updated_by, row.operator_id) ? (
                    <span className="mono">{throughOf(row.updated_by, row.operator_id)}</span>
                  ) : undefined,
                },
                { label: "Updated", value: fmtDateTime(row.updated_at) },
              ],
            },
          ]}
        />
      </Wrap>

      {/* IN THE RAIL: a reader who opened a credential in a peek asks where
          the value is, and the rail is all they have on screen. */}
      {flush && (
        <section className="col gap-2">
          <div className="t-label">The value</div>
          <p className="t-body">
            Values aren’t shown. Use <InlineCode>crewlet secrets get</InlineCode> to read one.
          </p>
        </section>
      )}
    </>
  );
}

/**
 * One credential, beside the list it was found in.
 *
 * It asks the same two questions the screen does — the names and the reference
 * index — because a `peek=credential:` arrives from a pasted URL as often as
 * from a row, and neither answer is pushed. What it adds over the row is the
 * reference LIST: the table has room for a count and a tooltip, and the list
 * is what somebody deciding about a name actually reads.
 */
export function CredentialPeek({ name }: { name: string }) {
  const { rows, loading, error, refusal, unknown, readersOf } = useCredentials(name !== "");
  const row = (rows ?? []).find((r) => r.name === name) ?? null;

  return (
    <>
      {loading && rows === null && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
        {/* NOT AN EMPTY RAIL. A name that matches no row is a hand-edited URL
            or a credential removed since the link was made, and naming the one
            that resolved to nothing is more use than a header over no row. */}
        {rows && !row && (
          <EmptyState
            size="compact"
            icon={<KeyGlyph size="xl" />}
            title={`No credential called “${name}”`}
            description="It may have been removed, or the name is misspelled."
          />
        )}
        {row && (
          <>
            <ObjectHeader size="peek" kind="Credential" icon="key" title={row.name} />
            <div className="col gap-3">
              <CredentialBody row={row} paths={readersOf(row.name)} unknown={unknown} flush />
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

export function Secrets({ name }: { name?: string }) {
  const now = useNow();
  const toast = useToast();
  const { rows, loading, error, refusal, unknown, readersOf, reload } = useCredentials();

  const [writing, setWriting] = useState<{ editing: string } | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);

  const list = useMemo(() => rows ?? [], [rows]);
  // THE NAMES NO FIELD POINTS AT, or null where the reference check did not
  // answer — a count of "nothing reads these" from a check that never ran
  // would be the one figure on this screen that is a guess.
  const unread = list.some((r) => readersOf(r.name) === null)
    ? null
    : list.filter((r) => readersOf(r.name)?.length === 0).length;

  // THE ORDER `[` AND `]` WALK, published from the rows this screen holds so
  // the stepper walks the list as the reader sorted it.
  usePeekNeighbours(
    useMemo(() => list.map((s) => ({ kind: "credential" as const, id: s.name })), [list]),
  );

  const { open: openPeek } = usePeekControls();

  /**
   * A row's click.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `peekClickHandler` is the frame's one copy
   * of which clicks mean elsewhere and reads a mouse event; the `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  const openCredential = useCallback(
    (row: SecretRow, e: DataGridActivationEvent) => {
      const go = () => openPeek({ kind: "credential", id: row.name });
      if (!("button" in e)) {
        go();
        return;
      }
      peekClickHandler(go)?.(e);
    },
    [openPeek],
  );

  /**
   * A row action, inside a row that is a link.
   *
   * THE DEFAULT ACTION OF THE ROW IS NOT THIS BUTTON'S. Edit and Remove sit
   * inside the anchor each row now is, so without this an operator aiming at
   * the X would also peek the row it belongs to — and the browser would follow
   * the href on its way past. `Button` passes the event through for exactly
   * this case; see its own comment.
   */
  function rowAction(run: () => void): (e: React.MouseEvent) => void {
    return (e) => {
      e.preventDefault();
      e.stopPropagation();
      run();
    };
  }

  // `#/settings/secrets/{name}` ADDRESSES ONE ROW, and this screen accepted
  // the segment and dropped it: a reader who followed a link to one credential
  // got the whole table with no sign of which one they had asked for — and it
  // is where `Open ↗` from the rail lands.
  const addressed = name ?? "";
  const addressedRow = addressed ? (list.find((s) => s.name === addressed) ?? null) : null;

  return (
    <>
      <PageActions>
        {/* NO COUNT HERE: the Credentials tile below says how many are held,
            and a chip in the bar repeated it an inch above. */}
        <Button
          leadingIcon={<PlusGlyph size="sm" />}
          variant="primary"
          size="small"
          onClick={() => setWriting({ editing: "" })}
        >
          Store a secret
        </Button>
      </PageActions>

      {rows !== null && (
        <Card padding="none">
          <StatGroup columns={3}>
            <StatCard
              icon={<KeyGlyph size="xs" />}
              label="Credentials"
              value={list.length}
              sub="names the fleet holds"
            />
            {/* NOT WHERE THE VALUES LIVE. This tile once counted rows whose
                source read "store" — a word no writer stamps, since `source`
                is which path wrote a row and every row listed here is sealed
                in the store — so six sealed credentials read "0, the rest
                resolve from this process's environment" under a banner
                saying they live in the store. What reads a name is the fact
                this screen is scanned for, so its tile is the names nothing
                reads: each one is either a forgotten `${VAR}` or a
                credential nothing needs any more. */}
            <StatCard
              icon={<LinkGlyph size="xs" />}
              label="Read by nothing"
              value={unread ?? <EmptyValue label="Not known" />}
              sub={
                unread === null
                  ? "the reference check did not answer"
                  : "no config field names them"
              }
            />
            <StatCard
              icon={<ShieldGlyph size="xs" />}
              label="Distinct key ids"
              value={new Set(list.map((r) => r.key_id)).size}
              sub="a rekey moves every value onto a new one"
            />
          </StatGroup>
        </Card>
      )}

      {loading && rows === null && <Skeleton variant="text" rows={4} label="Loading" />}
      <QueryState
        error={error}
        refusal={refusal}
        loading={loading}
        empty={
          list.length
            ? undefined
            : {
                title: "No secrets are stored",
                // A COMMAND IS CODE: in prose it read as three words of the
                // sentence.
                hint: (
                  <>
                    Add one here or with <InlineCode>crewlet secrets set</InlineCode>.
                  </>
                ),
              }
        }
      >
        <Card padding="none">
          <DataGrid<SecretRow>
            rows={list}
            rowKey={(s) => s.name}
            // A ROW IS A REAL LINK AND A PLAIN CLICK PEEKS. The href is the
            // frame's own answer to where a credential lives, so a row's
            // target and the rail's `Open ↗` can never name different pages.
            rowHref={(s) => peekHref({ kind: "credential", id: s.name })}
            onRowActivate={openCredential}
            isSelected={(s) => s.name === addressed}
            defaultSort="name"
            columns={[
              {
                key: "name",
                header: "Name",
                // THE ONE FLEXIBLE TRACK, as the node is on Nodes and the seat
                // in each lease table: the row exists to name a credential,
                // so the name takes whatever width is spare and every fact
                // beside it sizes to its content. As one of two `1fr` columns
                // it split the spare width with Set by — at a 1280 window
                // "DATADOG_WEBHOOK_TOK…" sat beside "founder" in ~120px of
                // air, and beside a peek at 1440 every name fell to "DAT…".
                //
                // AND IT NEVER CUTS: the floor is its content. A credential's
                // name is the one thing a reader matches against the `${VAR}`
                // in their configuration, so where the row cannot hold every
                // fact, facts give way (`drop`) rather than the name — each
                // one a fact the credential's peek carries.
                floor: "max-content",
                sortValue: (s) => s.name,
                cell: (s) => <TableKeyCell value={s.name} />,
              },
              {
                // NEVER GIVES WAY: what reads a name is what breaks when it
                // goes, which is the question this list is scanned for.
                key: "read",
                header: "Read by",
                shrink: true,
                // AN UNANSWERED CHECK SORTS AS ITS OWN THING, below every
                // count: -1 rather than 0, so "not known" never sits among
                // the rows nothing reads.
                sortValue: (s) => readersOf(s.name)?.length ?? -1,
                cell: (s) => <Readers paths={readersOf(s.name)} />,
              },
              {
                key: "source",
                header: "Source",
                shrink: true,
                // The peek's own pill.
                drop: 3,
                sortValue: (s) => s.source,
                // Which path wrote it, said in full on the pointer; the peek
                // says it in words.
                cell: (s) => (
                  <span title={provenance(s.source)}>
                    <TableTextCell>{sourceWords(s.source)}</TableTextCell>
                  </span>
                ),
              },
              {
                key: "key",
                header: "Key id",
                shrink: true,
                // First to go: one id covers every row until a rekey, so the
                // column repeats itself down the list.
                drop: 1,
                sortValue: (s) => s.key_id,
                cell: (s) => <TableKeyCell value={s.key_id} />,
              },
              {
                key: "by",
                header: "Set by",
                shrink: true,
                drop: 2,
                sortValue: (s) => s.updated_by,
                // NOT `value || "—"`. An operator name is either recorded or
                // it is not, and the dash says which rather than standing in
                // for an empty string the reader would read as a name.
                cell: (s) =>
                  s.updated_by ? (
                    <TableTextCell>{authorLabel(s.updated_by, s.operator_id)}</TableTextCell>
                  ) : (
                    <EmptyValue label="Nobody recorded" />
                  ),
              },
              {
                key: "at",
                align: "right",
                header: "Updated",
                shrink: true,
                drop: 4,
                sortValue: (s) => tsKey(s.updated_at),
                cell: (s) => <TableDateCell value={s.updated_at} now={now} />,
              },
              {
                key: "act",
                header: "",
                action: true,
                shrink: true,
                cell: (s) => (
                  <span className="row gap-1">
                    <IconButton
                      size="sm"
                      variant="ghost"
                      icon={<PencilGlyph size="sm" />}
                      label={`Edit ${s.name}`}
                      title={`Edit ${s.name}`}
                      onClick={rowAction(() => setWriting({ editing: s.name }))}
                    />
                    <IconButton
                      size="sm"
                      variant="ghost"
                      icon={<XGlyph size="sm" />}
                      label={`Remove ${s.name}`}
                      title={`Remove ${s.name}`}
                      onClick={rowAction(() => setRemoving(s.name))}
                    />
                  </span>
                ),
              },
            ]}
          />
        </Card>
      </QueryState>

      {/* THE CREDENTIAL THIS PATH NAMES. Rendered under the table rather than
          in place of it, because the table is what a reader arriving from a
          link needs to put the one row in context — and because a name that
          matches nothing has to say so rather than leaving the screen looking
          like an ordinary listing. */}
      {addressed && addressedRow && (
        <>
          <ObjectHeader kind="Credential" icon="key" title={addressedRow.name} />
          <CredentialBody
            row={addressedRow}
            paths={readersOf(addressedRow.name)}
            unknown={unknown}
          />
        </>
      )}
      {addressed && !addressedRow && rows && (
        <EmptyState
          icon={<KeyGlyph size="xl" />}
          title={`No credential called “${addressed}”`}
          description="It may have been removed, or the name is misspelled."
        />
      )}

      {writing && (
        <SecretDialog
          editing={writing.editing}
          paths={writing.editing ? readersOf(writing.editing) : null}
          onClose={() => setWriting(null)}
          onDone={(stored) => {
            toast.ok(writing.editing ? `Updated ${stored}` : `Stored ${stored}`);
            void reload();
          }}
        />
      )}

      {removing && (
        <RemoveSecretDialog
          name={removing}
          paths={readersOf(removing)}
          unknown={unknown}
          onClose={() => setRemoving(null)}
          onDone={() => {
            toast.ok(`Removed ${removing}`);
            void reload();
          }}
        />
      )}
    </>
  );
}

/**
 * How many config fields read this name, and which.
 *
 * The paths ride the title rather than the cell: a name with four readers
 * would make the row four lines tall in a table whose job is to be scanned,
 * and the peek and the removal confirmation are where the list has to be read
 * anyway.
 */
function Readers({ paths }: { paths: string[] | null }) {
  if (paths === null) {
    return (
      <span className="muted" title="The active configuration could not be read.">
        not known
      </span>
    );
  }
  if (paths.length === 0) {
    return <EmptyValue label="No field in the active configuration names it" />;
  }
  return (
    <Tag appearance="outline" title={paths.join("\n")}>
      {plural(paths.length, "field")}
    </Tag>
  );
}
