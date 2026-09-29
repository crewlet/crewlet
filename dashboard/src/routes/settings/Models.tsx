/**
 * Settings › Models & keys: every model the company configures, the keys each
 * one rotates through, which of those a vendor is refusing right now, and which
 * seats would feel it.
 *
 * # One answer for the state, the chart for the reach
 *
 * `credential_pool` is the engine's: each key's provenance as the answering
 * node resolved it, beside its pool and the FLEET's cooldown ledger, the later
 * deadline winning — so a key a peer benched a second ago reads cooling here,
 * not ready. Which seats run on a model is the chain the engine resolved for
 * each seat, pushed on the org projection. Neither is worked out again here.
 *
 * # Names, never values
 *
 * A key is the variable it names, the vendor's conventional variable, or its
 * position when it is written into the document; the answer has no member a
 * value could travel in, and neither does this screen. The edit is one entity
 * write (`PUT /config/llm-providers/{id}`) of what the configuration already
 * serves redacted: a key written inline goes back as the mask, and the engine
 * restores it from the revision it was read from.
 *
 * Operator-only, like the rest of the column: which variable holds each
 * model's key, and when each is benched, is a map of which credential to take.
 */

import { Fragment, useMemo, useState } from "react";
import {
  Button,
  Callout,
  Card,
  EmptyState,
  EMPTY_VALUE,
  EmptyValue,
  IconButton,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import { ClockGlyph, CpuGlyph, KeyGlyph, PencilGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href } from "~/app/router.tsx";
import { usePageMenu } from "~/app/Shell.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtTime, inTimeExact, plural } from "~/lib/format.ts";
import {
  benchParts,
  KEY_STATE_WORDS,
  KEY_SOURCE_WORDS,
  keyBackWords,
  keyMark,
  keyName,
  keyWhy,
  modelLine,
  needsAttention,
  nextLift,
  POOL_STATE_WORDS,
  usesOf,
  type ModelUse,
} from "~/lib/models.ts";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import type {
  CredentialKeyRow,
  CredentialPoolAnswer,
  CredentialPoolRow,
} from "~/contract/credentials.ts";
import { EditModelDialog } from "./EditModelDialog.tsx";

/**
 * How often the answer is asked again, in ms.
 *
 * The node's own cooldown refresher pulls the fleet's ledger every 15 s
 * (`internal/engine/cooldowns.go`), and the shortest bench an operator can
 * configure is a minute: polling at the refresher's cadence shows a new bench
 * within one interval and a lifted one no later than the pool itself learns
 * of it. Slower would show a key cooling that the pools already use again.
 */
export const MODELS_POLL_MS = 15_000;

export function Models({ id }: { id?: string }) {
  const { data, loading, error, refetch } = useQuery("credential_pool", undefined, {
    pollMs: MODELS_POLL_MS,
  });
  const org = useOrg();
  const seats = useMemo(() => indexOrg(org).seats, [org]);
  const [editing, setEditing] = useState<string | null>(null);

  const providers = useMemo(() => data?.providers ?? [], [data]);
  const addressed = id ? (providers.find((p) => p.key === id) ?? null) : null;

  return (
    <>
      {error && !data && <QueryState error={error} loading={false} />}
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}

      {data && !id && <ModelList answer={data} seats={seats} onEdit={setEditing} />}

      {data && id && addressed && (
        <ModelPage row={addressed} answer={data} seats={seats} onEdit={setEditing} />
      )}
      {data && id && !addressed && (
        <EmptyState
          icon={<CpuGlyph size="xl" />}
          title={`No model called “${id}”`}
          description="The active configuration declares no providers.llm entry under this key. It may have been renamed or removed since the link was made."
          action={
            <a className="t-link" href={href(["settings", "models"])}>
              Every model →
            </a>
          }
        />
      )}

      {editing && (
        <EditModelDialog
          id={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            void refetch();
          }}
        />
      )}
    </>
  );
}

/** Every model, with its keys as marks and who runs on it. */
function ModelList({
  answer,
  seats,
  onEdit,
}: {
  answer: CredentialPoolAnswer;
  seats: readonly Seat[];
  onEdit: (id: string) => void;
}) {
  const now = useNow();
  const providers = answer.providers;
  const pooled = providers.filter((p) => p.state !== "login");
  const keys = pooled.flatMap((p) => p.keys.filter((k) => k.state !== "duplicate"));
  const ready = keys.filter((k) => k.state === "ready").length;
  const cooling = keys.filter((k) => k.state === "cooling").length;
  const attention = providers.filter(needsAttention).length;
  const lift = nextLift(providers);

  return (
    <>
      <PageNote>
        The models seats run on, the keys each rotates through, and which of them a vendor is
        refusing now. Variable names — this screen never holds a key.
      </PageNote>
      <KeysCallout />
      <FleetNote answer={answer} />

      <Card padding="none">
        <StatGroup columns={3}>
          <StatCard
            icon={<CpuGlyph size="xs" />}
            label="Models"
            value={providers.length}
            sub={
              attention ? `${attention} need${attention === 1 ? "s" : ""} attention` : "all ready"
            }
          />
          <StatCard
            icon={<KeyGlyph size="xs" />}
            label="Keys ready"
            value={`${ready} of ${keys.length}`}
            sub={cooling ? `${cooling} cooling` : "none cooling"}
          />
          <StatCard
            icon={<ClockGlyph size="xs" />}
            label="Next key back"
            value={lift ? inTimeExact(lift, now).replace(/^in /, "") : EMPTY_VALUE}
            sub={lift ? `at ${fmtTime(lift)}` : "nothing is cooling"}
          />
        </StatGroup>
      </Card>

      <Card padding="none">
        <Card.Header icon={<CpuGlyph size="sm" />} count={providers.length}>
          Models
        </Card.Header>
        {providers.length === 0 ? (
          <div style={{ padding: "var(--spacing-4)" }}>
            <p className="t-body muted">
              The company configures no model, so every seat&rsquo;s work waits on its inbox until
              one is added under <InlineCode>providers.llm</InlineCode>.
            </p>
          </div>
        ) : (
          <DataGrid<CredentialPoolRow>
            rows={providers}
            rowKey={(p) => p.key}
            rowHref={(p) => href(["settings", "models", p.key])}
            columns={[
              {
                key: "model",
                header: "Model",
                floor: "12rem",
                sortValue: (p) => p.key,
                cell: (p) => (
                  <span className="col model-name">
                    <span className="mono">{p.key}</span>
                    <span className="t-caption muted truncate" title={p.model || undefined}>
                      {modelLine(p)}
                    </span>
                  </span>
                ),
              },
              {
                key: "state",
                header: "State",
                shrink: true,
                sortValue: (p) => p.state,
                cell: (p) => <PoolTag row={p} />,
              },
              {
                key: "keys",
                header: "Keys",
                // TWICE THE MODEL COLUMN'S SHARE of what is left: a key's mark
                // is its whole variable name and a model carries several, so
                // at an even split they stood one per line beside a Model
                // column holding a short key and its id.
                width: "minmax(0, 2fr)",
                cell: (p) => <KeyMarks row={p} now={now} />,
              },
              {
                key: "seats",
                header: "Runs",
                shrink: true,
                drop: 2,
                sortValue: (p) => usesOf(seats, p.key).length,
                cell: (p) => <ReachLine uses={usesOf(seats, p.key)} />,
              },
              {
                key: "bench",
                header: "Bench",
                shrink: true,
                drop: 1,
                // STACKED, one cause per line: on one line the pair outgrew
                // the column's cap at 1280 and was clipped mid-glyph ("auth 2"
                // for "auth 24h"), and a cell that clips shows a wrong value.
                cell: (p) =>
                  p.state === "login" ? (
                    <EmptyValue label="Nothing rotates" />
                  ) : (
                    <BenchLine row={p} stacked />
                  ),
              },
              {
                key: "act",
                header: "",
                label: "Actions",
                shrink: true,
                cell: (p) => <EditButton id={p.key} onEdit={onEdit} icon />,
              },
            ]}
          />
        )}
      </Card>
    </>
  );
}

/** One model: its keys whole, and every seat it reaches. */
function ModelPage({
  row,
  answer,
  seats,
  onEdit,
}: {
  row: CredentialPoolRow;
  answer: CredentialPoolAnswer;
  seats: readonly Seat[];
  onEdit: (id: string) => void;
}) {
  const now = useNow();
  const uses = usesOf(seats, row.key);
  const words = POOL_STATE_WORDS[row.state];
  const duplicates = row.keys.filter((k) => k.state === "duplicate").length;
  const counted = row.keys.length - duplicates;
  const access = useConfigWriteAccess();
  // ON A PHONE EDIT FOLDS INTO THE BAR'S "MORE": beside the trail it took a
  // line of its own under the crumb. The inline button is the wide bar's.
  usePageMenu([
    {
      key: "edit",
      label: `Edit ${row.key}`,
      icon: <PencilGlyph size="sm" />,
      disabled: !access.can,
      description: access.can ? undefined : access.reason,
      onSelect: () => onEdit(row.key),
    },
  ]);
  const facts: Fact[] = [
    {
      label: "Model",
      // ONE TOKEN: a vendor id broken across a line is not one anybody can
      // search for, so it keeps its line and the title carries the whole id
      // where even two tracks are too narrow.
      value: row.model ? (
        <span className="mono" title={row.model}>
          {row.model}
        </span>
      ) : null,
      token: true,
    },
    {
      label: "Keys",
      // THE SAME SET THE KEYS CARD COUNTS, said whole: a duplicate is a row
      // there and is not a key the pool holds, so it is named rather than
      // left as the difference between two numbers.
      // STACKED like the bench beside it, so a narrow fact never wraps
      // between a number and its noun.
      value:
        row.state === "login" ? (
          "a CLI login"
        ) : duplicates ? (
          <span className="model-parts is-stacked">
            <span className="model-part">{`${row.ready} of ${counted} ready`}</span>
            <span className="model-part">{plural(duplicates, "duplicate")}</span>
          </span>
        ) : (
          `${row.ready} of ${counted} ready`
        ),
    },
    {
      label: "Bench",
      value: row.state === "login" ? "nothing rotates" : <BenchLine row={row} stacked />,
    },
    {
      label: "Runs",
      // THE LIST'S WORDS for nobody: its Runs cell says "–" with this label,
      // and "0 seats" here read as a different fact about the same model.
      value: uses.length ? plural(uses.length, "seat") : <EmptyValue label={NO_SEAT} />,
    },
  ];

  return (
    <>
      <PageActions>
        <span className="page-action-folds">
          <EditButton id={row.key} onEdit={onEdit} />
        </span>
      </PageActions>
      <ObjectHeader
        kind="Model"
        icon="cpu"
        identifier={row.type}
        title={row.key}
        status={<PoolTag row={row} />}
        facts={facts}
      />
      <FleetNote answer={answer} />
      {row.state !== "ready" && (
        <Callout
          variant={
            words.tone === "danger" ? "danger" : words.tone === "warning" ? "warning" : "neutral"
          }
        >
          {words.hint}
        </Callout>
      )}

      {row.state !== "login" && (
        <Card padding="none">
          <Card.Header icon={<KeyGlyph size="sm" />} count={row.keys.length}>
            Keys
          </Card.Header>
          {row.keys.length === 0 ? (
            <div style={{ padding: "var(--spacing-4)" }}>
              <p className="t-body muted">This model names no key.</p>
            </div>
          ) : (
            <DataGrid<{ key: CredentialKeyRow; at: number }>
              name="keys"
              rows={row.keys.map((key, i) => ({ key, at: i + 1 }))}
              rowKey={(r) => String(r.at)}
              defaultSort="at"
              columns={[
                {
                  key: "at",
                  header: "#",
                  shrink: true,
                  sortValue: (r) => r.at,
                  cell: (r) => <span className="muted">{r.at}</span>,
                },
                {
                  key: "name",
                  header: "Key",
                  floor: "10rem",
                  sortValue: (r) => keyName(r.key, r.at),
                  cell: (r) => <KeyLabel keyRow={r.key} at={r.at} />,
                },
                {
                  key: "state",
                  header: "State",
                  shrink: true,
                  sortValue: (r) => r.key.state,
                  cell: (r) => <KeyTag keyRow={r.key} />,
                },
                {
                  key: "lifts",
                  header: "Back",
                  shrink: true,
                  sortValue: (r) => r.key.cooling_until ?? "",
                  cell: (r) =>
                    r.key.cooling_until ? (
                      <span title={r.key.cooling_until}>
                        {fmtTime(r.key.cooling_until)}{" "}
                        <span className="muted">{inTimeExact(r.key.cooling_until, now)}</span>
                      </span>
                    ) : r.key.state === "duplicate" ? (
                      // SAID, NOT A DASH: a duplicate is in the pool — as the
                      // key it repeats, which comes back whenever that one does.
                      <span className="muted">{keyBackWords(r.key)}</span>
                    ) : (
                      <EmptyValue label={keyBackWords(r.key)} />
                    ),
                },
                {
                  key: "why",
                  header: "Why",
                  drop: 1,
                  cell: (r) => <span className="t-caption muted">{keyWhy(r.key)}</span>,
                },
                {
                  key: "uses",
                  header: "Calls here",
                  shrink: true,
                  align: "right",
                  drop: 2,
                  sortValue: (r) => r.key.uses,
                  cell: (r) =>
                    r.key.state === "unresolved" || r.key.state === "duplicate" ? (
                      <EmptyValue label="None" />
                    ) : (
                      <span
                        className="t-num"
                        title={`Leased ${r.key.uses} times by ${answer.node} since it applied this configuration; ${r.key.in_flight} in flight now`}
                      >
                        {r.key.uses}
                        {r.key.in_flight > 0 && (
                          <span className="muted"> · {r.key.in_flight} live</span>
                        )}
                      </span>
                    ),
                },
                {
                  key: "hint",
                  header: "Hint",
                  shrink: true,
                  drop: 3,
                  cell: (r) =>
                    r.key.hint ? (
                      <span
                        className="mono t-caption muted"
                        title="The engine's credential_cooled log lines name a key by this hint — never by its value"
                      >
                        {r.key.hint}
                      </span>
                    ) : (
                      <EmptyValue label="None" />
                    ),
                },
              ]}
            />
          )}
        </Card>
      )}

      <Card padding="none">
        <Card.Header icon={<UsersGlyph size="sm" />} count={uses.length}>
          Seats on it
        </Card.Header>
        {uses.length === 0 ? (
          <div style={{ padding: "var(--spacing-4)" }}>
            <p className="t-body muted">
              No seat&rsquo;s chain names this model. A seat that names none runs on the entry
              called <InlineCode>default</InlineCode>, or the first one configured.
            </p>
          </div>
        ) : (
          <DataGrid<ModelUse>
            name="seats"
            rows={uses}
            rowKey={(u) => u.seat.key}
            rowHref={(u) => (u.seat.handle ? href(["agents", "seats", u.seat.handle]) : "")}
            columns={[
              {
                key: "seat",
                header: "Seat",
                floor: "10rem",
                sortValue: (u) => u.seat.name,
                cell: (u) => (
                  <SeatCell handle={u.seat.handle} name={u.seat.name} kind={u.seat.kind} />
                ),
              },
              {
                key: "runs",
                header: "Runs on it",
                cell: (u) =>
                  u.every ? (
                    <TextCell>Every phase</TextCell>
                  ) : u.runs.length ? (
                    <TextCell>{u.runs.join(", ")}</TextCell>
                  ) : (
                    <EmptyValue label="Nothing first" />
                  ),
              },
              {
                key: "fallback",
                header: "Falls back to it",
                drop: 1,
                cell: (u) =>
                  u.fallback.length ? (
                    <TextCell>{u.fallback.join(", ")}</TextCell>
                  ) : (
                    <EmptyValue label="Never" />
                  ),
              },
            ]}
          />
        )}
      </Card>
    </>
  );
}

/**
 * Where a key's value lives, said once on the list: a model's keys are
 * pointers into the sealed store, and the edit writes pointers.
 */
function KeysCallout() {
  return (
    <Callout variant="neutral" icon={<KeyGlyph size="md" />}>
      <span className="col" style={{ gap: 4 }}>
        <span>
          Each model names its keys as <InlineCode>{"${NAME}"}</InlineCode>, resolved from the
          company&rsquo;s{" "}
          <a className="prose-link" href={href(["settings", "secrets"])}>
            secrets
          </a>{" "}
          or the engine&rsquo;s environment. With several, calls spread across them; a key the
          vendor refuses is benched and the rest carry on. When every key is benched, a seat falls
          through to its next model.
        </span>
        <span className="t-caption">
          A bench is shared: one node&rsquo;s refusal is every node&rsquo;s within 15 seconds.
        </span>
      </span>
    </Callout>
  );
}

/** Said only when the fleet's ledger was not read: every deadline is this node's alone. */
function FleetNote({ answer }: { answer: CredentialPoolAnswer }) {
  if (answer.fleet) return null;
  return (
    <Callout variant="warning" role="status">
      <span className="col" style={{ gap: 4 }}>
        <span>
          These are <span className="mono">{answer.node}</span>&rsquo;s own cooldowns only: the
          fleet&rsquo;s cooldown ledger could not be read, so a key another node benched may read
          ready here.
        </span>
        {answer.fleet_error && <span className="t-caption">{answer.fleet_error}</span>}
      </span>
    </Callout>
  );
}

function PoolTag({ row }: { row: CredentialPoolRow }) {
  const words = POOL_STATE_WORDS[row.state];
  return (
    <Tag size="sm" variant={words.tone} title={words.hint}>
      {words.label}
    </Tag>
  );
}

function KeyTag({ keyRow }: { keyRow: CredentialKeyRow }) {
  const words = KEY_STATE_WORDS[keyRow.state];
  return (
    <Tag size="sm" variant={words.tone} title={keyWhy(keyRow)}>
      {words.label}
    </Tag>
  );
}

/** A key's name as it reads in a row: the variable, or its place in the list. */
function KeyLabel({ keyRow, at }: { keyRow: CredentialKeyRow; at: number }) {
  if (keyRow.source === "inline") {
    return (
      <span className="muted" title={`Key ${at}: ${KEY_SOURCE_WORDS.inline}`}>
        {keyName(keyRow, at)}
      </span>
    );
  }
  return (
    <span className="row gap-2">
      <KeyCell value={keyRow.ref} />
      {keyRow.source === "default" && (
        <Tag size="sm" appearance="outline" title={`${keyRow.ref}: ${KEY_SOURCE_WORDS.default}`}>
          default
        </Tag>
      )}
    </span>
  );
}

/**
 * A model's keys as one mark each, in declaration order: the state in its
 * tone, the variable in its title. A cooling key carries how long it has left,
 * which is the one number a person watching a rate limit wants.
 */
function KeyMarks({ row, now }: { row: CredentialPoolRow; now: number }) {
  if (row.state === "login") return <EmptyValue label="A CLI login" />;
  if (row.keys.length === 0) return <EmptyValue label="No key" />;
  return (
    <span className="row gap-1 wrap" role="list" aria-label={`${row.key} keys`}>
      {row.keys.map((k, i) => {
        const words = KEY_STATE_WORDS[k.state];
        const name = keyName(k, i + 1);
        const left =
          k.state === "cooling" && k.cooling_until ? inTimeExact(k.cooling_until, now) : "";
        return (
          <span role="listitem" key={i}>
            <Tag
              size="sm"
              appearance="outline"
              variant={words.tone}
              title={`${name}: ${words.label.toLowerCase()}${left ? `, back ${left}` : ""}. ${keyWhy(k)}`}
            >
              <span className="mono model-key">{keyMark(k, i + 1)}</span>
              {/* THE STATE IN WORDS for a reader who does not get the tone:
                  a screen reader, or eyes that cannot tell the inks apart. */}
              <span className="sr-only">, {words.label.toLowerCase()}</span>
              {left && <span className="model-key-left"> {left.replace(/^in /, "")}</span>}
            </Tag>
          </span>
        );
      })}
    </span>
  );
}

/**
 * "rate limit 1h · auth 5m": a model's bench times, each kept whole, the
 * status codes behind them in each part's title. STACKED where the line is
 * narrow (a header fact): one part per line and no separator, because a
 * wrapped "·" is left dangling at the end of the first line.
 */
function BenchLine({ row, stacked = false }: { row: CredentialPoolRow; stacked?: boolean }) {
  const parts = benchParts(row);
  return (
    <span className={stacked ? "model-parts is-stacked" : "model-parts"}>
      {parts.map((part, i) => (
        <Fragment key={part.key}>
          {i > 0 && !stacked && (
            <span className="model-parts-sep" aria-hidden="true">
              ·
            </span>
          )}
          <span className="model-part" title={part.title}>
            {part.words}
          </span>
        </Fragment>
      ))}
    </span>
  );
}

/** What a model no seat's chain names reads as, on the list and on its page alike. */
const NO_SEAT = "No seat names it";

/** "3 seats · fallback for 2" — who would feel this model going quiet. */
function ReachLine({ uses }: { uses: ModelUse[] }) {
  const runs = uses.filter((u) => u.runs.length).length;
  const behind = uses.length - runs;
  if (uses.length === 0) return <EmptyValue label={NO_SEAT} />;
  return (
    <TextCell>
      {plural(runs, "seat")}
      {behind > 0 && <span className="muted"> · fallback for {behind}</span>}
    </TextCell>
  );
}

/**
 * Edit, for every reader, disabled with the reason for one who cannot change
 * the configuration — a write the engine would refuse is not offered as one.
 */
function EditButton({
  id,
  onEdit,
  icon,
}: {
  id: string;
  onEdit: (id: string) => void;
  icon?: boolean;
}) {
  const access = useConfigWriteAccess();
  const reason = access.can ? undefined : access.reason;
  // INSIDE A ROW THAT IS A LINK: the row's own navigation is not this
  // button's, and the browser would follow it on the way past.
  const press = (e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    onEdit(id);
  };
  if (icon) {
    return (
      <IconButton
        size="sm"
        variant="ghost"
        icon={<PencilGlyph size="sm" />}
        label={`Edit ${id}`}
        title={reason ?? `Edit ${id}`}
        disabledReason={reason}
        onClick={press}
      />
    );
  }
  return (
    <Button
      variant="secondary"
      leadingIcon={<PencilGlyph size="sm" />}
      disabledReason={reason}
      title={reason}
      onClick={press}
    >
      Edit
    </Button>
  );
}
