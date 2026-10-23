/**
 * Settings › Sign-in policy: how a person signs in, as this node enforces it.
 *
 * # What the engine enforces, on the node that answered
 *
 * The answer (`sign_in_policy`, an administrator's) is the posture the sign-in
 * code itself reads: whether a second factor is required, the shortest
 * password, how long a session lives from sign-in and unused, and how long a
 * proof of identity authorises a sensitive gesture, each with its default
 * applied where the node's file writes none. It is TIER A, written in each
 * node's own `crewlet.yaml` (`api.auth`), so it is THIS node's and nodes of
 * one fleet may differ: the page names the node rather than presenting the
 * reading as the company's. Read only: it changes with an edit to that file
 * and a restart.
 *
 * # Drawn as every read-only settings page is
 *
 * Each group is a card holding a [SettingsTable], the shape Settings › Limits
 * takes: the setting, its key under `api.auth` in the node's file, the value
 * enforced, and where the value comes from. The session's idle limit is the
 * one value no file sets (an engine constant, see `queries.SignInPolicy`), and
 * its source says so rather than pointing at a key that does not exist.
 */

import { Card, InlineCode, SettingsTable } from "@crewlethq/ui";
import { ClockGlyph, ShieldUserGlyph } from "@crewlethq/icons/glyphs";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtSetting, plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";

/** Where a value set in the node's file comes from. */
const FROM_FILE = "crewlet.yaml";

export function SignInPolicy() {
  const { data, loading, error, refusal, detail } = useQuery("sign_in_policy", undefined, {
    pollMs: 300_000,
  });
  return (
    <>
      <PageNote>
        {data ? (
          <>
            Set in this node&rsquo;s crewlet.yaml (<InlineCode>{data.node}</InlineCode>, under
            api.auth), and changed with a restart.
          </>
        ) : (
          "Set in each node's crewlet.yaml (under api.auth), and changed with a restart."
        )}
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && (
          <>
            <Card padding="none">
              <Card.Header icon={<ShieldUserGlyph size="sm" />}>
                <Card.Title id="sign-in-signing">Signing in</Card.Title>
        {/* In the section body's own rhythm, as every Settings page stacks. */}
              </Card.Header>
              <SettingsTable aria-labelledby="sign-in-signing">
                <SettingsTable.Row
                  label="Two-step verification"
                  settingKey="api.auth.totp"
                  source={FROM_FILE}
                >
                  {data.policy.second_factor === "required"
                    ? "Required for every password sign-in"
                    : "Optional, each person chooses"}
                </SettingsTable.Row>
                <SettingsTable.Row
                  label="Shortest password"
                  settingKey="api.auth.min_password_length"
                  source={FROM_FILE}
                >
                  {plural(data.policy.min_password_length, "character")}
                </SettingsTable.Row>
              </SettingsTable>
            </Card>
            <Card padding="none">
              <Card.Header icon={<ClockGlyph size="sm" />}>
                <Card.Title id="sign-in-sessions">Sessions</Card.Title>
              </Card.Header>
              <SettingsTable aria-labelledby="sign-in-sessions">
                <SettingsTable.Row
                  label="Longest a session lives"
                  settingKey="api.auth.session.absolute"
                  source={FROM_FILE}
                >
                  {fmtSetting(data.policy.session_absolute_seconds)}
                </SettingsTable.Row>
                <SettingsTable.Row label="Signed out when unused for" source="Fixed by the engine">
                  {fmtSetting(data.policy.session_idle_seconds)}
                </SettingsTable.Row>
                <SettingsTable.Row
                  label="A sensitive change asks again after"
                  settingKey="api.auth.session.step_up"
                  source={FROM_FILE}
                >
                  {fmtSetting(data.policy.step_up_seconds)}
                </SettingsTable.Row>
              </SettingsTable>
            </Card>
          </>
        )}
      </QueryState>
    </>
  );
}
