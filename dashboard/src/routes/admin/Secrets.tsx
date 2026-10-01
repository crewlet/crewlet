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
} from "@crewlethq/ui";
import {
  AddGlyph,
  CloseGlyph,
  DatabaseGlyph,
  EditGlyph,
  KeyGlyph,
  ShieldGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { SecretDialog } from "./SecretDialog.tsx";
import { RemoveSecretDialog } from "./RemoveSecretDialog.tsx";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { useRestRead } from "~/lib/restRead.ts";
import { authorLabel, throughOf } from "~/lib/attribution.ts";
import { rest, RestError, restFailure } from "~/protocol/index.ts";
import type { ConfigReference, RestFailure, SecretRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * A failed read of the list as the pair [QueryState] is keyed on.
 *
 * THE BANNER IS A TABLE OVER `QueryErrorCode`, not a place to put a sentence:
 * this screen handed it prose, the lookup missed every time, and a 401 —
 * the refusal an unguarded operator actually hits here, since `/secrets` is
 * guarded reads included — rendered the red "a code this build does not know"
 * banner instead of the auth-gated one. The sentence even promised a button
 * that only exists inside the entry that was being skipped.
 *
 * EVERY OTHER CASE IS [restFailure]'s, the one reading every REST screen
 * shares: a read no answer from the engine came back to is `unanswered`, a
 * `503` the engine wrote is `unavailable` — it was drawn here as a fault on
 * the node, "its log says what went wrong", for a node only catching up — and
 * anything else is a fault. The one case that is this route's own is the
 * ENGINE's 404, which on the LIST route is the whole surface being
 * unregistered: `secretsapi.Routes` does that on a process that cannot reach
 * the fleet's coordination store. A gateway's 404 says nothing of the kind,
 * so it stays `unanswered`.
 */
function failureOf(err: unknown): RestFailure {
  if (err instanceof RestError && err.status === 404 && !err.unanswered) {
    return { error: "unknown_query", refusal: null };
  }
  return restFailure(err);
}

/**
 * How a read that did not answer is reported IN PROSE.
 *
 * Only the reference index needs this: its refusal is a caption under the
 * caution banner in the removal confirmation, where there is no `QueryState`
 * to key a code on and the engine's own words are what an operator acts on.
 * The list's refusal goes through [failureOf] instead — see there for why a
 * sentence must never reach the banner.
 */
function refusalSentence(err: unknown): string {
  if (!(err instanceof RestError)) return String(err);
  if (err.unauthorized) {
    // THE GRANT THE ENGINE NAMED, where it named one: a refusal on
    // authority carries the grants that would have admitted the caller, and
    // "needs an operator token" sent a signed-in person to find a token when
    // what they lacked was `config:read`.
    const grants = Array.isArray(err.body.grants)
      ? err.body.grants.filter((g): g is string => typeof g === "string")
      : [];
    return grants.length > 0
      ? `This needs ${grants.join(" or ")}, which the credential you presented does not carry.`
      : "This needs a credential that carries the grant to read it. Sign in as somebody who holds it.";
  }
  return err.detail || err.code || "the engine refused the read";
}

interface Credentials {
  /** Null until the first answer — never an empty list standing in for one. */
  rows: SecretRow[] | null;
  loading: boolean;
  /**
   * Why the list could not be read, in the terms [QueryState] renders from —
   * the code, and the refusal that lets the banner say what would change the
   * answer — or null. See [failureOf].
   */
  failure: RestFailure | null;
  /** Why the reference index is unknown, when it is. */
  unknown: string | null;
  /** The config fields naming one credential, or null where the check did not answer. */
  readersOf: (name: string) => string[] | null;
  /** Read both again, quietly: a write this screen made has landed. */
  reload: () => void;
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
 * EACH IS THE SHARED REST READ (`~/lib/restRead.ts`), which is what keeps the
 * banner true. Nothing polls this surface, so a read is asked again only when
 * something says to: a `503` the engine wrote — a node whose identity estate
 * or chart is behind, one draining — when its `Retry-After` says, and never on
 * a timer at its zero; a read no answer came back to on a backoff, because
 * the socket can be up the whole time (one request past its deadline on a slow
 * engine) and its coming back was the only thing that read this again; the
 * socket coming back; and a write this screen made. A failure with no hint (a
 * fault, a refusal on authority) waits for a person.
 *
 * `enabled` is the peek's guard: a rail opened on no name asks nothing, and a
 * rail CLOSED asks nothing more — not what an answer armed while it was open,
 * and not what an answer still in flight would arm when it lands, which a
 * `503` landing after the rail closed used to do for as long as the node
 * refused, with nothing on screen to read into.
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
  // THE LAST GOOD LIST STAYS. A refusal to refresh is not a reason to tell an
  // operator the company holds no credentials.
  const list = useRestRead(
    "/secrets",
    async (signal) =>
      ((await rest.get("/secrets", signal)) as { secrets?: SecretRow[] } | null)?.secrets ?? [],
    { enabled },
  );

  // THE REFERENCE INDEX IS THREE-VALUED, and collapsing it to two is the one
  // mistake this screen must not make: null means the question was not
  // answered — never that the answer was "nothing" — and the removal
  // confirmation branches on exactly that.
  const references = useRestRead(
    "/config/references",
    async (signal) => {
      try {
        return (
          (
            (await rest.get("/config/references", signal)) as {
              references?: ConfigReference[];
            } | null
          )?.references ?? []
        );
      } catch (err) {
        // A 404 IS AN ANSWER — the ENGINE's, never a gateway's. A deployment
        // before its first config import has no active document, so nothing
        // can be pointing at anything, and treating that as a failed check
        // would put a warning in front of every removal on a new install.
        if (err instanceof RestError && err.status === 404 && !err.unanswered) return [];
        throw err;
      }
    },
    { enabled },
  );
  // NOT THE LAST ANSWER, unlike the list: an index from before a check that
  // failed is the old answer to "what breaks if this goes", and drawn as the
  // current one it would let a removal through with no warning about a field
  // that names the row now.
  const index = references.failure ? null : references.data;

  // Grouped by name, because one credential routinely has several readers: a
  // seat's bot_token and its mcp_env entry are two pointers at one row, and
  // both have to be visible before it goes.
  const readers = useMemo(() => {
    if (index === null) return null;
    const byName = new Map<string, string[]>();
    for (const ref of index) {
      byName.set(ref.name, [...(byName.get(ref.name) ?? []), ref.path]);
    }
    return byName;
  }, [index]);

  const readersOf = useCallback(
    (name: string): string[] | null => readers?.get(name) ?? (readers ? [] : null),
    [readers],
  );

  const { refetch: rereadList } = list;
  const { refetch: rereadReferences } = references;
  const reload = useCallback(() => {
    rereadList();
    rereadReferences();
  }, [rereadList, rereadReferences]);

  return {
    rows: list.data,
    loading: list.loading,
    failure: list.failure ? failureOf(list.error) : null,
    unknown: references.failure ? refusalSentence(references.error) : null,
    readersOf,
    reload,
  };
}

/**
 * The facts a credential is recognised by, in the table's own column order.
 *
 * ONE FUNCTION for the page and the rail. The SOURCE is not among them — it is
 * the header's own pill — and neither is the value, which this surface does
 * not hold at all.
 */
function credentialFacts(row: SecretRow, paths: string[] | null, now: number): Fact[] {
  return [
    { label: "Read by", value: <Readers paths={paths} /> },
    { label: "Key id", value: <span className="mono">{row.key_id}</span> },
    {
      label: "Set by",
      value: authorLabel(row.updated_by, row.operator_id) || "nobody recorded",
    },
    { label: "Updated", value: <DateCell at={row.updated_at} now={now} /> },
  ];
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
              <span>
                The active configuration could not be read, so it is not known whether anything
                points at this name.
              </span>
              {unknown && <span className="t-caption">{unknown}</span>}
            </span>
          </Callout>
        ) : paths.length === 0 ? (
          <p className="t-body">
            No field in the active configuration names this. Removing it would change nothing the
            company is running.
          </p>
        ) : (
          <div className="col gap-2">
            <p className="t-body">
              {paths.length === 1
                ? "One field in the active configuration points at this name."
                : `${paths.length} fields in the active configuration point at this name.`}{" "}
              Removing it leaves each of them resolving to nothing, and the surfaces holding one
              start refusing deliveries.
            </p>
            <span className="col" style={{ gap: 2 }}>
              {paths.map((path) => (
                <InlineCode key={path}>{path}</InlineCode>
              ))}
            </span>
          </div>
        )}
      </Wrap>

      <Wrap title="Where it came from">
        <PropertiesRail
          groups={[
            {
              properties: [
                {
                  label: "Source",
                  value: row.source,
                  title:
                    "store: sealed in the fleet's coordination store. Anything else resolves from this process's environment.",
                },
                { label: "Key id", value: row.key_id, code: true },
                { label: "Set by", value: row.updated_by || undefined },
                {
                  label: "Through",
                  value: throughOf(row.updated_by, row.operator_id) || undefined,
                  code: true,
                  title: "the credential the value was stored through",
                },
                { label: "Updated", value: fmtDateTime(row.updated_at) },
              ],
            },
          ]}
        />
      </Wrap>

      {/* ONLY IN THE RAIL. The page carries this sentence in the banner above
          its table, and a reader who opened a credential in a peek has the
          same question with none of that on screen: where is the value. */}
      {flush && (
        <section className="col gap-2">
          <div className="t-label">The value</div>
          <p className="t-body">
            Never sent to this page, here or anywhere else in the dashboard. Reading one is{" "}
            <InlineCode>crewlet secrets get</InlineCode>, which logs the access.
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
  const now = useNow();
  const { rows, loading, failure, unknown, readersOf } = useCredentials(name !== "");
  const row = (rows ?? []).find((r) => r.name === name) ?? null;

  return (
    <>
      {loading && rows === null && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState
        error={failure?.error ?? null}
        refusal={failure?.refusal ?? null}
        loading={loading}
      >
        {/* NOT AN EMPTY RAIL. A name that matches no row is a hand-edited URL
            or a credential removed since the link was made, and naming the one
            that resolved to nothing is more use than a header over no row. */}
        {rows && !row && (
          <EmptyState
            size="compact"
            icon={<KeyGlyph size="xl" />}
            title={`No credential called “${name}”`}
            description="The fleet holds no row under this name. It may have been removed, or the name may be spelled differently in the config that points at it."
          />
        )}
        {row && (
          <>
            <ObjectHeader
              size="peek"
              kind="Credential"
              icon="key"
              title={row.name}
              status={<Tag appearance="outline">{row.source}</Tag>}
              facts={credentialFacts(row, readersOf(row.name), now)}
            />
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
  const { rows, loading, failure, unknown, readersOf, reload } = useCredentials();

  const [writing, setWriting] = useState<{ editing: string } | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);

  const list = useMemo(() => rows ?? [], [rows]);
  const fromStore = list.filter((r) => r.source === "store").length;

  // THE ORDER `[` AND `]` WALK, published from the rows this screen holds so
  // the stepper walks the list as the reader sorted it.
  usePeekNeighbours(
    useMemo(() => list.map((s) => ({ kind: "credential" as const, id: s.name })), [list]),
  );

  const { open: openPeek } = usePeekControls();

  /**
   * A row's click.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one copy
   * of which clicks mean elsewhere and reads a mouse event; the `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  const openCredential = useCallback(
    (row: SecretRow, e: React.MouseEvent | React.KeyboardEvent) => {
      const go = () => openPeek({ kind: "credential", id: row.name });
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
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

  // `#/admin/credentials/{name}` ADDRESSES ONE ROW, and this screen accepted
  // the segment and dropped it: a reader who followed a link to one credential
  // got the whole table with no sign of which one they had asked for — and it
  // is where `Open ↗` from the rail lands.
  const addressed = name ?? "";
  const addressedRow = addressed ? (list.find((s) => s.name === addressed) ?? null) : null;

  return (
    <>
      <PageActions>
        {<Tag appearance="outline">{plural(list.length, "credential")} held</Tag>}
        {
          <Button
            leadingIcon={<AddGlyph size="sm" />}
            variant="primary"
            onClick={() => setWriting({ editing: "" })}
          >
            Store a secret
          </Button>
        }
      </PageActions>
      <PageNote>
        The company's sealed credentials. Names, key ids and provenance — this screen never asks for
        a value.
      </PageNote>

      {/* THE SHIELD, NOT THE TONE'S OWN INFO MARK. This strip is about where
          the values live and who can read them, and the glyph is half the
          sentence. */}
      <Callout variant="neutral" icon={<ShieldGlyph size="md" />}>
        <span className="col" style={{ gap: 4 }}>
          <span>
            These live in the fleet's coordination store, sealed with the Tier A keyring, and every
            node reads them. A <InlineCode>${"{VAR}"}</InlineCode> in the company config resolves
            here first and falls back to the process environment.
          </span>
          <span className="t-caption">
            Values are never sent to this page. Reading one is{" "}
            <InlineCode>crewlet secrets get</InlineCode>, which logs the access. A new value reaches
            a running seat at the next configuration activation or restart.
          </span>
        </span>
      </Callout>

      <Card padding="none">
        <StatGroup columns={3}>
          <StatCard
            icon={<KeyGlyph size="xs" />}
            label="Credentials"
            value={list.length}
            sub="names the fleet holds"
          />
          <StatCard
            icon={<DatabaseGlyph size="xs" />}
            label="In the secret store"
            value={fromStore}
            sub="the rest resolve from this process's environment"
          />
          <StatCard
            icon={<ShieldGlyph size="xs" />}
            label="Distinct key ids"
            value={new Set(list.map((r) => r.key_id)).size}
            sub="a rekey moves every value onto a new one"
          />
        </StatGroup>
      </Card>

      {loading && rows === null && <Skeleton variant="text" rows={4} label="Loading" />}
      <QueryState
        error={failure?.error ?? null}
        refusal={failure?.refusal ?? null}
        loading={loading}
        empty={
          list.length
            ? undefined
            : {
                title: "No secrets are stored",
                hint: "Store one here, set one with crewlet secrets set, or let a provisioning command hand one straight to the engine.",
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
                sortValue: (s) => s.name,
                cell: (s) => <KeyCell value={s.name} />,
              },
              {
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
                sortValue: (s) => s.source,
                cell: (s) => <Tag appearance="outline">{s.source}</Tag>,
              },
              {
                key: "key",
                header: "Key id",
                shrink: true,
                sortValue: (s) => s.key_id,
                cell: (s) => <KeyCell value={s.key_id} />,
              },
              {
                key: "by",
                header: "Set by",
                sortValue: (s) => s.updated_by,
                // NOT `value || "—"`. An operator name is either recorded or
                // it is not, and the dash says which rather than standing in
                // for an empty string the reader would read as a name.
                cell: (s) =>
                  s.updated_by ? (
                    <TextCell>{authorLabel(s.updated_by, s.operator_id)}</TextCell>
                  ) : (
                    <EmptyValue label="Nobody recorded" />
                  ),
              },
              {
                key: "at",
                header: "Updated",
                shrink: true,
                sortValue: (s) => tsKey(s.updated_at),
                cell: (s) => <DateCell at={s.updated_at} now={now} />,
              },
              {
                key: "act",
                header: "",
                label: "Actions",
                shrink: true,
                cell: (s) => (
                  <span className="row gap-1">
                    <IconButton
                      size="sm"
                      variant="ghost"
                      icon={<EditGlyph size="sm" />}
                      label={`Edit ${s.name}`}
                      title={`Edit ${s.name}`}
                      onClick={rowAction(() => setWriting({ editing: s.name }))}
                    />
                    <IconButton
                      size="sm"
                      variant="ghost"
                      icon={<CloseGlyph size="sm" />}
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
          <ObjectHeader
            kind="Credential"
            icon="key"
            title={addressedRow.name}
            status={<Tag appearance="outline">{addressedRow.source}</Tag>}
            facts={credentialFacts(addressedRow, readersOf(addressedRow.name), now)}
          />
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
          description="The fleet holds no row under this name. It may have been removed since the link was made, or the name may be spelled differently in the config that points at it."
        />
      )}

      {writing && (
        <SecretDialog
          editing={writing.editing}
          paths={writing.editing ? readersOf(writing.editing) : null}
          onClose={() => setWriting(null)}
          onDone={(stored) => {
            toast.ok(writing.editing ? `Updated ${stored}` : `Stored ${stored}`);
            reload();
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
            reload();
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
