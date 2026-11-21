/**
 * A held growth call as the decision a person makes: allow once or deny an
 * agent's ask to start or raise spend (`ad_spend`), publish to customers
 * (`public_post`) or manage the vendor account (`account_admin`).
 *
 * WHAT IS ALLOWED IS WHAT IS SHOWN. Every name and text is drawn whole and
 * wrapped, never cut, and money is drawn only through the kit's `Money`, in
 * the account's own currency: a person must see exactly what they allow.
 * Somebody's words (a review, a reply, a campaign name, the agent's reason, a
 * vendor's finding) are plain text, never Markdown.
 */

import type { ReactNode } from "react";
import {
  Money,
  ProposalCard,
  Rating,
  SegmentedMeter,
  formatMoney,
  type ChangeListRow,
  type ProposalDecisionRefusal,
  type ProposalState,
} from "@crewlethq/ui";
import type {
  GrowthCard,
  GrowthCardAct,
  GrowthCardChange,
  GrowthCardMoney,
} from "~/contract/growth.ts";
import { growthProductName, spendCapsNote } from "./words.ts";
import "~/styles/growth.css";

export interface GrowthDecisionCardProps {
  card: GrowthCard;
  /** The engine's own question for the call (`held_call.question`), the card's title. */
  question: string;
  state: ProposalState;
  /** The note the person sends back with either answer. */
  note: string;
  onNote: (note: string) => void;
  onAllow: () => void;
  onDeny: () => void;
  /** Why the last answer was refused while the call still waits. */
  refusal?: ProposalDecisionRefusal | undefined;
  /** Why Allow once is held for now (a denial on its way), said beside it. */
  held?: string | undefined;
  /** What the applied call did, or why it failed, in the engine's words. */
  receipt?: ReactNode;
  failure?: ReactNode;
}

export function GrowthDecisionCard({
  card,
  question,
  state,
  note,
  onNote,
  onAllow,
  onDeny,
  refusal,
  held,
  receipt,
  failure,
}: GrowthDecisionCardProps) {
  // ANY ACT THAT CANNOT BE UNDONE waits for a tick: an account act, or a live target's delete.
  const danger = card.acts.some((act) => act.danger === true);
  // AN AMOUNT WITHOUT ITS CURRENCY cannot be read, so it cannot be allowed here.
  const unpriced = pricedIn(card) === undefined && carriesMoney(card);
  return (
    <ProposalCard
      title={titleOf(card)}
      subtitle={growthProductName(card.product)}
      state={state}
      stepUp
      expiresAt={card.lapses_at}
      tone={danger ? "danger" : "default"}
      confirmation={
        danger
          ? { label: `I understand this cannot be undone in ${growthProductName(card.product)}.` }
          : undefined
      }
      summary={summaryOf(card, question)}
      caution={findingsOf(card)}
      changes={growthCardRows(card)}
      changesHeading={headingOf(card, state)}
      note={noteOf(card)}
      approveLabel="Allow once"
      labels={{ decline: "Deny" }}
      reply={{
        label: "Note to the agent",
        value: note,
        onValueChange: onNote,
        help: "Sent with your answer.",
      }}
      approveBlocked={
        held ??
        (unpriced
          ? `${growthProductName(card.product)} did not say which currency the amounts are in, so this cannot be allowed here.`
          : undefined)
      }
      decisionRefusal={refusal}
      receipt={receipt}
      failure={failure}
      onApprove={onAllow}
      onDecline={onDeny}
    />
  );
}

/**
 * The card's title: what is asked, short enough for the card's one line. The
 * engine's own question names the target too, and is the summary, wrapped.
 */
function titleOf(card: GrowthCard): string {
  const first = card.acts[0]?.summary ?? "";
  return card.acts.length > 1 ? `${first} (${card.acts.length})` : first;
}

/** The account the call acts on, as the vendor names it, with the currency and zone its figures are in. */
export function accountLine(card: GrowthCard): string {
  const { account } = card;
  const name = account.account_name.trim() || account.account;
  const id =
    account.account_name.trim() && account.account_name.trim() !== account.account
      ? ` (${account.account})`
      : "";
  return [`${name}${id}`, account.currency, account.time_zone].filter(Boolean).join(" · ");
}

/** Whether a public act publishes text (a reply, a post), rather than changing what customers see. */
function posts(card: GrowthCard): boolean {
  return card.act === "public_post" && card.acts.every((act) => act.text !== undefined);
}

function summaryOf(card: GrowthCard, question: string): string {
  if (card.act !== "public_post") return question;
  const n = card.acts.length;
  const product = growthProductName(card.product);
  const together = `allowed or denied ${n === 1 ? "as one" : "together"}`;
  return posts(card)
    ? `${question} ${n === 1 ? "It is" : `All ${n} are`} published on ${product}, where anyone can read ${n === 1 ? "it" : "them"}, and ${together}.`
    : `${question} Customers see ${n === 1 ? "it" : `all ${n}`} on ${product} once ${n === 1 ? "it runs" : "they run"}, and ${n === 1 ? "it is" : "they are"} ${together}.`;
}

function findingsOf(card: GrowthCard): ReactNode {
  if (card.findings.length === 0) return undefined;
  return (
    <>
      {`${growthProductName(card.product)} flagged ${card.findings.length === 1 ? "this" : "these"} when it checked the change:`}
      <ul className="growth-decision__findings">
        {card.findings.map((finding, index) => (
          <li key={index}>{finding}</li>
        ))}
      </ul>
    </>
  );
}

function headingOf(card: GrowthCard, state: ProposalState): string | undefined {
  if (card.act !== "public_post" || card.acts.length === 1) return undefined;
  const n = card.acts.length;
  if (!posts(card)) return state === "applied" ? `What changed (${n})` : `What will change (${n})`;
  return state === "applied" ? `What was published (${n})` : `What will be published (${n})`;
}

function noteOf(card: GrowthCard): string {
  const moved = posts(card)
    ? "If what it answers changes before it runs, nothing runs and the agent asks again."
    : card.money
      ? "If anything it changes, the caps or the account's currency move before it runs, nothing runs and the agent asks again."
      : "If anything it changes moves before it runs, nothing runs and the agent asks again.";
  const others =
    card.other_open_asks > 0
      ? ` ${card.other_open_asks === 1 ? "Another open ask changes" : `${card.other_open_asks} other open asks change`} the same thing: allowing this one voids ${card.other_open_asks === 1 ? "it" : "them"}.`
      : "";
  return `Runs once. ${moved}${others}`;
}

/** The currency every amount on the card is in: the spend figures', else the account's; none when it is not a code. */
function pricedIn(card: GrowthCard): string | undefined {
  const code = (card.money ? card.money.currency : (card.account.currency ?? ""))
    .trim()
    .toUpperCase();
  return /^[A-Z]{3}$/.test(code) ? code : undefined;
}

function carriesMoney(card: GrowthCard): boolean {
  return card.money !== null || card.acts.some((act) => act.changes.some((change) => change.money));
}

/** Every row the card draws, in order: each act's changes or text, the money against the caps, then the agent's reason. */
export function growthCardRows(card: GrowthCard): ChangeListRow[] {
  const currency = pricedIn(card);
  const rows: ChangeListRow[] = [
    { kind: "fact", label: "Account", value: accountLine(card), block: true },
  ];
  const many = card.acts.length > 1;
  card.acts.forEach((act, index) => {
    // A PUBLIC ACT WITHOUT TEXT (a live listing's edit, a delete) is drawn as the fields it changes.
    if (card.act === "public_post" && act.text !== undefined)
      rows.push(publicRow(act, many ? `${index + 1} of ${card.acts.length}` : ""));
    else rows.push(...changeRows(act, currency));
  });
  if (card.money && currency) rows.push(moneyRow(card.money, currency, card.product));
  if (card.reason.trim()) {
    rows.push({ kind: "fact", label: "The agent's reason", value: card.reason, block: true });
  }
  return rows;
}

function valueOf(
  change: GrowthCardChange,
  side: "before" | "after",
  currency: string | undefined,
): ReactNode {
  const value = change[side];
  if (!change.money) return value;
  // NEVER BARE MICROS: a figure a million times the amount is worse than none.
  return currency ? (
    <Money micros={value} currency={currency} />
  ) : (
    "An amount in an unnamed currency"
  );
}

function changeRows(act: GrowthCardAct, currency: string | undefined): ChangeListRow[] {
  const target: ChangeListRow = { kind: "fact", label: "Target", value: act.target, block: true };
  return [
    target,
    ...act.changes.map((change): ChangeListRow => {
      if (change.before === "")
        return { kind: "added", label: change.label, to: valueOf(change, "after", currency) };
      if (change.after === "")
        return { kind: "removed", label: change.label, from: valueOf(change, "before", currency) };
      return {
        kind: "changed",
        label: change.label,
        from: valueOf(change, "before", currency),
        to: valueOf(change, "after", currency),
      };
    }),
  ];
}

function publicRow(act: GrowthCardAct, place: string): ChangeListRow {
  const text = act.text ?? "";
  const length = [...text].length;
  // A REPLY THAT REPLACES ONE shows the words it takes down, whole.
  const replaced = act.changes.find((change) => !change.money && change.before !== "")?.before;
  const head = {
    label: place ? `${act.summary}, ${place}` : act.summary,
    block: true,
    note: act.text_limit
      ? `${length.toLocaleString()} of ${act.text_limit.toLocaleString()} characters`
      : undefined,
  };
  const post = (
    <div className="growth-decision__post">
      <div className="growth-decision__target">
        {/* The figure beside the stars: on a tinted row the empty ones are faint. */}
        {act.target_rating === undefined ? null : (
          <Rating value={act.target_rating} size="xs" showValue />
        )}
        <span className="growth-decision__author">{act.target_author ?? act.target}</span>
      </div>
      {act.target_excerpt ? <q className="growth-decision__excerpt">{act.target_excerpt}</q> : null}
      <p className="growth-decision__text">{text}</p>
    </div>
  );
  return replaced === undefined
    ? { kind: "added", ...head, to: post }
    : {
        kind: "changed",
        ...head,
        from: <p className="growth-decision__text">{replaced}</p>,
        to: post,
      };
}

function moneyRow(money: GrowthCardMoney, currency: string, product: string): ChangeListRow {
  const now = BigInt(money.daily_micros);
  const after = BigInt(money.daily_after_micros);
  const delta = BigInt(money.delta_micros);
  const cap = BigInt(money.daily_cap_micros);
  // A LOWERING draws what stays set, and nothing added.
  const lowers = delta < 0n;
  const say = (micros: bigint, sign?: "exceptZero") =>
    formatMoney(micros, currency, undefined, { signDisplay: sign });
  // The bar's geometry only: a share of the cap, never a figure anybody reads.
  const share = (micros: bigint) => (cap > 0n ? Number((micros * 10_000n) / cap) / 100 : 0);
  const adds = lowers ? 0n : delta;
  return {
    kind: "fact",
    label: "Daily budgets on the account",
    block: true,
    value: (
      <div className="growth-decision__capacity">
        <span className="growth-decision__figures">
          <Money micros={now} currency={currency} /> set,{" "}
          <Money micros={after} currency={currency} /> with this change, of the{" "}
          <Money micros={cap} currency={currency} /> daily cap
        </span>
        <SegmentedMeter
          segments={[
            { id: "set", value: share(lowers ? after : now), tone: "info", label: "set" },
            { id: "change", value: share(adds), tone: "warning", label: "this change" },
          ]}
          total={100}
          label={`${say(now)} set, ${say(delta, "exceptZero")} with this change: ${say(after)} of the ${say(cap)} daily cap`}
        />
      </div>
    ),
    note: `${
      lowers
        ? `This change lowers the daily budgets by ${say(-delta)}.`
        : `This change adds ${say(delta)} of the ${say(BigInt(money.per_change_cap_micros))} one change may add.`
    } ${
      money.overdelivery
        ? `Caps limit the budgets agents may set. ${money.overdelivery}`
        : spendCapsNote(product)
    }`,
  };
}
