<!--
  The Power Control page, in two views. "Now" is every printer that has a
  plug, what the fleet draws now, and the buttons that switch each plug. A
  person looks at it as a table or as cards, like the printers page, and the
  choice is kept. "Energy" is what the printers used and cost over time. It
  is drawn only while it is chosen, and it also opens when no plug is set up
  or the plugs cannot be read.

  "Now" also says what today and this month used and cost. Those figures come
  from the store's summary, which is read once a minute and not with the
  five-second poll. A link from another screen can name the view to open in
  the address of the page; that is not kept as the person's own choice.
-->
<script lang="ts">
  import {
    Alert,
    AlertDescription,
    AlertTitle,
    Badge,
    Button,
    Card,
    CardContent,
    EmptyState,
    PageHeader,
    PrinterIcon,
    Skeleton,
    StatusCard,
    Switch,
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
    Tabs,
    TabsContent,
    TabsList,
    TabsTrigger,
    viewModes,
    ViewModeToggle,
  } from '@prusa/connect-ui/primitives';
  import { printerHref } from '@prusa/connect-ui/shell';
  import { onMount } from 'svelte';

  import AllOff from './all-off.svelte';
  import type { EnergyFigures } from './api';
  import { factOf, factText, hasFigure } from './energy';
  import EnergyPane from './energy-pane.svelte';
  import FaultNote from './fault-note.svelte';
  import {
    formatCost,
    formatCountdown,
    formatEnergy,
    formatSpan,
    formatWatts,
  } from './format';
  import { heldHost } from './host';
  import LinkButton from './link-button.svelte';
  import { askedInAddress, PANE_PARAM, settingsTabHref } from './links';
  import PlugBadge from './plug-badge.svelte';
  import PlugCard from './plug-card.svelte';
  import { artworkOf, plugLineOf } from './plug-line';
  import { plugStateOf, wattsNow } from './plug-state';
  import PowerButtons from './power-buttons.svelte';
  import { powerFor } from './power.svelte';
  import { tripped } from './signal';
  import { type AllOffResult, type PlugListRow, summaryOf } from './summary';

  /** The key the chosen view is kept under, in this module's own store. */
  const VIEW_KEY = 'power-page-view';
  /** The key the chosen pane is kept under: what is on now, or the energy. */
  const PANE_KEY = 'power-page-pane';

  const host = heldHost();
  const t = host.t;
  const power = powerFor(host);

  let fault = $state.raw<unknown>(undefined);
  /** The printer whose default is being changed. */
  let busyPrinter = $state('');
  /** What "Turn off idle printers" did, until it is closed. */
  let allOff = $state.raw<AllOffResult | undefined>(undefined);
  let view = $state(host.store.get(VIEW_KEY) === 'cards' ? 'cards' : 'table');
  const askedPane = askedInAddress(PANE_PARAM);
  const keptPane = host.store.get(PANE_KEY) === 'energy' ? 'energy' : 'now';
  let pane = $state(
    askedPane === 'energy' || askedPane === 'now' ? askedPane : keptPane,
  );

  onMount(() => power.watch());

  // Today and this month are shown on "Now" only, so they are read for it only.
  $effect(() => {
    if (pane !== 'now') return undefined;

    return power.watchSummary();
  });

  const overview = $derived(power.overview);
  const views = $derived(viewModes(['table', 'cards']));

  /** Every printer the person may see that has a plug, by name. */
  const rows = $derived(
    host.fleet.devices
      .map((printer): PlugListRow => {
        const entry = power.entry(printer.id);

        return {
          id: printer.id,
          name: printer.name || printer.id,
          model: printer.model,
          entry,
          state: plugStateOf(entry?.config, entry?.plug),
          watts: wattsNow(entry?.config, entry?.plug),
        };
      })
      .filter((row) => row.state !== 'none')
      .sort((a, b) => a.name.localeCompare(b.name)),
  );
  const without = $derived(host.fleet.devices.length - rows.length);
  const totals = $derived(summaryOf(rows));

  const summary = $derived(power.summary);
  /** What the fleet costs an hour at what it draws now, with a price. */
  const perHour = $derived(
    summary === undefined || summary.price_now === null || totals.metered === 0
      ? undefined
      : formatCost((totals.watts / 1000) * summary.price_now, summary.currency),
  );

  /** What a card of today or of this month says. */
  const periodCard = (
    figures: EnergyFigures,
    currency: string,
  ): { value: string | undefined; caption: string | undefined } => {
    const fact = factOf(figures);
    if (!hasFigure(fact)) {
      return { value: undefined, caption: factText(t, fact) };
    }

    const cost =
      figures.cost === null ? undefined : formatCost(figures.cost, currency);
    if (fact === 'partly') {
      // The sum is too low, which the Energy tab says of the same figure.
      return {
        value: formatEnergy(figures.energy_wh),
        caption:
          cost === undefined
            ? factText(t, fact)
            : t('power.page.total.partly', { cost }),
      };
    }

    return { value: formatEnergy(figures.energy_wh), caption: cost };
  };

  const pickView = (next: string): void => {
    if (next !== 'table' && next !== 'cards') return;
    view = next;
    host.store.set(VIEW_KEY, next);
  };

  const pickPane = (next: string): void => {
    if (next !== 'now' && next !== 'energy') return;
    pane = next;
    host.store.set(PANE_KEY, next);
  };

  /** Sets whether a printer turns off after every print. */
  const setDefault = async (id: string, enabled: boolean): Promise<void> => {
    busyPrinter = id;
    fault = undefined;
    try {
      await power.setAutoOff(id, enabled, true);
    } catch (thrown) {
      fault = thrown;
    } finally {
      busyPrinter = '';
    }
  };
</script>

<div class="flex flex-col gap-6" data-part="power-page">
  <PageHeader title={t('power.page.title')}>
    {#snippet actions()}
      <div class="flex flex-wrap items-center justify-end gap-2">
        {#if pane === 'now'}
          <AllOff
            {rows}
            onresult={(result: AllOffResult | undefined) => (allOff = result)}
          />
          <ViewModeToggle
            value={view}
            onValueChange={pickView}
            options={views}
          />
        {/if}
        {#if power.mayConfigure}
          <LinkButton
            href={settingsTabHref('plugs')}
            variant="secondary"
            size="md"
          >
            {t('power.page.manage')}
          </LinkButton>
        {/if}
      </div>
    {/snippet}
  </PageHeader>

  <Tabs value={pane} onValueChange={pickPane} data-part="pane-tabs">
    <TabsList aria-label={t('power.page.tab.label')}>
      <TabsTrigger value="now">{t('power.page.tab.now')}</TabsTrigger>
      <TabsTrigger value="energy">{t('power.page.tab.energy')}</TabsTrigger>
    </TabsList>
    <TabsContent value="now">
      {#if pane === 'now'}
        <div class="flex flex-col gap-6 pt-4" data-part="now">
          {#if overview === undefined || !host.fleet.loaded}
            {#if power.overviewFault === undefined && host.fleet.error === undefined}
              <Skeleton
                class="h-24"
                role="status"
                aria-label={t('power.page.reading')}
              />
              <Skeleton class="h-48" />
            {:else}
              <EmptyState heading={t('power.page.failed')} headingLevel="h2">
                {#snippet action()}
                  <FaultNote fault={power.overviewFault ?? host.fleet.error} />
                  <Button onclick={() => void power.refresh()}>
                    {t('power.page.retry')}
                  </Button>
                {/snippet}
              </EmptyState>
            {/if}
          {:else if rows.length === 0}
            <EmptyState
              heading={t('power.page.empty')}
              description={t('power.page.emptyNote')}
              headingLevel="h2"
            >
              {#snippet action()}
                {#if power.mayConfigure}
                  <LinkButton
                    href={settingsTabHref('plugs')}
                    variant="default"
                    size="md"
                  >
                    {t('power.page.setUp')}
                  </LinkButton>
                {/if}
              {/snippet}
            </EmptyState>
          {:else}
            <div
              class={[
                'grid grid-cols-2 gap-3',
                summary?.recording === false
                  ? 'md:grid-cols-4'
                  : 'md:grid-cols-3',
              ]}
              data-part="totals"
            >
              <StatusCard
                edge
                tone={totals.watts > 0 ? 'active' : 'quiet'}
                title={t('power.page.total.now')}
                value={totals.metered > 0
                  ? formatWatts(totals.watts)
                  : undefined}
                caption={t('power.page.total.metered', {
                  count: totals.metered,
                })}
              >
                {#if perHour !== undefined}
                  <p
                    class="text-caption text-muted-foreground"
                    data-part="per-hour"
                  >
                    {t('power.page.total.perHour', { cost: perHour })}
                  </p>
                {/if}
              </StatusCard>
              <StatusCard
                edge
                tone={totals.on > 0 ? 'active' : 'quiet'}
                title={t('power.page.total.on')}
                value={t('power.page.total.onOf', {
                  on: totals.on,
                  all: rows.length,
                })}
              />
              <StatusCard
                edge
                tone="idle"
                title={t('power.page.total.energy')}
                value={formatEnergy(totals.energyWh)}
                caption={t('power.page.total.energyNote')}
              />
              {#if summary?.recording}
                {@const today = periodCard(summary.today, summary.currency)}
                {@const month = periodCard(
                  summary.this_month,
                  summary.currency,
                )}
                <div class="contents" data-part="today">
                  <StatusCard
                    edge
                    tone={today.value === undefined ? 'quiet' : 'idle'}
                    title={t('power.page.total.today')}
                    value={today.value}
                    caption={today.caption}
                  />
                </div>
                <div class="contents" data-part="month">
                  <StatusCard
                    edge
                    tone={month.value === undefined ? 'quiet' : 'idle'}
                    title={t('power.page.total.month')}
                    value={month.value}
                    caption={month.caption}
                  />
                </div>
              {/if}
              <StatusCard
                edge
                tone={totals.silent > 0 ? 'fault' : 'quiet'}
                title={t('power.page.total.silent')}
                value={String(totals.silent)}
              />
            </div>

            <FaultNote fault={fault ?? power.overviewFault} />

            {#if allOff !== undefined}
              <Alert
                variant={allOff.stayed.length > 0 ? 'warning' : 'success'}
                data-part="all-off-result"
              >
                <AlertTitle>
                  {t('power.page.allOff.done', { count: allOff.done })}
                </AlertTitle>
                <AlertDescription>
                  <div class="flex flex-col items-start gap-2">
                    {#if allOff.stayed.length > 0}
                      <p>{t('power.page.allOff.skipped')}</p>
                      <ul class="list-disc ps-5">
                        {#each allOff.stayed as stayed (stayed.id)}
                          <li data-printer={stayed.id}>
                            {t('power.page.allOff.item', {
                              printer: stayed.name,
                              reason: stayed.reason,
                            })}
                          </li>
                        {/each}
                      </ul>
                    {/if}
                    <Button
                      variant="secondary"
                      size="sm"
                      onclick={() => (allOff = undefined)}
                    >
                      {t('power.page.allOff.close')}
                    </Button>
                  </div>
                </AlertDescription>
              </Alert>
            {/if}

            {#if view === 'cards'}
              <ul
                class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3"
                data-part="plug-cards"
              >
                {#each rows as row (row.id)}
                  <li>
                    <PlugCard
                      {row}
                      confirm={overview.confirm_turn_off}
                      busy={busyPrinter === row.id}
                      ondefault={(enabled: boolean) =>
                        void setDefault(row.id, enabled)}
                      onfault={(thrown: unknown) => (fault = thrown)}
                    />
                  </li>
                {/each}
              </ul>
            {:else}
              <Card density="compact">
                <CardContent>
                  <Table data-part="plugs">
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t('power.page.col.printer')}</TableHead>
                        <TableHead>{t('power.page.col.state')}</TableHead>
                        <TableHead class="text-end">
                          {t('power.page.col.now')}
                        </TableHead>
                        <TableHead class="text-end">
                          {t('power.page.col.session')}
                        </TableHead>
                        <TableHead>{t('power.page.col.plug')}</TableHead>
                        <TableHead>{t('power.page.col.autoOff')}</TableHead>
                        <TableHead class="text-end">
                          <span class="sr-only"
                            >{t('power.page.col.switch')}</span
                          >
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {#each rows as row (row.id)}
                        {@const session = row.entry?.session_usage}
                        <TableRow data-printer={row.id}>
                          <TableCell>
                            <div class="flex items-center gap-3">
                              <PrinterIcon
                                model={artworkOf(row.model)}
                                reported={row.model}
                                class="size-11 shrink-0"
                              />
                              <a
                                class="font-medium hover:underline"
                                href={printerHref(row.id)}
                              >
                                {row.name}
                              </a>
                            </div>
                          </TableCell>
                          <TableCell>
                            <span
                              class="inline-flex flex-wrap items-center gap-2"
                            >
                              <PlugBadge plugState={row.state} />
                              {#if tripped(row.entry?.plug)}
                                <Badge
                                  variant="destructive"
                                  data-part="protection"
                                >
                                  {t('power.protection.badge')}
                                </Badge>
                              {/if}
                              {#if row.entry?.config.auto_off_after_print}
                                <Badge variant="outline">
                                  {t('power.page.autoOff')}
                                </Badge>
                              {/if}
                              {#if row.state === 'on' && row.entry?.idle_off_remaining_sec !== undefined}
                                <Badge variant="outline" data-part="idle-off">
                                  {t('power.page.idleOff', {
                                    span: formatCountdown(
                                      row.entry.idle_off_remaining_sec,
                                    ),
                                  })}
                                </Badge>
                              {/if}
                            </span>
                          </TableCell>
                          <TableCell class="text-end tabular-nums">
                            {row.watts === undefined
                              ? t('power.page.noReading')
                              : formatWatts(row.watts)}
                          </TableCell>
                          <TableCell class="text-end tabular-nums">
                            {#if session?.active}
                              {t('power.page.session', {
                                energy: formatEnergy(session.energy_wh),
                                span: formatSpan(session.duration_sec),
                              })}
                            {:else}
                              {t('power.page.noReading')}
                            {/if}
                          </TableCell>
                          <TableCell class="text-muted-foreground">
                            {plugLineOf(t, row.entry?.config)}
                          </TableCell>
                          <TableCell>
                            <Switch
                              aria-label={t('power.page.autoOffOf', {
                                printer: row.name,
                              })}
                              checked={row.entry?.config.auto_off_default ===
                                true}
                              disabled={busyPrinter === row.id ||
                                !power.mayOperate(row.id)}
                              onCheckedChange={(enabled: boolean) =>
                                void setDefault(row.id, enabled)}
                            />
                          </TableCell>
                          <TableCell>
                            <div class="flex justify-end">
                              <PowerButtons
                                deviceId={row.id}
                                printer={row.name}
                                plugState={row.state}
                                confirm={overview.confirm_turn_off}
                                onfault={(thrown: unknown) => (fault = thrown)}
                              />
                            </div>
                          </TableCell>
                        </TableRow>
                      {/each}
                    </TableBody>
                  </Table>
                </CardContent>
              </Card>
            {/if}

            {#if without > 0}
              <p class="text-muted-foreground text-sm" data-part="without">
                {t('power.page.without', { count: without })}
                {#if power.mayConfigure}
                  <LinkButton href={settingsTabHref('plugs')}>
                    {t('power.page.setUp')}
                  </LinkButton>
                {/if}
              </p>
            {/if}
          {/if}
        </div>
      {/if}
    </TabsContent>
    <TabsContent value="energy">
      {#if pane === 'energy'}
        <div class="pt-4">
          <EnergyPane />
        </div>
      {/if}
    </TabsContent>
  </Tabs>
</div>
