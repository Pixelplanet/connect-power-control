<!--
  The Energy view of the Power Control page: what the printers used and cost
  over a day, a week, a month or a year, as four figures, how much of the
  energy went into printing and how much was used while idle, a chart of the
  period and a table of the printers, of the prints or of the files.

  The process works out every sum, the cost and the days, so this pane only
  asks and draws. It makes its own reads, apart from the page's five-second
  poll, and reads again once a minute while it shows the period that runs now.
  Two things are worked out here: what a running month or year is on course
  for, and which of energy and cost the chart draws.

  A link from another screen can name the printer to narrow to, in the
  address of the page. It is not kept as the person's own choice.

  A read that fails leaves the figures of the last one on screen. The toolbar
  then goes back to those figures, and the note about the failure offers to
  ask again.
-->
<script lang="ts">
  import {
    Alert,
    AlertDescription,
    Button,
    Card,
    CardContent,
    EmptyState,
    Progress,
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
    Skeleton,
    StatusCard,
    type StatusCardProps,
    ToggleGroup,
    ToggleGroupItem,
  } from '@prusa/connect-ui/primitives';
  import { formatNumber } from '@prusa/connect-ui/i18n';

  import type { EnergyPrinter, EnergyReport, EnergySpan } from './api';
  import {
    browserOffsetSec,
    browserZone,
    changeOf,
    courseOf,
    downloadCsv,
    factOf,
    factText,
    hasFigure,
    isSpan,
    SPANS,
    spanLabel,
  } from './energy';
  import EnergyBars from './energy-bars.svelte';
  import EnergyFiles from './energy-files.svelte';
  import EnergyPrinters from './energy-printers.svelte';
  import EnergyPrints from './energy-prints.svelte';
  import FaultNote from './fault-note.svelte';
  import { formatCost, formatEnergy, formatHours, formatShare } from './format';
  import { heldHost } from './host';
  import LinkButton from './link-button.svelte';
  import { askedInAddress, DEVICE_PARAM, settingsTabHref } from './links';
  import { powerFor } from './power.svelte';

  /** The keys the choices are kept under, in this module's own store. */
  const SPAN_KEY = 'power-energy-span';
  const DEVICE_KEY = 'power-energy-device';
  const TABLE_KEY = 'power-energy-table';
  const MEASURE_KEY = 'power-energy-measure';

  /** How often the period that runs now is read again. */
  const REFRESH_MS = 60_000;
  /** The value of "All printers" in the list: an empty value means no choice. */
  const ALL = 'all';

  type TableKind = 'printers' | 'prints' | 'files';
  const isTable = (value: string | undefined): value is TableKind =>
    value === 'printers' || value === 'prints' || value === 'files';
  /** What the bars of the chart stand for. */
  type Measure = 'energy' | 'cost';

  /** A period and a printer: what a read asks for and what a report shows. */
  interface Choice {
    span: EnergySpan;
    at: string | undefined;
    device: string;
  }

  /** What one of the four cards says. */
  interface CardText {
    title: string;
    value?: string | undefined;
    caption?: string | undefined;
    trend?: StatusCardProps['trend'];
    trendLabel?: string | undefined;
  }

  const host = heldHost();
  const t = host.t;
  const power = powerFor(host);

  const storedSpan = host.store.get(SPAN_KEY);
  const firstSpan: EnergySpan = isSpan(storedSpan) ? storedSpan : 'month';
  const firstDevice =
    askedInAddress(DEVICE_PARAM) ?? host.store.get(DEVICE_KEY) ?? '';

  // What the toolbar shows: the choices that are asked for while a read
  // runs, and the choices of the report on screen once the read ended, also
  // when it failed. So the toolbar and the figures do not stay apart.
  let span = $state<EnergySpan>(firstSpan);
  /** A site day inside the period, undefined for the period that runs now. */
  let at = $state<string | undefined>(undefined);
  /** The printer the figures are narrowed to, '' for all. */
  let device = $state(firstDevice);
  const storedTable = host.store.get(TABLE_KEY);
  let table = $state<TableKind>(
    isTable(storedTable) ? storedTable : 'printers',
  );

  /** What a person chose for the chart. Cost needs a price, see `measure`. */
  let measureChoice = $state<Measure>(
    host.store.get(MEASURE_KEY) === 'cost' ? 'cost' : 'energy',
  );

  /** The read that is asked for. A new object asks, also for the same again. */
  let ask = $state.raw<Choice>({
    span: firstSpan,
    at: undefined,
    device: firstDevice,
  });
  let report = $state.raw<EnergyReport | undefined>(undefined);
  let fault = $state.raw<unknown>(undefined);
  /** What the read that failed asked for, for "Try again". */
  let failed = $state.raw<Choice | undefined>(undefined);
  let busy = $state(false);

  /** What a download or the zone button was refused with. */
  let actionFault = $state.raw<unknown>(undefined);
  let saving = $state(false);
  let settingZone = $state(false);

  /** Puts the toolbar, and the choices that are kept, to `next`. */
  const choose = (next: Choice): void => {
    if (next.span !== span) host.store.set(SPAN_KEY, next.span);
    if (next.device !== device) {
      if (next.device === '') host.store.delete(DEVICE_KEY);
      else host.store.set(DEVICE_KEY, next.device);
    }
    span = next.span;
    at = next.at;
    device = next.device;
  };

  /**
   * The choices a report shows. The period that runs now has no day, so it
   * follows the clock, and another span chosen from it runs now too.
   */
  const choiceOf = (shown: EnergyReport): Choice => ({
    span: shown.span,
    at: shown.next_at === '' ? undefined : shown.at,
    device: shown.device,
  });

  /** Asks for a period and a printer, and shows the choice in the toolbar. */
  const show = (next: Choice): void => {
    choose(next);
    // A copy, so that the choice of a read that failed asks again.
    ask = { ...next };
  };

  /** Asks for what the toolbar shows once more. */
  const reload = (): void => {
    ask = { span, at, device };
  };

  /** Asks again for what the last read failed on. */
  const retry = (): void => {
    if (failed === undefined) reload();
    else show(failed);
  };

  $effect(() => {
    const query = ask;
    const abort = new AbortController();
    busy = true;
    power.api.getEnergy(query, abort.signal).then(
      (read) => {
        if (abort.signal.aborted) return;
        report = read;
        fault = undefined;
        failed = undefined;
        busy = false;
        // A printer that is gone, or that the person may no longer see, is
        // read as no filter. The kept choice goes with it.
        choose(choiceOf(read));
      },
      (thrown: unknown) => {
        if (abort.signal.aborted) return;
        fault = thrown;
        failed = query;
        busy = false;
        // The figures on screen stay, so the toolbar goes back to them. The
        // same click then asks again, and so does "Try again".
        if (report !== undefined) choose(choiceOf(report));
      },
    );

    return () => {
      abort.abort();
    };
  });

  // Only the period that runs now gets new figures.
  $effect(() => {
    if (report?.next_at !== '') return;
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') reload();
    }, REFRESH_MS);

    return () => {
      clearInterval(timer);
    };
  });

  const pickSpan = (next: string): void => {
    if (!isSpan(next)) return;
    // The day is the one that is asked for, not the one of the report on
    // screen: a span chosen while an earlier period is still being read
    // lies in that period. The period that runs now has no day, so the new
    // span is the one that runs now too.
    show({ span: next, at, device });
  };

  const pickDevice = (next: string): void => {
    show({ span, at, device: next === ALL ? '' : next });
  };

  /** Goes to the period before or after the one on screen. */
  const step = (day: string | undefined): void => {
    if (report === undefined || day === undefined || day === '') return;
    show({ span: report.span, at: day, device });
  };

  const pickTable = (next: string): void => {
    if (!isTable(next)) return;
    table = next;
    host.store.set(TABLE_KEY, next);
  };

  const pickMeasure = (next: string): void => {
    if (next !== 'energy' && next !== 'cost') return;
    measureChoice = next;
    host.store.set(MEASURE_KEY, next);
  };

  /** The printers of the list: all, each one with a record, and the chosen one. */
  const printerItems = $derived.by(() => {
    const items = [
      { value: ALL, label: t('power.energy.printer.all') },
      ...(report?.printers ?? []).map((printer) => ({
        value: printer.device_id,
        label: printer.name,
      })),
    ];
    // A chosen printer with no record in this period is not in the answer.
    if (device !== '' && !items.some((item) => item.value === device)) {
      const listed = host.fleet.devices.find((one) => one.id === device);
      items.push({
        value: device,
        label:
          listed === undefined || listed.name === '' ? device : listed.name,
      });
    }

    return items;
  });

  const totalFact = $derived(report ? factOf(report.total) : 'none');
  /** The period shown is the one that runs now, so it is not whole yet. */
  const running = $derived(report?.next_at === '');
  /** With no price the chart has only energy to draw. */
  const measure = $derived<Measure>(
    report?.price_set === true ? measureChoice : 'energy',
  );
  /**
   * What the running month or year is on course for, as a factor on its
   * total. It is worked out when the figures are read, which is once a minute.
   */
  const course = $derived(
    report === undefined ? undefined : courseOf(report, Date.now()),
  );

  /** The printers the figures are about: the chosen one, or all. */
  const scope = $derived(
    (report?.printers ?? []).filter(
      (printer) =>
        report?.device === '' || printer.device_id === report?.device,
    ),
  );
  /** The printers whose plug was on with no reading at all. */
  const unmeasuredPrinters = $derived(
    scope.filter((printer) => factOf(printer) === 'notMeasured'),
  );
  const unmeasured = $derived(unmeasuredPrinters.length);
  /**
   * The meter covered only a part of the on time, so the total is too low.
   * The printers with no reading at all are left out of this test: the card
   * names them in a line of their own.
   */
  const partly = $derived.by((): boolean => {
    if (report === undefined || !hasFigure(totalFact)) return false;
    const { total } = report;
    const unmeasuredOn = unmeasuredPrinters.reduce(
      (sum, printer) => sum + printer.on_sec,
      0,
    );

    return (
      factOf({
        observed_sec: total.observed_sec,
        on_sec: total.on_sec - unmeasuredOn,
        metered_sec: total.metered_sec,
      }) === 'partly'
    );
  });

  const costCard = $derived.by((): CardText => {
    const title = t('power.energy.total.cost');
    if (report === undefined) return { title };
    const { total, previous, currency } = report;
    if (!report.price_set) {
      return {
        title,
        caption: t(
          report.can_configure
            ? 'power.energy.total.noPrice'
            : 'power.energy.total.noPriceAsk',
        ),
      };
    }
    // A card with no figure says why, as the Energy card beside it does.
    if (totalFact === 'none') {
      return { title, caption: t('power.energy.total.costNoRecord') };
    }
    if (totalFact === 'off') {
      return { title, caption: t('power.energy.total.costOff') };
    }
    if (totalFact === 'notMeasured') {
      return { title, caption: t('power.energy.total.costNotMeasured') };
    }
    if (total.cost === null) return { title };
    const value = formatCost(total.cost, currency);
    const before = previous?.cost ?? null;
    if (before === null) return { title, value };
    if (running) {
      // A part of a month against a whole month would mislead.
      return {
        title,
        value,
        caption: t('power.energy.total.before', {
          cost: formatCost(before, currency),
        }),
      };
    }
    const change = changeOf(before, total.cost);
    if (change === undefined) return { title, value };
    if (Math.round(change * 100) === 0) {
      return {
        title,
        value,
        trend: 'flat',
        trendLabel: t('power.energy.total.same'),
      };
    }

    return change > 0
      ? {
          title,
          value,
          trend: 'up',
          trendLabel: t('power.energy.total.more', {
            percent: formatShare(change),
          }),
        }
      : {
          title,
          value,
          trend: 'down',
          trendLabel: t('power.energy.total.less', {
            percent: formatShare(-change),
          }),
        };
  });

  const energyCard = $derived.by((): CardText => {
    const title = t('power.energy.total.energy');
    if (report === undefined || totalFact === 'none') return { title };
    const { total } = report;
    if (!hasFigure(totalFact)) {
      return { title, caption: factText(t, totalFact) };
    }

    return {
      title,
      value: formatEnergy(total.energy_wh),
      caption: report.night
        ? t('power.energy.total.dayNight', {
            day: formatEnergy(Math.max(0, total.energy_wh - total.night_wh)),
            night: formatEnergy(total.night_wh),
          })
        : undefined,
    };
  });

  /** With all printers: the one that costs or uses most. With one: its on time. */
  const thirdCard = $derived.by((): CardText => {
    if (report === undefined) return { title: t('power.energy.total.top') };
    if (report.device !== '') {
      const title = t('power.energy.total.on');
      if (totalFact === 'none') return { title };
      const { total } = report;

      return {
        title,
        value: formatHours(total.on_sec),
        caption:
          totalFact === 'partly'
            ? t('power.energy.total.onNote', {
                measured: formatHours(total.metered_sec),
                on: formatHours(total.on_sec),
              })
            : undefined,
      };
    }
    const priced = report.price_set;
    const title = t(
      priced ? 'power.energy.total.top' : 'power.energy.total.topEnergy',
    );
    const amountOf = (printer: EnergyPrinter): number =>
      priced ? (printer.cost ?? 0) : printer.energy_wh;
    const measured = report.printers.filter((printer) =>
      hasFigure(factOf(printer)),
    );
    const sum = measured.reduce((all, printer) => all + amountOf(printer), 0);
    const top = measured.reduce<EnergyPrinter | undefined>(
      (best, printer) =>
        best === undefined || amountOf(printer) > amountOf(best)
          ? printer
          : best,
      undefined,
    );
    if (top === undefined || sum <= 0) return { title };

    return {
      title,
      value: priced
        ? formatCost(amountOf(top), report.currency)
        : formatEnergy(top.energy_wh),
      caption: t('power.energy.total.topNote', {
        printer: top.name,
        percent: formatShare(amountOf(top) / sum),
      }),
    };
  });

  const printsCard = $derived.by((): CardText => {
    const title = t('power.energy.total.prints');
    if (report === undefined || totalFact === 'none') return { title };
    const value = formatNumber(report.prints_total);
    // The average is over the prints of printers with a meter. A print on a
    // plug with no meter has no energy, which is not the same as none used.
    const count = scope.reduce(
      (sum, printer) => sum + (hasFigure(factOf(printer)) ? printer.prints : 0),
      0,
    );
    if (count === 0) return { title, value };
    const amount =
      report.price_set && report.prints_cost !== null
        ? formatCost(report.prints_cost / count, report.currency)
        : formatEnergy(report.prints_wh / count);

    return {
      title,
      value,
      caption: t('power.energy.total.perPrint', { amount }),
    };
  });

  /**
   * How the energy of the period splits into printing and idle. Undefined
   * with no figure, with nothing used, and where the process says no split.
   */
  const split = $derived.by(() => {
    if (report === undefined || !hasFigure(totalFact)) return undefined;
    const { total, currency } = report;
    if (total.energy_wh <= 0 || total.print_wh === undefined) return undefined;
    const part = (wattHours: number) => ({
      energy: formatEnergy(wattHours),
      percent: formatShare(wattHours / total.energy_wh),
    });
    const unknown = total.split_unknown_wh ?? 0;

    return {
      share: Math.min(1, total.print_wh / total.energy_wh),
      printing: part(total.print_wh),
      idle: part(total.idle_wh ?? 0),
      unknown: unknown > 0 ? part(unknown) : undefined,
      idleCost:
        typeof total.idle_cost === 'number'
          ? formatCost(total.idle_cost, currency)
          : undefined,
      estimated: total.split_estimated === true,
    };
  });

  /** Which line about the time zone the pane shows, if any. */
  const zoneNotice = $derived.by(
    (): 'offer' | 'server' | 'named' | undefined => {
      if (report === undefined) return undefined;
      const here = browserZone();
      if (report.zone_set)
        return report.time_zone === here ? undefined : 'named';
      if (report.utc_offset_sec === browserOffsetSec()) return undefined;

      return report.can_configure && here !== '' ? 'offer' : 'server';
    },
  );

  /** Makes this browser's zone the site zone, then reads the figures again. */
  const useBrowserZone = async (): Promise<void> => {
    settingZone = true;
    actionFault = undefined;
    try {
      await power.api.saveEnergySettings({ time_zone: browserZone() });
      reload();
    } catch (thrown) {
      actionFault = thrown;
    } finally {
      settingZone = false;
    }
  };

  /**
   * Saves the table on screen for the period on screen. The Prints and the
   * Files table are narrowed to the chosen printer and so is their file. The
   * Printers table lists every printer, so its file holds every printer too.
   */
  const saveCsv = async (): Promise<void> => {
    if (report === undefined) return;
    saving = true;
    actionFault = undefined;
    try {
      await downloadCsv(
        power.api,
        {
          span: report.span,
          at: report.at,
          device: table === 'printers' ? '' : report.device,
        },
        table === 'printers' ? 'periods' : table,
      );
    } catch (thrown) {
      actionFault = thrown;
    } finally {
      saving = false;
    }
  };
</script>

{#snippet chevron(points: string)}
  <svg
    aria-hidden="true"
    viewBox="0 0 24 24"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    class="size-4 fill-none stroke-current rtl:-scale-x-100"
  >
    <polyline {points} />
  </svg>
{/snippet}

{#snippet setPrice()}
  <span class="text-sm">
    <LinkButton href={settingsTabHref('automation')}>
      {t('power.energy.total.setPrice')}
    </LinkButton>
  </span>
{/snippet}

{#snippet totalCard(card: CardText)}
  <StatusCard
    edge
    tone={card.value === undefined ? 'quiet' : 'idle'}
    title={card.title}
    value={card.value}
    caption={card.caption}
    trend={card.trend}
    trendLabel={card.trendLabel}
  />
{/snippet}

<div class="flex flex-col gap-6" data-part="energy" aria-busy={busy}>
  <div class="flex flex-wrap items-center gap-2" data-part="energy-toolbar">
    <ToggleGroup
      type="single"
      variant="outline"
      size="sm"
      value={span}
      onValueChange={pickSpan}
      aria-label={t('power.energy.span.label')}
    >
      {#each SPANS as one (one)}
        <ToggleGroupItem value={one} data-span={one}>
          {t(`power.energy.span.${one}`)}
        </ToggleGroupItem>
      {/each}
    </ToggleGroup>
    <div class="flex items-center gap-1" data-part="energy-steps">
      <Button
        variant="ghost"
        size="icon-sm"
        label={t('power.energy.span.previous')}
        disabled={busy || report === undefined || report.prev_at === ''}
        onclick={() => {
          step(report?.prev_at);
        }}
      >
        {@render chevron('15 18 9 12 15 6')}
      </Button>
      <span class="text-sm font-medium tabular-nums" data-part="energy-span">
        {report === undefined ? '' : spanLabel(t, report)}
      </span>
      <Button
        variant="ghost"
        size="icon-sm"
        label={t('power.energy.span.next')}
        disabled={busy || report === undefined || report.next_at === ''}
        onclick={() => {
          step(report?.next_at);
        }}
      >
        {@render chevron('9 18 15 12 9 6')}
      </Button>
    </div>
    <div class="ms-auto w-full sm:w-56">
      <Select
        value={device === '' ? ALL : device}
        items={printerItems}
        onValueChange={pickDevice}
      >
        <SelectTrigger
          size="sm"
          class="w-full"
          aria-label={t('power.energy.printer.label')}
          data-part="energy-printer"
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent aria-label={t('power.energy.printer.label')}>
          {#each printerItems as item (item.value)}
            <SelectItem value={item.value}>{item.label}</SelectItem>
          {/each}
        </SelectContent>
      </Select>
    </div>
  </div>

  <FaultNote fault={actionFault} />

  {#if report === undefined}
    {#if fault === undefined}
      <Skeleton
        class="h-24"
        role="status"
        aria-label={t('power.energy.reading')}
      />
      <Skeleton class="h-40" />
      <Skeleton class="h-48" />
    {:else}
      <EmptyState heading={t('power.energy.failed')} headingLevel="h2">
        {#snippet action()}
          <div class="flex flex-col items-center gap-3">
            <FaultNote {fault} />
            <Button onclick={reload}>{t('power.page.retry')}</Button>
          </div>
        {/snippet}
      </EmptyState>
    {/if}
  {:else if !report.recording}
    <EmptyState
      heading={t('power.energy.off')}
      description={t('power.energy.offNote')}
      headingLevel="h2"
      data-part="energy-off"
    />
  {:else if report.earliest === ''}
    <EmptyState
      heading={t('power.energy.none')}
      description={t('power.energy.noneNote')}
      headingLevel="h2"
      data-part="energy-none"
    />
  {:else}
    {#if fault !== undefined}
      <div data-part="energy-fault">
        <FaultNote {fault}>
          {#snippet actions()}
            <Button variant="secondary" size="sm" onclick={retry}>
              {t('power.page.retry')}
            </Button>
          {/snippet}
        </FaultNote>
      </div>
    {/if}

    {#if zoneNotice === 'offer'}
      <Alert data-part="energy-zone-offer">
        <AlertDescription>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <span>{t('power.energy.zone.differs')}</span>
            <Button
              variant="secondary"
              size="sm"
              pending={settingZone}
              onclick={() => void useBrowserZone()}
            >
              {t('power.energy.zone.use', { zone: browserZone() })}
            </Button>
          </div>
        </AlertDescription>
      </Alert>
    {/if}
    {#if zoneNotice === 'server' || zoneNotice === 'named' || report.price_changed || report.removed > 0 || report.prints_dropped > 0}
      <div
        class="text-muted-foreground flex flex-col gap-1 text-sm"
        data-part="energy-notices"
      >
        {#if zoneNotice === 'server'}
          <p data-part="energy-zone">{t('power.energy.zone.server')}</p>
        {:else if zoneNotice === 'named'}
          <p data-part="energy-zone">
            {t('power.energy.zone.note', { zone: report.time_zone })}
          </p>
        {/if}
        {#if report.price_changed}
          <p data-part="energy-price-changed">
            {t('power.energy.priceChanged')}
          </p>
        {/if}
        {#if report.removed > 0}
          <p data-part="energy-removed">
            {t('power.energy.removed', { count: report.removed })}
          </p>
        {/if}
        {#if report.prints_dropped > 0}
          <p data-part="energy-dropped">
            {t('power.energy.dropped', { count: report.prints_dropped })}
          </p>
        {/if}
      </div>
    {/if}

    <div
      class="grid grid-cols-2 gap-3 md:grid-cols-4"
      data-part="energy-totals"
    >
      <div class="contents" data-part="energy-cost">
        <StatusCard
          edge
          tone={costCard.value === undefined ? 'quiet' : 'idle'}
          title={costCard.title}
          value={costCard.value}
          caption={costCard.caption}
          trend={costCard.trend}
          trendLabel={costCard.trendLabel}
          {...!report.price_set && report.can_configure
            ? { action: setPrice }
            : {}}
        >
          {#if course !== undefined && report.total.cost !== null && costCard.value !== undefined}
            <p class="text-caption text-muted-foreground" data-part="course">
              {t('power.energy.total.course', {
                amount: formatCost(report.total.cost * course, report.currency),
              })}
            </p>
          {/if}
        </StatusCard>
      </div>
      <div class="contents" data-part="energy-used">
        <StatusCard
          edge
          tone={energyCard.value === undefined ? 'quiet' : 'idle'}
          title={energyCard.title}
          value={energyCard.value}
          caption={energyCard.caption}
        >
          {#if course !== undefined && energyCard.value !== undefined}
            <p
              class="text-caption text-muted-foreground"
              data-part="course-energy"
            >
              {t('power.energy.total.course', {
                amount: formatEnergy(report.total.energy_wh * course),
              })}
            </p>
          {/if}
          {#if partly}
            <p class="text-caption text-muted-foreground" data-part="partly">
              {t('power.energy.cell.partly')}
            </p>
          {/if}
          {#if unmeasured > 0 && energyCard.value !== undefined}
            <p
              class="text-caption text-muted-foreground"
              data-part="unmeasured"
            >
              {t('power.energy.total.unmeasured', { count: unmeasured })}
            </p>
          {/if}
        </StatusCard>
      </div>
      <div class="contents" data-part="energy-top">
        {@render totalCard(thirdCard)}
      </div>
      <div class="contents" data-part="energy-prints-count">
        {@render totalCard(printsCard)}
      </div>
    </div>

    {#if split !== undefined}
      <Card density="compact" data-part="energy-split">
        <CardContent class="flex flex-col gap-2">
          <div
            class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1"
          >
            <p class="text-sm font-medium">{t('power.energy.split.label')}</p>
            {#if split.idleCost !== undefined}
              <p class="text-sm tabular-nums" data-part="split-idle-cost">
                {t('power.energy.split.idleCost', { cost: split.idleCost })}
              </p>
            {/if}
          </div>
          <Progress
            value={split.share * 100}
            aria-label={t('power.energy.split.printing', split.printing)}
          />
          <div
            class="flex flex-wrap justify-between gap-x-4 gap-y-1 text-sm tabular-nums"
          >
            <span data-part="split-printing">
              {t('power.energy.split.printing', split.printing)}
            </span>
            {#if split.unknown !== undefined}
              <span class="text-muted-foreground" data-part="split-unknown">
                {t('power.energy.split.unknown', split.unknown)}
              </span>
            {/if}
            <span data-part="split-idle">
              {t('power.energy.split.idle', split.idle)}
            </span>
          </div>
          <p class="text-muted-foreground text-xs">
            {t('power.energy.split.note')}
          </p>
          {#if split.estimated}
            <p
              class="text-muted-foreground text-xs"
              data-part="split-estimated"
            >
              {t('power.energy.split.estimated')}
            </p>
          {/if}
        </CardContent>
      </Card>
    {/if}

    <Card density="compact">
      <CardContent class="flex flex-col gap-3">
        {#if report.price_set && totalFact !== 'none'}
          <div class="flex justify-end">
            <ToggleGroup
              type="single"
              variant="outline"
              size="sm"
              value={measure}
              onValueChange={pickMeasure}
              aria-label={t('power.energy.measure.label')}
              data-part="energy-measure"
            >
              <ToggleGroupItem value="energy" data-measure="energy">
                {t('power.energy.measure.energy')}
              </ToggleGroupItem>
              <ToggleGroupItem value="cost" data-measure="cost">
                {t('power.energy.measure.cost')}
              </ToggleGroupItem>
            </ToggleGroup>
          </div>
        {/if}
        {#if totalFact === 'none'}
          <p
            class="text-muted-foreground flex h-40 items-center justify-center text-sm"
            data-part="energy-empty"
          >
            {t('power.energy.emptySpan')}
          </p>
        {:else}
          {#key `${report.span} ${report.at} ${report.device} ${measure}`}
            <EnergyBars
              points={report.points}
              currency={report.currency}
              priceSet={report.price_set}
              span={report.span}
              {measure}
            />
          {/key}
        {/if}
      </CardContent>
    </Card>

    <Card density="compact" data-part="energy-table">
      <CardContent class="flex flex-col gap-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <ToggleGroup
            type="single"
            variant="outline"
            size="sm"
            value={table}
            onValueChange={pickTable}
            aria-label={t('power.energy.table.label')}
          >
            <ToggleGroupItem value="printers" data-table="printers">
              {t('power.energy.table.printers')}
            </ToggleGroupItem>
            <ToggleGroupItem value="prints" data-table="prints">
              {t('power.energy.table.prints')}
            </ToggleGroupItem>
            <ToggleGroupItem value="files" data-table="files">
              {t('power.energy.table.files')}
            </ToggleGroupItem>
          </ToggleGroup>
          <Button
            variant="secondary"
            size="sm"
            pending={saving}
            onclick={() => void saveCsv()}
            data-part="energy-csv"
          >
            {t('power.energy.csv')}
          </Button>
        </div>
        {#if table === 'prints'}
          <EnergyPrints
            prints={report.prints}
            total={report.prints_total}
            currency={report.currency}
            priceSet={report.price_set}
          />
        {:else if table === 'files'}
          <EnergyFiles
            files={report.files ?? []}
            total={report.files_total ?? 0}
            currency={report.currency}
            priceSet={report.price_set}
          />
        {:else if report.printers.length === 0}
          <p
            class="text-muted-foreground py-6 text-center text-sm"
            data-part="no-printers"
          >
            {t('power.energy.emptySpan')}
          </p>
        {:else}
          <EnergyPrinters
            printers={report.printers}
            currency={report.currency}
            priceSet={report.price_set}
          />
        {/if}
      </CardContent>
    </Card>
  {/if}
</div>
