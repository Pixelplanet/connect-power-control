// What the Energy view works out in the browser: which fact a figure states,
// how a period is named, and the CSV download. Cost, days and night hours are
// worked out by the process, so every screen and the CSV file agree.
import { formatDate, type Translate } from '@prusa/connect-ui/i18n';

import type {
  EnergyFigures,
  EnergyPoint,
  EnergyQuery,
  EnergyReport,
  EnergySpan,
  EnergyTable,
  PowerApi,
} from './api';
import { saveInBrowser } from './download';
import { formatCost, formatEnergy } from './format';

export const SPANS: readonly EnergySpan[] = ['day', 'week', 'month', 'year'];

export const isSpan = (value: string | undefined): value is EnergySpan =>
  SPANS.some((span) => span === value);

/**
 * What a figure says. Three facts never share a number: nothing was seen,
 * the plug was on with no reading, and a reading, also a real zero.
 */
export type EnergyFact = 'none' | 'off' | 'notMeasured' | 'partly' | 'measured';

/** Below this share of the on time, a figure is marked as partly measured. */
const MEASURED_SHARE = 0.9;

export const factOf = (
  figures: Pick<EnergyFigures, 'observed_sec' | 'on_sec' | 'metered_sec'>,
): EnergyFact => {
  if (figures.observed_sec <= 0) return 'none';
  if (figures.on_sec <= 0) return 'off';
  if (figures.metered_sec <= 0) return 'notMeasured';

  return figures.metered_sec < MEASURED_SHARE * figures.on_sec
    ? 'partly'
    : 'measured';
};

/** Whether a fact comes with a figure to show. */
export const hasFigure = (fact: EnergyFact): boolean =>
  fact === 'partly' || fact === 'measured';

const FACT_KEYS: Readonly<Record<EnergyFact, string>> = {
  none: 'power.energy.cell.none',
  off: 'power.energy.cell.off',
  notMeasured: 'power.energy.cell.notMeasured',
  partly: 'power.energy.cell.partly',
  measured: 'power.energy.cell.partly',
};

/** The words for a fact: what stands in place of a figure, or beside one. */
export const factText = (t: Translate, fact: EnergyFact): string =>
  t(FACT_KEYS[fact]);

/** A print's fact: it was seen for as long as it ran. */
export const printFactOf = (print: {
  duration_sec: number;
  metered_sec: number;
}): EnergyFact => {
  const ran = Math.max(print.duration_sec, 1);

  return factOf({
    observed_sec: ran,
    on_sec: ran,
    metered_sec: print.metered_sec,
  });
};

// Every date on the Energy view is the site's wall clock, which the process
// sends as a day, an hour or a time with no offset. It is put on the UTC
// clock and drawn with the UTC zone, so the browser's own zone never moves it.
const SITE: Intl.DateTimeFormatOptions = { timeZone: 'UTC' };

/** A site day and hour as a date to hand to `formatDate` with `SITE`. */
export const siteDate = (day: string, hour = 0): Date => {
  const [year, month, date] = day.split('-').map(Number);

  return new Date(Date.UTC(year ?? 1970, (month ?? 1) - 1, date ?? 1, hour));
};

const drawn = (when: Date, options: Intl.DateTimeFormatOptions): string =>
  formatDate(when, { ...options, ...SITE });

/** A site wall-clock time such as "2026-11-18T14:05:00", as a person reads it. */
export const siteTime = (local: string): string =>
  drawn(new Date(`${local}Z`), { dateStyle: 'medium', timeStyle: 'short' });

/** An hour of the day, 0 to 23, as the reader's clock writes it. */
export const hourLabel = (hour: number): string =>
  drawn(new Date(Date.UTC(2000, 0, 1, hour)), {
    hour: '2-digit',
    minute: '2-digit',
  });

/**
 * The hours of the day that a report holds more than once. On the day the
 * clocks go back, one hour of the site's wall clock comes twice.
 */
export const repeatedHours = (
  points: readonly EnergyPoint[],
): ReadonlySet<number> => {
  const seen = new Set<number>();
  const twice = new Set<number>();
  for (const point of points) {
    if (seen.has(point.hour)) twice.add(point.hour);
    seen.add(point.hour);
  }

  return twice;
};

/** How far the site's clock is from UTC at a period's start, as "+02:00". */
const offsetOf = (start: string): string =>
  /[+-]\d{2}:\d{2}$/.exec(start)?.[0] ?? '+00:00';

/**
 * One period in full, for the readout: "Tue 18 Nov", "14:00", "November 2026".
 * `twice` says that the report holds this hour of the day two times. The
 * distance from UTC then tells the two apart: "02:00 (UTC+02:00)".
 */
export const periodLabel = (
  t: Translate,
  point: EnergyPoint,
  span: EnergySpan,
  twice = false,
): string => {
  if (span === 'day') {
    const hour = hourLabel(point.hour);

    return twice
      ? t('power.energy.chart.hourOffset', {
          hour,
          offset: offsetOf(point.start),
        })
      : hour;
  }
  if (span === 'year') {
    return drawn(siteDate(point.day), { month: 'long', year: 'numeric' });
  }

  return drawn(siteDate(point.day), {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
  });
};

/** One period in short, for the axis under the chart. */
export const axisLabel = (point: EnergyPoint, span: EnergySpan): string => {
  if (span === 'day') return hourLabel(point.hour);
  if (span === 'year') return drawn(siteDate(point.day), { month: 'short' });
  if (span === 'week') {
    return drawn(siteDate(point.day), { weekday: 'short', day: 'numeric' });
  }

  return drawn(siteDate(point.day), { day: 'numeric', month: 'short' });
};

/** The span a report covers: a day, a week from and to, a month or a year. */
export const spanLabel = (t: Translate, report: EnergyReport): string => {
  const first = siteDate(report.at);
  switch (report.span) {
    case 'day':
      return drawn(first, {
        weekday: 'short',
        day: 'numeric',
        month: 'short',
        year: 'numeric',
      });
    case 'week':
      return t('power.energy.span.range', {
        from: drawn(first, { day: 'numeric', month: 'short' }),
        to: drawn(siteDate(report.points.at(-1)?.day ?? report.at), {
          day: 'numeric',
          month: 'short',
          year: 'numeric',
        }),
      });
    case 'month':
      return drawn(first, { month: 'long', year: 'numeric' });
    case 'year':
      return drawn(first, { year: 'numeric' });
  }
};

/**
 * What the chart says about one period, for the readout and a screen reader.
 * A figure the meter covered only in part says so, as it does in the tables.
 * `twice` is passed on to `periodLabel`.
 */
export const readoutOf = (
  t: Translate,
  point: EnergyPoint,
  span: EnergySpan,
  currency: string,
  twice = false,
): string => {
  const period = periodLabel(t, point, span, twice);
  const fact = factOf(point);
  if (!hasFigure(fact)) {
    return t('power.energy.chart.readoutFact', {
      period,
      fact: factText(t, fact),
    });
  }
  const energy = formatEnergy(point.energy_wh);
  const partly = fact === 'partly';
  if (point.cost === null) {
    return t(
      partly
        ? 'power.energy.chart.readoutPartlyNoCost'
        : 'power.energy.chart.readoutNoCost',
      { period, energy },
    );
  }

  return t(
    partly ? 'power.energy.chart.readoutPartly' : 'power.energy.chart.readout',
    { period, energy, cost: formatCost(point.cost, currency) },
  );
};

/** The zone this browser's clock follows, '' when the browser does not say. */
export const browserZone = (): string => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    return '';
  }
};

/** How far `zone` is ahead of UTC now, as "UTC+01:00"; '' when unknown. */
const zoneOffset = (zone: string): string => {
  try {
    const part = new Intl.DateTimeFormat('en', {
      timeZone: zone,
      timeZoneName: 'longOffset',
    })
      .formatToParts(new Date())
      .find((entry) => entry.type === 'timeZoneName');
    const text = (part?.value ?? '').replace('GMT', 'UTC');

    return text === 'UTC' ? 'UTC+00:00' : text;
  } catch {
    return '';
  }
};

/** One time zone a person can pick: its name and how it reads in the list. */
export interface ZoneItem {
  value: string;
  label: string;
}

/**
 * Every time zone this browser knows, with UTC, each named with its offset
 * now: "Europe/Prague (UTC+01:00)". `also` are names that must be in the list
 * whatever the browser knows, such as the saved zone. Empty when the browser
 * cannot list its zones; the caller then lets a person type the name.
 */
export const zoneItems = (also: readonly string[]): ZoneItem[] => {
  let known: string[];
  try {
    known = Intl.supportedValuesOf('timeZone');
  } catch {
    return [];
  }
  const names = new Set(['UTC', ...known]);
  for (const name of also) if (name !== '') names.add(name);

  return [...names]
    .sort((a, b) => a.localeCompare(b, 'en'))
    .map((value) => {
      const offset = zoneOffset(value);
      const name = value.replaceAll('_', ' ');

      return { value, label: offset === '' ? name : `${name} (${offset})` };
    });
};

/** How far this browser's clock is ahead of UTC now, in seconds. */
export const browserOffsetSec = (): number =>
  -new Date().getTimezoneOffset() * 60;

/**
 * How much `current` is above `previous`, as a part of `previous`: 0.12 is
 * 12 % more. Undefined when there is nothing to compare with.
 */
export const changeOf = (
  previous: number | null | undefined,
  current: number | null | undefined,
): number | undefined =>
  previous == null || current == null || previous <= 0
    ? undefined
    : (current - previous) / previous;

/**
 * Saves one table of the view as a CSV file. The process answers the file in
 * parts, because one answer is bounded. Every part after the first carries
 * the first one's `as_of`, so a record made meanwhile does not move the rows.
 * The byte order mark makes a spreadsheet read the file as UTF-8.
 */
export const downloadCsv = async (
  api: Pick<PowerApi, 'getEnergyExport'>,
  query: EnergyQuery,
  table: EnergyTable,
): Promise<void> => {
  const first = await api.getEnergyExport(query, table, 0);
  const parts = [first.csv];
  let last = 0;
  let next = first.next_offset;
  // An offset that does not rise would never end, so it ends the file.
  while (next > last) {
    const part = await api.getEnergyExport(query, table, next, first.as_of);
    parts.push(part.csv);
    last = next;
    next = part.next_offset;
  }
  saveInBrowser(
    new Blob(['\uFEFF', ...parts], { type: 'text/csv;charset=utf-8' }),
    first.file_name,
  );
};

const NO_FIGURES: EnergyFigures = {
  energy_wh: 0,
  night_wh: 0,
  cost: null,
  observed_sec: 0,
  on_sec: 0,
  metered_sec: 0,
};

/** Several figures as one. The cost is null when no part has a cost. */
export const sumFigures = (list: readonly EnergyFigures[]): EnergyFigures =>
  list.reduce<EnergyFigures>(
    (sum, one) => ({
      energy_wh: sum.energy_wh + one.energy_wh,
      night_wh: sum.night_wh + one.night_wh,
      cost: one.cost === null ? sum.cost : (sum.cost ?? 0) + one.cost,
      observed_sec: sum.observed_sec + one.observed_sec,
      on_sec: sum.on_sec + one.on_sec,
      metered_sec: sum.metered_sec + one.metered_sec,
    }),
    NO_FIGURES,
  );

const DAY_MS = 86_400_000;
/** The least of a period that must have run before its course is told. */
const COURSE_AFTER_MS = { month: 3 * DAY_MS, year: 30 * DAY_MS } as const;

/**
 * What the running month or year is on course for, as a factor on its total
 * so far: the whole period over the part that has run. A period that started
 * before the first record runs from that record. Undefined for a day, a
 * week, a period that is over, and one too young to tell.
 */
export const courseOf = (
  report: EnergyReport,
  now: number,
): number | undefined => {
  if (report.span !== 'month' && report.span !== 'year') return undefined;
  if (report.next_at !== '' || !hasFigure(factOf(report.total))) {
    return undefined;
  }
  const from = Date.parse(report.from);
  const to = Date.parse(report.to);
  if (!Number.isFinite(from) || !Number.isFinite(to)) return undefined;
  // The first day of record at the site's midnight.
  const first =
    report.earliest === ''
      ? from
      : siteDate(report.earliest).getTime() - report.utc_offset_sec * 1000;
  const start = Math.max(from, Number.isFinite(first) ? first : from);
  const elapsed = now - start;
  const whole = to - start;
  if (elapsed < COURSE_AFTER_MS[report.span] || elapsed >= whole) {
    return undefined;
  }

  return whole / elapsed;
};
