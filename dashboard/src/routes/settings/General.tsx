/**
 * Settings › General: the company-wide basics every team and seat runs on,
 * today the company's clock.
 *
 * THE CLOCK IS THE ENGINE'S ROW of `timezone` (`settings`, an administrator's
 * read), beside its default and changed in place. A move shifts every budget
 * window and every schedule without a zone of its own, which the engine's
 * check says before the save asks to go ahead. A reader who may not read the
 * document sees the zone the org projection carries, with the pencil held and
 * its reason.
 */

import { Card, PageNote, SettingsTable } from "@crewlethq/ui";
import { ClockGlyph } from "@crewlethq/icons/glyphs";
import { UnreadSetting } from "~/components/SettingEditor.tsx";
import { TextSetting } from "~/components/TextSetting.tsx";
import { useCompanyZone } from "~/lib/companyDay.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";

export function GeneralSettings() {
  const viewer = useViewer();
  const zone = useCompanyZone();
  const settings = useQuery("settings", undefined, { enabled: viewer.admin });
  const row = settings.data?.settings.find((r) => r.key === "timezone");
  return (
    <>
      <PageNote>The company-wide basics every team and seat runs on.</PageNote>
      <Card as="section" padding="none">
        <Card.Header
          icon={<ClockGlyph size="sm" />}
          count={1}
          subtitle="Where every day, week and month begins: budgets, due dates, and schedules without a zone of their own."
        >
          <Card.Title id="company-clock">Company clock</Card.Title>
        </Card.Header>
        <SettingsTable aria-labelledby="company-clock">
          {row ? (
            <TextSetting
              row={row}
              label="Time zone"
              area="General"
              reread="Reload to see the company clock as it is now."
              onApplied={settings.refetch}
            />
          ) : (
            <UnreadSetting label="Time zone" settingKey="timezone">
              {zone || "UTC"}
            </UnreadSetting>
          )}
        </SettingsTable>
      </Card>
    </>
  );
}
