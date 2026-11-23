/**
 * A spending product's caps per vendor account, and the form an admin sets
 * them in: the most one change may add to the account's daily budgets, and
 * the ceiling on those budgets after any change, each in the account's own
 * currency.
 *
 * A RAISE IS A RAISE. The engine asks for a fresh sign-in before it stores a
 * higher cap, and the transport asks the person for it (`app/StepUp.tsx`), so
 * nothing here draws its own. An account without both caps refuses every
 * request to spend, and the table says so rather than leave a gap.
 */

import { useCallback, useEffect, useId, useMemo, useState } from "react";
import {
  Button,
  Callout,
  FormField,
  IconButton,
  Input,
  Money,
  MoneyField,
  parseMoney,
  Section,
  Stack,
  Tag,
  Text,
} from "@crewlethq/ui";
import { PlusGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import "~/styles/growth.css";
import { SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import type { GrowthSpend } from "~/contract/growth.ts";
import { getConfig } from "~/protocol/configWrite.ts";
import { AccountList } from "./AccountList.tsx";
import {
  capsPlan,
  checkRows,
  liveCurrency,
  rowsOf,
  spendOf,
  type CapAccount,
  type CapRow,
} from "./caps.ts";

/** An account as a person tells it apart: its name, else its ID, and the ID too where another has the same name. */
function nameOf(account: CapAccount, among: readonly CapAccount[]): string {
  if (!account.name.trim()) return account.id;
  const twin = among.some((other) => other.id !== account.id && other.name === account.name);
  return twin ? `${account.name} (${account.id})` : account.name;
}
import { useSettingWrite } from "~/lib/useSettingWrite.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { growthProductName, spendCapsNote } from "./words.ts";

const REREAD = "Reload to see the caps as they are now.";

/**
 * A spending product's caps on its Settings tab, read from the company
 * document as it stands and read again once a saved change is applied.
 */
export function SpendCapsSection({
  kind,
  accounts,
}: {
  kind: string;
  accounts: readonly CapAccount[];
}) {
  const [spend, setSpend] = useState<GrowthSpend | null>(null);
  const [failed, setFailed] = useState("");
  const read = useCallback(
    (signal?: AbortSignal) => {
      getConfig(signal).then(
        (answer) => {
          if (signal?.aborted) return;
          if (answer.kind === "document") setSpend(spendOf(answer.document, kind));
          else
            setFailed(
              "The company's settings could not be read, so its spending caps are not shown.",
            );
        },
        () =>
          !signal?.aborted &&
          setFailed(
            "The company's settings could not be read, so its spending caps are not shown.",
          ),
      );
    },
    [kind],
  );
  useEffect(() => {
    const controller = new AbortController();
    read(controller.signal);
    return () => controller.abort();
  }, [read]);
  return (
    <Section spacing="tight" title="Spending caps">
      {failed ? (
        <Text as="p" tone="secondary">
          {failed}
        </Text>
      ) : spend === null ? (
        <Text as="p" tone="secondary">
          Reading the caps.
        </Text>
      ) : (
        <SpendCaps kind={kind} spend={spend} accounts={accounts} onSaved={() => read()} />
      )}
    </Section>
  );
}

export interface SpendCapsProps {
  /** The spending product's kind: `google_ads`, `apple_ads`, `meta_ads`. */
  kind: string;
  spend: GrowthSpend;
  /** The accounts the product's connections reach, to offer and to check currencies against. */
  accounts: readonly CapAccount[];
  /** Called once this node applies a saved change. */
  onSaved: () => void;
}

/** The caps as they stand, and the button that opens the form. */
export function SpendCaps({ kind, spend, accounts, onSaved }: SpendCapsProps) {
  const access = useConfigWriteAccess();
  const [editing, setEditing] = useState(false);
  const rows = spend.accounts ?? [];
  const product = growthProductName(kind);
  const named = (id: string) => accounts.find((account) => account.id === id)?.name.trim();
  const uncapped = accounts.filter((account) => !rows.some((row) => row.account === account.id));
  return (
    <Stack gap={3}>
      <Text as="p" tone="secondary">
        {spendCapsNote(kind)} An account without both caps refuses every request to spend; reading,
        changes that lower spending and pausing still work.
      </Text>
      {rows.length === 0 ? (
        <Text as="p">
          No account has spending caps, so agents cannot ask to spend on {product}.
        </Text>
      ) : (
        <AccountList
          label={`Spending caps for ${product}`}
          rows={rows.map((row) => ({
            key: row.account,
            name: named(row.account) ?? "",
            id: row.account,
            facts: [
              <>Per change {capCell(row.per_change, row.currency)}</>,
              <>Daily {capCell(row.daily, row.currency)}</>,
              ...refusedFact(row.currency, liveCurrency(accounts, row.account)),
            ],
          }))}
        />
      )}
      {uncapped.length > 0 ? (
        <Callout variant="info">
          {uncapped.length === 1
            ? `${nameOf(uncapped[0]!, accounts)} has no caps, so agents cannot ask to spend on it.`
            : `${uncapped.length} accounts have no caps, so agents cannot ask to spend on them.`}
        </Callout>
      ) : null}
      <div>
        <Button
          variant="secondary"
          size="small"
          tooltip
          disabledReason={access.can ? undefined : access.reason}
          onClick={() => setEditing(true)}
        >
          Set spending caps
        </Button>
      </div>
      {editing ? (
        <CapsForm
          kind={kind}
          spend={spend}
          accounts={accounts}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            onSaved();
          }}
        />
      ) : null}
    </Stack>
  );
}

/** A cap as the document holds it, drawn in its currency, or the gap it leaves; never a bare number. */
function capCell(amount: string | undefined, currency: string | undefined) {
  if (amount === undefined || amount.trim() === "") return <Text tone="secondary">Not set</Text>;
  const code = (currency ?? "").trim().toUpperCase();
  const read = /^[A-Z]{3}$/.test(code) ? parseMoney(amount, code) : null;
  return read && "micros" in read ? (
    <Money micros={read.micros} currency={code} />
  ) : (
    <Text tone="secondary">set, but not readable as an amount</Text>
  );
}

/** Why caps stored in no currency, or in one the account is not in, refuse every request to spend. */
function refusedFact(stored: string | undefined, live: string) {
  const code = (stored ?? "").trim().toUpperCase();
  if (!/^[A-Z]{3}$/.test(code))
    return [
      <Tag key="refused" size="sm" variant="warning">
        No currency: spending refused
      </Tag>,
    ];
  if (live !== "" && live !== code)
    return [
      <Tag key="refused" size="sm" variant="warning">
        The account is in {live}: spending refused
      </Tag>,
    ];
  return [];
}

let fresh = 0;

function CapsForm({
  kind,
  spend,
  accounts,
  onClose,
  onSaved,
}: {
  kind: string;
  spend: GrowthSpend;
  accounts: readonly CapAccount[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const product = growthProductName(kind);
  // THE PAGE'S OWN IDS for the rows' headings and the add button, focused when a row goes.
  const base = useId();
  const [rows, setRows] = useState<CapRow[]>(() => rowsOf(spend, accounts));
  const write = useSettingWrite({ reread: REREAD, onApplied: onSaved });
  const { problems, ok } = useMemo(() => checkRows(rows, accounts), [rows, accounts]);
  const set = (key: string, change: Partial<CapRow>) =>
    setRows((current) => current.map((row) => (row.key === key ? { ...row, ...change } : row)));
  const add = (account?: CapAccount) => {
    fresh += 1;
    setRows((current) => [
      ...current,
      {
        key: `new-${fresh}`,
        account: account?.id ?? "",
        currency: account?.currency.trim().toUpperCase() ?? "",
        perChange: "",
        daily: "",
        stored: false,
        fixed: account !== undefined,
        // A VENDOR THAT REPORTS NO CURRENCY leaves it for the person to type.
        currencyFixed:
          account !== undefined && /^[A-Z]{3}$/.test(account.currency.trim().toUpperCase()),
      },
    ]);
  };
  const offered = accounts.filter(
    (account) => !rows.some((row) => row.account.trim() === account.id),
  );
  return (
    <Modal
      open
      title={`Spending caps for ${product}`}
      size="lg"
      onClose={onClose}
      closeDisabledReason={write.busy ? "Saving the caps." : undefined}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={write.busy}
            disabledReason={ok ? undefined : "Correct the marked fields first."}
            onClick={() => write.submit(capsPlan(kind, product, spend.accounts ?? [], rows))}
          >
            Save caps
          </Button>
        </>
      }
    >
      <Stack gap={4}>
        <Text as="p" tone="secondary">
          {spendCapsNote(kind)} Raising a cap asks you to confirm it is you.
        </Text>
        {rows.map((row) => {
          const problem = problems[rows.indexOf(row)] ?? {};
          const known = accounts.find((account) => account.id === row.account.trim());
          const currency = row.currency.trim().toUpperCase();
          // Until the row names its currency the amounts cannot be read, so they wait on it.
          const priced = /^[A-Z]{3}$/.test(currency);
          return (
            <section
              key={row.key}
              role="group"
              aria-labelledby={`${base}-${row.key}`}
              className="growth-caps__row"
            >
              <div className="growth-caps__head">
                <h3 id={`${base}-${row.key}`} className="growth-caps__legend">
                  {known?.name.trim()
                    ? `${known.name} (${known.id})`
                    : row.account.trim() || "New account"}
                </h3>
                <IconButton
                  variant="ghost"
                  size="sm"
                  label={`Remove the caps of ${known ? nameOf(known, accounts) : row.account.trim() || "this account"}`}
                  icon={<XGlyph />}
                  onClick={() => {
                    setRows((current) => current.filter((other) => other.key !== row.key));
                    requestAnimationFrame(() => document.getElementById(`${base}-add`)?.focus());
                  }}
                />
              </div>
              {row.fixed && row.currencyFixed ? null : (
                <div className="growth-caps__fields">
                  {row.fixed ? null : (
                    <FormField label="Account ID" error={problem.account}>
                      {({ id, describedBy }) => (
                        <Input
                          id={id}
                          aria-describedby={describedBy}
                          value={row.account}
                          spellCheck={false}
                          autoComplete="off"
                          onChange={(event) => set(row.key, { account: event.target.value })}
                        />
                      )}
                    </FormField>
                  )}
                  {row.currencyFixed ? null : (
                    <FormField label="Currency" error={problem.currency}>
                      {({ id, describedBy }) => (
                        <Input
                          id={id}
                          aria-describedby={describedBy}
                          value={row.currency}
                          maxLength={3}
                          spellCheck={false}
                          autoComplete="off"
                          onChange={(event) =>
                            set(row.key, { currency: event.target.value.toUpperCase() })
                          }
                        />
                      )}
                    </FormField>
                  )}
                </div>
              )}
              {/* A FIXED PART'S PROBLEM has no field to show it on. */}
              {row.fixed && problem.account ? (
                <Callout variant="warning">{problem.account}</Callout>
              ) : null}
              {row.currencyFixed && problem.currency ? (
                <Callout variant="warning">{problem.currency}</Callout>
              ) : null}
              <div className="growth-caps__fields">
                <FormField
                  label="Per change"
                  helper="The most one change may add to the daily budgets."
                  error={problem.perChange}
                >
                  {({ id, describedBy }) => (
                    <MoneyField
                      id={id}
                      aria-describedby={describedBy}
                      value={row.perChange}
                      currency={priced ? currency : "XXX"}
                      disabled={!priced}
                      error={problem.perChange !== undefined}
                      onValueChange={(value) => set(row.key, { perChange: value })}
                    />
                  )}
                </FormField>
                <FormField
                  label="Daily"
                  helper="The ceiling on the daily budgets after any change."
                  error={problem.daily}
                >
                  {({ id, describedBy }) => (
                    <MoneyField
                      id={id}
                      aria-describedby={describedBy}
                      value={row.daily}
                      currency={priced ? currency : "XXX"}
                      disabled={!priced}
                      error={problem.daily !== undefined}
                      onValueChange={(value) => set(row.key, { daily: value })}
                    />
                  )}
                </FormField>
              </div>
              {problem.note ? (
                <Text as="p" tone="secondary">
                  {problem.note}
                </Text>
              ) : null}
            </section>
          );
        })}
        <div className="growth-caps__add">
          {offered.map((account) => (
            <Button
              key={account.id}
              size="small"
              variant="secondary"
              leadingIcon={<PlusGlyph />}
              onClick={() => add(account)}
            >
              {nameOf(account, accounts)}
            </Button>
          ))}
          <Button
            id={`${base}-add`}
            size="small"
            variant="ghost"
            leadingIcon={<PlusGlyph />}
            onClick={() => add()}
          >
            Another account
          </Button>
        </div>
        <SettingWriteStatus
          write={write}
          unchanged="The caps are already so."
          // RELOAD READS THE CAPS AGAIN, so the next save is made over what is there now.
          onReload={onSaved}
          answersWarning
        />
      </Stack>
    </Modal>
  );
}
