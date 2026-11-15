/**
 * The five fields of the cron grammar a schedule is written in: COPIES of
 * `internal/cron`'s field table, each field's least and greatest value and the
 * names it reads as values, held by `internal/cron`'s
 * `TestTheDashboardReadsTheFieldsTheGrammarHas`.
 *
 * THE ENGINE OWNS THE GRAMMAR. It refuses a schedule whose expression it
 * cannot parse, and the schedule form checks the same rules before anything
 * is sent, so a mistake is named beside the field while it is typed. A second
 * copy of the grammar kept beside the form had already drifted: it read a
 * month's name as a weekday's and refused `6-7`, the weekend the engine's own
 * documentation writes. The bounds and the names are therefore declared here,
 * where a gate holds them to the engine's.
 *
 * A NAME'S VALUE IS ITS PLACE: `names[i]` is `min + i`, so `jan` is 1 and
 * `sun` is 0. Day-of-week takes 0 to 7, both ends Sunday.
 */
export const CRON_FIELDS = {
  minute: { min: 0, max: 59, names: [] },
  hour: { min: 0, max: 23, names: [] },
  "day-of-month": { min: 1, max: 31, names: [] },
  month: {
    min: 1,
    max: 12,
    names: ["jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"],
  },
  "day-of-week": { min: 0, max: 7, names: ["sun", "mon", "tue", "wed", "thu", "fri", "sat"] },
} as const;
