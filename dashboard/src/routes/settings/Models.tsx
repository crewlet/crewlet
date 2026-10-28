/**
 * Settings › Models & keys: every model the company configures — the coding
 * CLI it drives, the model it runs, how it signs in — and which seats run on
 * it.
 *
 * # The document for the entries, the chart for the reach
 *
 * Every model is a `providers.llm` entry of the company document, read through
 * the same Admin-only `config` question the Configuration screen asks, and
 * drawn as the engine will run it: a field the entry leaves out is said as the
 * default the engine applies (`contract/models.ts`, held to the engine's own).
 * Which seats run on a model is the chain the engine resolved for each seat,
 * pushed on the org projection. Neither is worked out again here.
 *
 * There is no per-key state to show. A model is a CLI, and a CLI holds one
 * sign-in and rotates nothing, so what an operator needs is how it signs in —
 * which mode, and which variable the credential is read from — whether it can
 * run a seat's executor and where, and whether anything still runs on it.
 *
 * # Names, never values
 *
 * The document is served redacted: a credential written as one whole
 * `${NAME}` is that name, and anything else is the mask, which this screen
 * says is "written inline" and never draws. The edit is one entity write
 * (`PUT /config/llm-providers/{id}`) of that same redacted entity: a
 * credential written inline goes back as the mask, and the engine restores it
 * from the revision it was read from.
 *
 * Operator-only, like the rest of the column: which variable signs each model
 * in is a map of which credential to take.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  Card,
  DescriptionList,
  EmptyState,
  EmptyValue,
  IconButton,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  TableEntityCell,
  TableTextCell,
} from "@crewlethq/ui";
import {
  CpuGlyph,
  KeyGlyph,
  PencilGlyph,
  SquareTerminalGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { linkedSeat } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href } from "~/app/router.tsx";
import { usePageMenu } from "~/app/Shell.tsx";
import { plural } from "~/lib/format.ts";
import {
  authWords,
  credentialWords,
  modelEntries,
  modelLine,
  signInCredential,
  signInFallback,
  signInSource,
  roleWords,
  timeoutWords,
  usesOf,
  type Credential,
  type ModelEntry,
  type ModelUse,
} from "~/lib/models.ts";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { EditModelDialog } from "./EditModelDialog.tsx";

/**
 * The company's models and its seats, as this page and its Home widget read
 * them: the document's `providers.llm` entries (`config`, Admin's) and the org
 * projection the seats' chains ride on.
 *
 * RE-READ ON EVERY PUBLISHED COMPANY, and never on a clock. The document
 * changes only with a revision, and the engine re-sends the org projection on
 * each one it publishes — which is also what the seats are drawn from — so
 * the two move together, and a poll would only ask about a document that
 * cannot have changed.
 */
export function useModels() {
  const query = useQuery("config");
  const { refetch } = query;
  const org = useOrg();
  const seats = useMemo(() => indexOrg(org).seats, [org]);
  const seen = useRef(org);
  useEffect(() => {
    if (seen.current === org) return;
    seen.current = org;
    refetch();
  }, [org, refetch]);
  const entries = useMemo(() => modelEntries(query.data), [query.data]);
  return { ...query, entries, seats };
}

export function Models({ id }: { id?: string }) {
  const { entries, seats, loading, error, refusal, data, refetch } = useModels();
  const [editing, setEditing] = useState<string | null>(null);

  const addressed = id ? (entries.find((e) => e.key === id) ?? null) : null;
  // NULL IS AN ANSWER: no company is configured yet, which has no model — not
  // a question still out. And a document already read stays on screen through
  // a later read that failed: `useQuery` keeps its last good answer for that.
  const answered = data !== null || (!loading && !error);

  return (
    <>
      {error && !data && <QueryState error={error} refusal={refusal} loading={false} />}
      {loading && <Skeleton variant="text" rows={6} label="Loading" />}

      {answered && !id && <ModelList entries={entries} seats={seats} onEdit={setEditing} />}

      {answered && id && addressed && (
        <ModelPage entry={addressed} seats={seats} onEdit={setEditing} />
      )}
      {answered && id && !addressed && (
        <EmptyState
          icon={<CpuGlyph size="xl" />}
          title={`No model called “${id}”`}
          description="It may have been renamed or removed."
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
            refetch();
          }}
        />
      )}
    </>
  );
}

/** Every model, with the CLI it drives, how it signs in and who runs on it. */
function ModelList({
  entries,
  seats,
  onEdit,
}: {
  entries: ModelEntry[];
  seats: readonly Seat[];
  onEdit: (id: string) => void;
}) {
  const unused = entries.filter((e) => usesOf(seats, e.key).length === 0).length;
  const executors = entries.filter((e) => e.executor).length;
  const metered = entries.filter((e) => e.authMode === "api-key").length;

  return (
    <>
      <Card padding="none">
        <StatGroup columns={3}>
          <StatCard
            icon={<CpuGlyph size="xs" />}
            label="Models"
            value={entries.length}
            // NONE IS NOT ALL IN USE: "0, every one in use" said every model
            // ran a seat on a company with no model to run a turn on.
            sub={
              entries.length === 0
                ? "none configured"
                : unused
                  ? `${unused} named by no seat`
                  : "every one in use"
            }
          />
          <StatCard
            icon={<SquareTerminalGlyph size="xs" />}
            label="Executors"
            value={executors}
            // NONE IS A COMPANY WHOSE AGENT SEATS LAUNCH NOTHING: every seat's
            // executor is a coding CLI's own run, and only these CLIs have one.
            sub={executors ? `can run a seat’s executor` : "none can run a seat’s executor"}
          />
          <StatCard
            icon={<KeyGlyph size="xs" />}
            label="On an API key"
            value={metered}
            sub={metered ? "billed to a metered account" : "none billed per token"}
          />
        </StatGroup>
      </Card>

      <Card padding="none">
        <Card.Header icon={<CpuGlyph size="sm" />} count={entries.length}>
          <Card.Title>Models</Card.Title>
        </Card.Header>
        {entries.length === 0 ? (
          // THE KIT'S EMPTY STATE, as the MCP servers card beside it draws its
          // own, with the way to where a model is added: a plain sentence in
          // a padded box was the one empty card on Settings drawn by hand.
          <EmptyState
            size="compact"
            icon={<CpuGlyph size="xl" />}
            title="No model configured yet"
            description={
              <>
                Add one under <InlineCode>providers.llm</InlineCode> in the company document.
              </>
            }
            action={
              <ButtonLink variant="secondary" size="small" href={href(["settings", "config"])}>
                Open Configuration
              </ButtonLink>
            }
          />
        ) : (
          <DataGrid<ModelEntry>
            rows={entries}
            rowKey={(e) => e.key}
            rowHref={(e) => href(["settings", "models", e.key])}
            columns={[
              {
                key: "model",
                header: "Model",
                floor: "12rem",
                sortValue: (e) => e.key,
                cell: (e) => (
                  <span className="col model-name">
                    <span className="mono">{e.key}</span>
                    <span className="t-caption muted truncate" title={modelLine(e)}>
                      {modelLine(e)}
                    </span>
                  </span>
                ),
              },
              {
                key: "runs",
                header: "Runs as",
                shrink: true,
                drop: 1,
                sortValue: (e) => roleWords(e).label + e.runIn,
                cell: (e) => <RoleLine entry={e} />,
              },
              {
                key: "signin",
                header: "Signs in with",
                sortValue: (e) => e.authMode,
                cell: (e) => <SignInLine entry={e} />,
              },
              {
                key: "seats",
                header: "Runs",
                shrink: true,
                drop: 2,
                sortValue: (e) => usesOf(seats, e.key).length,
                cell: (e) => <ReachLine uses={usesOf(seats, e.key)} />,
              },
              {
                key: "act",
                header: "",
                action: true,
                shrink: true,
                cell: (e) => <EditButton id={e.key} onEdit={onEdit} icon />,
              },
            ]}
          />
        )}
      </Card>
    </>
  );
}

/** One model: what it runs, how it signs in, and every seat it reaches. */
function ModelPage({
  entry,
  seats,
  onEdit,
}: {
  entry: ModelEntry;
  seats: readonly Seat[];
  onEdit: (id: string) => void;
}) {
  const uses = usesOf(seats, entry.key);
  const role = roleWords(entry);
  const access = useConfigWriteAccess();
  // ON A PHONE EDIT FOLDS INTO THE BAR'S "MORE": beside the trail it took a
  // line of its own under the crumb. The inline button is the wide bar's.
  usePageMenu([
    {
      key: "edit",
      label: `Edit ${entry.key}`,
      icon: <PencilGlyph size="sm" />,
      disabled: !access.can,
      description: access.can ? undefined : access.reason,
      onSelect: () => onEdit(entry.key),
    },
  ]);
  const facts: Fact[] = [
    {
      label: "Model",
      // ONE TOKEN: a model id broken across a line is not one anybody can
      // search for, so it keeps its line and the title carries the whole id
      // where even two tracks are too narrow.
      value: entry.model ? (
        <span className="mono" title={entry.model}>
          {entry.model}
        </span>
      ) : null,
      token: true,
    },
    {
      label: "CLI",
      value: <span className="mono">{entry.agent}</span>,
      note: entry.agentDefault ? "The default: the entry names no agent." : undefined,
    },
    {
      label: "Runs as",
      value: entry.executor && entry.runIn ? `${role.label}, in ${entry.runIn}` : role.label,
      note:
        entry.executor && !entry.runIn
          ? "In providers.sandbox’s default cell: the entry names no run_in."
          : undefined,
    },
    {
      label: "Timeout",
      value: timeoutWords(entry.timeoutSeconds),
      note: entry.timeoutSet ? undefined : "The default: the entry names no timeout_seconds.",
    },
    {
      label: "At once",
      value: plural(entry.maxConcurrent, "process", "processes"),
      note: entry.concurrencySet ? undefined : "The default: the entry names no max_concurrent.",
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
          <EditButton id={entry.key} onEdit={onEdit} />
        </span>
      </PageActions>
      <ObjectHeader
        kind="Model"
        icon="cpu"
        identifier={entry.agent}
        title={entry.key}
        description={role.hint}
        facts={facts}
      />

      <SignInCard entry={entry} />

      <Card padding="none">
        <Card.Header icon={<UsersGlyph size="sm" />} count={uses.length}>
          <Card.Title>Seats on it</Card.Title>
        </Card.Header>
        {uses.length === 0 ? (
          <div style={{ padding: "var(--spacing-4)" }}>
            <p className="t-body muted">No seat names this model.</p>
          </div>
        ) : (
          <DataGrid<ModelUse>
            name="seats"
            rows={uses}
            rowKey={(u) => u.seat.key}
            rowHref={(u) => href(["company", "org", "seats", u.seat.handle])}
            columns={[
              {
                key: "seat",
                header: "Seat",
                floor: "10rem",
                sortValue: (u) => u.seat.name,
                cell: (u) => (
                  <TableEntityCell
                    {...linkedSeat({
                      handle: u.seat.handle,
                      name: u.seat.name,
                      kind: u.seat.kind,
                      avatar: u.seat.avatar,
                    })}
                  />
                ),
              },
              {
                key: "runs",
                header: "Runs on it",
                cell: (u) =>
                  u.every ? (
                    <TableTextCell>Every phase</TableTextCell>
                  ) : u.runs.length ? (
                    <TableTextCell>{u.runs.join(", ")}</TableTextCell>
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
                    <TableTextCell>{u.fallback.join(", ")}</TableTextCell>
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
 * How a model signs in: the mode, the credential that mode reads — by the
 * variable it names, never its value — and what else its CLI is handed. The
 * CLI, the mode and the sign-in are changed in Configuration, where the whole
 * entry is in view; the link goes to this entry there.
 */
function SignInCard({ entry }: { entry: ModelEntry }) {
  const auth = authWords(entry.authMode);
  const read = signInCredential(entry);
  return (
    <Card>
      <Card.Header icon={<KeyGlyph size="sm" />}>
        <Card.Title>Signing in</Card.Title>
      </Card.Header>
      <DescriptionList>
        <DescriptionList.Item term="Signs in with">
          <span className="col model-name">
            <span>{auth.label}</span>
            <span className="t-caption muted">
              {auth.hint}
              {entry.authDefault ? " The default: the entry names no auth.mode." : ""}
            </span>
          </span>
        </DescriptionList.Item>
        {read && (
          <DescriptionList.Item term={read.label}>
            {read.value ? (
              <CredentialName value={read.value} />
            ) : (
              <span className="muted">{signInFallback(entry)}</span>
            )}
          </DescriptionList.Item>
        )}
        {entry.bundle && (
          <DescriptionList.Item term="Login bundle">
            <CredentialName value={entry.bundle} />
          </DescriptionList.Item>
        )}
        <DescriptionList.Item term="Login kept in">
          {entry.stateDir ? (
            <span className="mono">{entry.stateDir}</span>
          ) : (
            <span className="muted">A directory derived from the key on each node.</span>
          )}
        </DescriptionList.Item>
        {entry.env.length > 0 && (
          <DescriptionList.Item term="Environment">
            <ul className="model-env" aria-label={`${entry.key} environment`}>
              {entry.env.map((v) => (
                <li key={v.name}>
                  <span className="mono">{v.name}</span>
                  <span className="muted"> from </span>
                  {v.value ? (
                    <CredentialName value={v.value} />
                  ) : (
                    <span className="muted">nothing</span>
                  )}
                </li>
              ))}
            </ul>
          </DescriptionList.Item>
        )}
      </DescriptionList>
      <p className="t-caption model-signin-change">
        <a
          className="t-link"
          href={href(["settings", "config"], {
            lens: "entities",
            kind: "llm-providers",
            entity: entry.key,
          })}
        >
          Change the CLI, where its runs happen or how it signs in under Configuration →
        </a>
      </p>
    </Card>
  );
}

/** A credential by the variable it names, or said to be inline — never a value. */
function CredentialName({ value }: { value: Credential }) {
  return value.kind === "reference" ? (
    <span className="mono">{value.name}</span>
  ) : (
    <span className="muted">{credentialWords(value)}</span>
  );
}

/** "Executor · e2b": what the engine runs the CLI for, and where its executor runs happen. */
function RoleLine({ entry }: { entry: ModelEntry }) {
  const words = roleWords(entry);
  return (
    <span className="truncate" title={words.hint}>
      {words.label}
      {entry.executor && entry.runIn && <span className="muted"> · {entry.runIn}</span>}
    </span>
  );
}

/** "API key" over "ANTHROPIC_KEY_A": the sign-in, and where its credential comes from. */
function SignInLine({ entry }: { entry: ModelEntry }) {
  const words = authWords(entry.authMode);
  const source = signInSource(entry);
  const named = signInCredential(entry)?.value?.kind === "reference";
  return (
    <span className="col model-name" title={words.hint}>
      <span>{words.label}</span>
      {source && (
        <span className={`t-caption muted truncate${named ? " mono" : ""}`} title={source}>
          {source}
        </span>
      )}
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
    <TableTextCell>
      {plural(runs, "seat")}
      {behind > 0 && <span className="muted"> · fallback for {behind}</span>}
    </TableTextCell>
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
