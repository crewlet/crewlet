/**
 * The company's credentials — names and provenance, never values.
 *
 * This surface existed on the engine and had ZERO references in the previous
 * dashboard: `/secrets` was reachable, guarded, and unreachable from any
 * screen. Reads are guarded here too, deliberately — the list of what a
 * company holds a credential for is itself worth guarding.
 *
 * There is no reveal button. The one route that returns a value needs an
 * explicit flag and logs the access, and putting that behind a click in a
 * dashboard that anyone with the token can open is not a trade worth making;
 * `crewlet secrets get` is the deliberate path.
 */

import { useCallback, useEffect, useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Empty, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { fmtDateTime, relTime, tsKey, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { onTokenChanged, rest, RestError } from "~/protocol/index.ts";
import type { SecretRow } from "~/protocol/index.ts";

export function Secrets() {
  const now = useNow();
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

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const body = (await rest.get("/secrets")) as { secrets?: SecretRow[] } | null;
      setRows(body?.secrets ?? []);
      setError(null);
    } catch (err) {
      // The last good list stays on screen. A refusal to refresh is not a
      // reason to tell an operator the company holds no credentials.
      setError(
        err instanceof RestError
          ? err.unauthorized
            ? "This surface needs an operator token. Set one from the engine panel."
            : err.detail || err.code || "the engine refused the read"
          : String(err),
      );
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);
  // This screen's refusal names the missing token, so supplying one has to
  // refresh it in place rather than waiting for a reload.
  useEffect(() => onTokenChanged(() => void load()), [load]);

  const list = rows ?? [];
  const fromStore = list.filter((r) => r.source === "store").length;

  return (
    <>
      <ScreenHead
        title="Secrets"
        sub="The company's sealed credentials. Names, key ids and provenance — this screen never asks for a value."
        badges={<Badge outline>{plural(list.length, "credential")} held</Badge>}
      />

      <div className="banner neutral">
        <Icon name="shield" size="sm" />
        <span className="col" style={{ gap: 4 }}>
          <span>
            These live in the fleet's coordination store, sealed with the Tier A keyring, and every
            node reads them. A <code className="inline">${"{VAR}"}</code> in the company config
            resolves here first and falls back to the process environment.
          </span>
          <span className="t-caption">
            Values are never sent to this page. Reading one is{" "}
            <code className="inline">crewlet secrets get</code>, which logs the access.
          </span>
        </span>
      </div>

      <Panel padding="none">
        <StatRow cols={3}>
          <Stat icon="key" label="Credentials" value={list.length} sub="names the fleet holds" />
          <Stat
            icon="database"
            label="In the secret store"
            value={fromStore}
            sub="the rest resolve from this process's environment"
          />
          <Stat
            icon="shield"
            label="Distinct key ids"
            value={new Set(list.map((r) => r.key_id)).size}
            sub="a rekey moves every value onto a new one"
          />
        </StatRow>
      </Panel>

      {loading && rows === null && <Skeleton rows={4} />}
      <QueryState
        error={error}
        loading={loading}
        empty={
          list.length
            ? undefined
            : {
                title: "No secrets are stored",
                hint: "Set one with crewlet secrets set, or let a provisioning command hand one straight to the engine.",
              }
        }
      >
        <Panel padding="none">
          <DataTable<SecretRow>
            rows={list}
            rowKey={(s) => s.name}
            defaultSort={{ key: "name", dir: "asc" }}
            columns={[
              {
                key: "name",
                header: "Name",
                sortValue: (s) => s.name,
                cell: (s) => <code className="inline">{s.name}</code>,
              },
              {
                key: "source",
                header: "Source",
                shrink: true,
                sortValue: (s) => s.source,
                cell: (s) => <Badge outline>{s.source}</Badge>,
              },
              {
                key: "key",
                header: "Key id",
                shrink: true,
                sortValue: (s) => s.key_id,
                cell: (s) => <code className="inline">{s.key_id}</code>,
              },
              {
                key: "by",
                header: "Set by",
                sortValue: (s) => s.updated_by,
                cell: (s) => s.updated_by || <span className="faint">—</span>,
              },
              {
                key: "at",
                header: "Updated",
                shrink: true,
                sortValue: (s) => tsKey(s.updated_at),
                cell: (s) => (
                  <span className="t-caption" title={fmtDateTime(s.updated_at)}>
                    {relTime(s.updated_at, now)}
                  </span>
                ),
              },
            ]}
          />
        </Panel>
      </QueryState>
    </>
  );
}
