/**
 * Settings › Sign-in policy: the company's `sign_in` block, changed in place
 * through the shared setting editor, beside this node's limits (ADR-0152).
 */

import { Callout, Card, InlineCode, PageNote, SettingsTable } from "@crewlethq/ui";
import { ClockGlyph, ServerGlyph, ShieldUserGlyph } from "@crewlethq/icons/glyphs";
import { LimitSetting } from "~/components/LimitSetting.tsx";
import { TextSetting } from "~/components/TextSetting.tsx";
import { QueryState } from "~/components/common.tsx";
import type { SignInPolicyAnswer } from "~/contract/signin.ts";
import { fmtSetting, plural } from "~/lib/format.ts";
import type { LimitRow } from "~/lib/limits.ts";
import { FROM_NODE_FILE } from "~/lib/nodeFile.ts";
import { useQuery } from "~/lib/useQuery.ts";

const AREA = "Sign-in policy";
const REREAD = "Reload to see the sign-in policy as it is now.";

/** The company document's keys, as the engine's rows name them. */
const KEYS = {
  password: "sign_in.min_password_length",
  lifetime: "sign_in.session_lifetime_seconds",
  idle: "sign_in.idle_timeout_seconds",
  stepUp: "sign_in.step_up_seconds",
} as const;

const characters = (n: number) => plural(n, "character");
const factorWords = (required: boolean) =>
  required ? "Required at every sign-in" : "Optional, each person chooses";

export function SignInPolicy() {
  const { data, loading, error, refusal, detail, refetch } = useQuery("sign_in_policy", undefined, {
    pollMs: 300_000,
  });
  return (
    <>
      <PageNote>
        How the company&rsquo;s people sign in. A change applies on every node at once, and each
        node holds it within the limits its operator set
        {data ? (
          <>
            {" "}
            (shown here for node <InlineCode>{data.node}</InlineCode>)
          </>
        ) : null}
        .
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && <Policy answer={data} onApplied={refetch} />}
      </QueryState>
    </>
  );
}

function Policy({ answer, onApplied }: { answer: SignInPolicyAnswer; onApplied: () => void }) {
  const row = (key: string): LimitRow | undefined => answer.settings.find((r) => r.key === key);
  const password = row(KEYS.password);
  const lifetime = row(KEYS.lifetime);
  const idle = row(KEYS.idle);
  const stepUp = row(KEYS.stepUp);
  const held = heldWords(answer);
  const limit = (cap: LimitRow | undefined, label: string, words?: (n: number) => string) =>
    cap && (
      <LimitSetting
        cap={cap}
        label={label}
        area={AREA}
        reread={REREAD}
        onApplied={onApplied}
        {...(words ? { words } : {})}
      />
    );
  return (
    <>
      <Card padding="none">
        <Card.Header icon={<ShieldUserGlyph size="sm" />}>
          <Card.Title id="sign-in-signing">Signing in</Card.Title>
        </Card.Header>
        <SettingsTable aria-labelledby="sign-in-signing">
          <TextSetting
            row={answer.second_factor_setting}
            label="Two-step verification"
            area={AREA}
            reread={REREAD}
            onApplied={onApplied}
            words={(v) => factorWords(v === true)}
            switchWords={{ on: "Required", off: "Optional" }}
            helper="Required asks every password sign-in for a code from an authenticator app. Optional lets a password alone sign people in."
          />
          {limit(password, "Shortest password", characters)}
        </SettingsTable>
      </Card>
      <Card padding="none">
        <Card.Header
          icon={<ClockGlyph size="sm" />}
          subtitle="A shorter limit ends sessions past it at their next request. A longer one applies to people who sign in after it."
        >
          <Card.Title id="sign-in-sessions">Sessions</Card.Title>
        </Card.Header>
        <SettingsTable aria-labelledby="sign-in-sessions">
          {limit(lifetime, "Longest a session lives")}
          {limit(idle, "Signed out when unused for")}
          {limit(stepUp, "A sensitive change asks again after")}
        </SettingsTable>
      </Card>
      {held.length > 0 && (
        <Callout variant="info" role="status">
          <span>
            Node <InlineCode>{answer.node}</InlineCode> holds {held.join(", ")}, the limit its
            operator set.
          </span>
        </Callout>
      )}
      <Card padding="none">
        <Card.Header
          icon={<ServerGlyph size="sm" />}
          subtitle="Set in the node’s configuration file. The company’s choices stay within them."
        >
          <Card.Title id="sign-in-limits">This node&rsquo;s limits</Card.Title>
        </Card.Header>
        <SettingsTable aria-labelledby="sign-in-limits">
          <SettingsTable.Row
            label="Two-step verification"
            settingKey="api.auth.limits.second_factor"
            source={FROM_NODE_FILE}
          >
            {answer.limits.second_factor === "required"
              ? "Always required"
              : "The company may make it optional"}
          </SettingsTable.Row>
          <SettingsTable.Row
            label="Shortest password the company may allow"
            settingKey="api.auth.limits.min_password_length"
            source={FROM_NODE_FILE}
          >
            {characters(answer.limits.min_password_length)}
          </SettingsTable.Row>
          <SettingsTable.Row
            label="Longest session"
            settingKey="api.auth.limits.max_session_lifetime"
            source={FROM_NODE_FILE}
          >
            {fmtSetting(answer.limits.max_session_lifetime_seconds)}
          </SettingsTable.Row>
          <SettingsTable.Row
            label="Longest unused session"
            settingKey="api.auth.limits.max_idle_timeout"
            source={FROM_NODE_FILE}
          >
            {fmtSetting(answer.limits.max_idle_timeout_seconds)}
          </SettingsTable.Row>
          <SettingsTable.Row
            label="Longest step-up window"
            settingKey="api.auth.limits.max_step_up"
            source={FROM_NODE_FILE}
          >
            {fmtSetting(answer.limits.max_step_up_seconds)}
          </SettingsTable.Row>
        </SettingsTable>
      </Card>
      <PageNote>
        API tokens, the keyring, who may hold admin on a node and how long the sign-in audit is kept
        stay in each node&rsquo;s configuration file: a node needs them before it can read the
        company, and the company must not be able to change them.
      </PageNote>
    </>
  );
}

/** Each company value this node enforces lower than the document says, in words. */
export function heldWords(answer: SignInPolicyAnswer): string[] {
  const out: string[] = [];
  const value = (key: string) => answer.settings.find((r) => r.key === key)?.value;
  if (answer.second_factor_setting.value === false && answer.policy.second_factor === "required") {
    out.push("two-step verification to required");
  }
  const password = value(KEYS.password);
  if (password !== undefined && answer.policy.min_password_length > password) {
    out.push(`the shortest password to ${characters(answer.policy.min_password_length)}`);
  }
  const lower = (key: string, enforced: number, what: string) => {
    const v = value(key);
    if (v !== undefined && enforced < v) out.push(`${what} to ${fmtSetting(enforced)}`);
  };
  lower(KEYS.lifetime, answer.policy.session_absolute_seconds, "sessions");
  lower(KEYS.idle, answer.policy.session_idle_seconds, "an unused session");
  lower(KEYS.stepUp, answer.policy.step_up_seconds, "the step-up window");
  return out;
}
