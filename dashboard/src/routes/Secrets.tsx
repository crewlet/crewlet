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
 * dashboard that anyone with the token can open is not a trade worth making;
 * `crewlet secrets get` is the deliberate path. Storing, editing and removing
 * a row need no such trade: none of them reads a value back.
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

import { useCallback, useEffect, useMemo, useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { SecretDialog } from "./SecretDialog.tsx";
import { RemoveSecretDialog } from "./RemoveSecretDialog.tsx";
import { useParam } from "~/app/router.tsx";
import { fmtDateTime, tsKey, plural } from "~/lib/format.ts";
import { onTokenChanged, rest, RestError } from "~/protocol/index.ts";
import type { ConfigReference, SecretRow } from "~/protocol/index.ts";
import {
  Button,
  DataView,
  EmptyState,
  EmptyValue,
  RelativeTime,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useNow,
  useToast,
  type DataViewColumn,
  type FilterDef,
  type FilterValues,
} from "@crewlethq/ui";
import {
  AddGlyph,
  CloseGlyph,
  DatabaseGlyph,
  EditGlyph,
  KeyGlyph,
  ShieldGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * How a read that did not answer is reported to the operator.
 *
 * One sentence for a missing token and the engine's own words otherwise,
 * shared by both reads this screen makes so they cannot describe the same
 * refusal two ways.
 */
function refusal(err: unknown): string {
  if (!(err instanceof RestError)) return String(err);
  if (err.unauthorized) {
    return "This surface needs an operator token. Set one from the engine panel.";
  }
  return err.detail || err.code || "the engine refused the read";
}

export function Secrets() {
  const now = useNow();
  const toast = useToast();
  // GET /secrets, over REST, because no question in the registry answers it.
  //
  // This screen asked `config_entities {kind: "secrets"}`, and that kind does
  // not exist: the entity kinds are roles, units, llm-providers and
  // mcp-servers, so the answer was always an ErrUnknownEntityKind folded to
  // a bad-params error and the table could never hold a row. The socket is
  // still the data channel for everything it answers; this surface is simply
  // not one of them.
  const [rows, setRows] = useState<SecretRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  // THE REFERENCE INDEX IS THREE-VALUED, and collapsing it to two is the one
  // mistake this screen must not make. `references` null means the question
  // was not answered — never that the answer was "nothing" — and the removal
  // confirmation branches on exactly that.
  const [references, setReferences] = useState<ConfigReference[] | null>(null);
  const [unknown, setUnknown] = useState<string | null>(null);

  const [writing, setWriting] = useState<{ editing: string } | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const body = (await rest.get("/secrets")) as { secrets?: SecretRow[] } | null;
      setRows(body?.secrets ?? []);
      setError(null);
    } catch (err) {
      // The last good list stays on screen. A refusal to refresh is not a
      // reason to tell an operator the company holds no credentials.
      setError(refusal(err));
    } finally {
      setLoading(false);
    }
  }, []);

  const loadReferences = useCallback(async () => {
    try {
      const body = (await rest.get("/config/references")) as {
        references?: ConfigReference[];
      } | null;
      setReferences(body?.references ?? []);
      setUnknown(null);
    } catch (err) {
      // A 404 IS AN ANSWER. A deployment before its first config import has
      // no active document, so nothing can be pointing at anything, and
      // treating that as a failed check would put a warning in front of
      // every removal on a new install.
      if (err instanceof RestError && err.status === 404) {
        setReferences([]);
        setUnknown(null);
        return;
      }
      setReferences(null);
      setUnknown(refusal(err));
    }
  }, []);

  const reload = useCallback(async () => {
    await Promise.all([load(), loadReferences()]);
  }, [load, loadReferences]);

  useEffect(() => {
    void reload();
  }, [reload]);
  // This screen's refusal names the missing token, so supplying one has to
  // refresh it in place rather than waiting for a reload.
  useEffect(() => onTokenChanged(() => void reload()), [reload]);

  const list = rows ?? [];
  const fromStore = list.filter((r) => r.source === "store").length;

  // Grouped by name, because one credential routinely has several readers: a
  // seat's bot_token and its mcp_env entry are two pointers at one row, and
  // both have to be visible before it goes.
  const readers = useMemo(() => {
    if (references === null) return null;
    const byName = new Map<string, string[]>();
    for (const ref of references) {
      byName.set(ref.name, [...(byName.get(ref.name) ?? []), ref.path]);
    }
    return byName;
  }, [references]);

  const pathsFor = (name: string): string[] | null => readers?.get(name) ?? (readers ? [] : null);

  /*
   * THE SCREEN OWNS THE NARROWING, so both axes are URL parameters. Source is
   * the axis worth having: whether a name is sealed in the coordination store
   * or resolves from this process's own environment is the difference between
   * a credential the fleet holds and one only this node can see.
   */
  const [q, setQ] = useParam("q", "");
  const [source, setSource] = useParam("source", "");

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return list
      .filter((s) => !source || s.source === source)
      .filter((s) => !needle || s.name.toLowerCase().includes(needle));
  }, [list, q, source]);

  const filters = useMemo<FilterDef<SecretRow>[]>(
    () => [
      { name: "q", label: "Search credentials", role: "search", placeholder: "Search by name" },
      {
        name: "source",
        label: "Source",
        kind: "select",
        options: [
          { value: "", label: "Any source" },
          ...[...new Set(list.map((s) => s.source))].sort().map((s) => ({ value: s, label: s })),
        ],
      },
    ],
    [list],
  );

  const values: FilterValues = { q, source };

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setQ(String(next.q ?? ""));
      setSource(String(next.source ?? ""));
    },
    [setQ, setSource],
  );

  const columns = useMemo<DataViewColumn<SecretRow>[]>(
    () => [
      { key: "name", header: "Name", sortable: true, mono: true, copyable: true },
      {
        key: "read",
        header: "Read by",
        shrink: true,
        // AN UNANSWERED CHECK SORTS AS ITS OWN THING, below every count, in
        // both directions: "not known" is not a zero, and the table's own
        // comparator puts an absent value last whichever way a reader sorts.
        sortable: true,
        sortValue: (s) => pathsFor(s.name)?.length ?? null,
        render: (s) => <Readers paths={pathsFor(s.name)} />,
      },
      {
        key: "source",
        header: "Source",
        shrink: true,
        sortable: true,
        render: (s) => <Tag appearance="outline">{s.source}</Tag>,
      },
      { key: "key_id", header: "Key id", shrink: true, sortable: true, mono: true },
      {
        key: "updated_by",
        header: "Set by",
        sortable: true,
        render: (s) => s.updated_by || <EmptyValue label="Not recorded" />,
      },
      {
        key: "updated_at",
        header: "Updated",
        shrink: true,
        sortable: true,
        firstDirection: "desc",
        sortValue: (s) => tsKey(s.updated_at),
        render: (s) => <RelativeTime className="t-caption" value={s.updated_at} now={now} />,
      },
    ],
    // `pathsFor` reads the references answer, which is what `readers` holds.
    [readers, now],
  );

  return (
    <>
      <ScreenHead
        title="Secrets"
        sub="The company's sealed credentials. Names, key ids and provenance — this screen never asks for a value."
        badges={<Tag appearance="outline">{plural(list.length, "credential")} held</Tag>}
        actions={
          <Button
            leadingIcon={<AddGlyph />}
            variant="primary"
            onClick={() => setWriting({ editing: "" })}
          >
            Store a secret
          </Button>
        }
      />

      <div className="banner neutral">
        <ShieldGlyph size="sm" />
        <span className="col" style={{ gap: 4 }}>
          <span>
            These live in the fleet's coordination store, sealed with the Tier A keyring, and every
            node reads them. A <code className="inline">${"{VAR}"}</code> in the company config
            resolves here first and falls back to the process environment.
          </span>
          <span className="t-caption">
            Values are never sent to this page. Reading one is{" "}
            <code className="inline">crewlet secrets get</code>, which logs the access. A new value
            reaches a running seat at the next configuration activation or restart.
          </span>
        </span>
      </div>

      <StatGroup columns={3}>
        <StatCard
          icon={<KeyGlyph />}
          label="Credentials"
          value={list.length}
          sub="names the fleet holds"
        />
        <StatCard
          icon={<DatabaseGlyph />}
          label="In the secret store"
          value={fromStore}
          sub="the rest resolve from this process's environment"
        />
        <StatCard
          icon={<ShieldGlyph />}
          label="Distinct key ids"
          value={new Set(list.map((r) => r.key_id)).size}
          sub="a rekey moves every value onto a new one"
        />
      </StatGroup>

      {loading && rows === null && <Skeleton label="Loading the secrets" variant="text" rows={4} />}
      {error && <QueryState error={error} loading={loading} />}

      <DataView<SecretRow>
        framed
        columns={columns}
        rows={shown}
        totalCount={list.length}
        getRowKey={(s) => s.name}
        defaultSort={{ key: "name", direction: "asc" }}
        filters={filters}
        filterValues={values}
        onFilterValuesChange={onValuesChange}
        rowActions={(s) => [
          {
            label: `Edit ${s.name}`,
            icon: <EditGlyph />,
            onClick: () => setWriting({ editing: s.name }),
          },
          {
            label: `Remove ${s.name}`,
            icon: <CloseGlyph />,
            danger: true,
            onClick: () => setRemoving(s.name),
          },
        ]}
        emptyMessage={
          <EmptyState
            size="compact"
            icon={<KeyGlyph />}
            title={list.length ? "No credential matches these filters" : "No secrets are stored"}
            description={
              list.length
                ? "Clear them to see every name the fleet holds."
                : "Store one here, set one with crewlet secrets set, or let a provisioning command hand one straight to the engine."
            }
          />
        }
      />

      {writing && (
        <SecretDialog
          editing={writing.editing}
          paths={writing.editing ? pathsFor(writing.editing) : null}
          onClose={() => setWriting(null)}
          onDone={(name) => {
            toast.ok(writing.editing ? `Updated ${name}` : `Stored ${name}`);
            void reload();
          }}
        />
      )}

      {removing && (
        <RemoveSecretDialog
          name={removing}
          paths={pathsFor(removing)}
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
 * and the removal confirmation is where the list has to be read anyway.
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
    return <EmptyValue label="No field reads it" />;
  }
  return (
    <Tag appearance="outline" title={paths.join("\n")}>
      {plural(paths.length, "field")}
    </Tag>
  );
}
