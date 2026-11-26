<!--
  The printers of the Energy view: what each used and cost in the period,
  its share of the fleet, what it used while idle, how long its plug was on
  and how many prints ended.
  The list is the whole fleet also while the figures above are narrowed to
  one printer, so the share stays a share of everything.

  The columns follow the room the table has, not the width of the window:
  beside the app's sidebar a wide window still leaves a narrow table. In
  little room only the printer, the energy and the cost keep a column. The
  name takes the room that is left and is cut when it is longer, so the
  figures are always on screen and the table needs no sideways scroll.
-->
<script lang="ts">
  import {
    PrinterIcon,
    Progress,
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
  } from '@prusa/connect-ui/primitives';
  import { printerHref } from '@prusa/connect-ui/shell';

  import type { EnergyPrinter } from './api';
  import { factOf, hasFigure } from './energy';
  import EnergyFigure from './energy-figure.svelte';
  import { formatCost, formatEnergy, formatHours, formatShare } from './format';
  import { heldHost } from './host';
  import { artworkOf } from './plug-line';

  let {
    printers,
    currency,
    priceSet,
  }: {
    printers: readonly EnergyPrinter[];
    currency: string;
    /** With no price the Cost column is left out. */
    priceSet: boolean;
  } = $props();

  const host = heldHost();
  const t = host.t;

  /** The printers Connect lists now, for the picture and the link. */
  const known = $derived(
    new Map(host.fleet.devices.map((printer) => [printer.id, printer])),
  );

  const rows = $derived(
    printers.map((printer) => {
      const fact = factOf(printer);

      return { printer, fact, figure: hasFigure(fact) };
    }),
  );

  /**
   * What a printer used with no print running. 'unknown' where none of its
   * records says whether a print ran, undefined where there is nothing to say.
   */
  const idleOf = (
    printer: EnergyPrinter,
  ): { energy: string; cost: string | undefined } | 'unknown' | undefined => {
    if (printer.print_wh === undefined || printer.idle_wh === undefined) {
      return undefined;
    }
    const unknown = printer.split_unknown_wh ?? 0;
    if (printer.energy_wh > 0 && unknown >= printer.energy_wh) return 'unknown';

    return {
      energy: formatEnergy(printer.idle_wh),
      cost:
        priceSet && typeof printer.idle_cost === 'number'
          ? formatCost(printer.idle_cost, currency)
          : undefined,
    };
  };

  /**
   * The name column. It asks for two fifths of the table and has no least
   * width, so it gives way to the figures and its text is cut to fit.
   */
  const NAME_CELL = 'w-2/5 max-w-0';

  /** What the whole fleet used, which a share is a part of. */
  const fleetWh = $derived(
    rows.reduce(
      (sum, row) => sum + (row.figure ? row.printer.energy_wh : 0),
      0,
    ),
  );
</script>

<div class="@container">
  <Table data-part="energy-printers">
    <TableHeader>
      <TableRow>
        <TableHead class={NAME_CELL}>{t('power.energy.col.printer')}</TableHead>
        <TableHead class="text-end!">{t('power.energy.col.energy')}</TableHead>
        {#if priceSet}
          <TableHead class="text-end!">{t('power.energy.col.cost')}</TableHead>
        {/if}
        <TableHead class="hidden @xl:table-cell">
          {t('power.energy.col.share')}
        </TableHead>
        <TableHead class="hidden text-end! @3xl:table-cell">
          {t('power.energy.col.idle')}
        </TableHead>
        <TableHead class="hidden text-end! @3xl:table-cell">
          {t('power.energy.col.on')}
        </TableHead>
        <TableHead class="hidden text-end! @3xl:table-cell">
          {t('power.energy.col.prints')}
        </TableHead>
      </TableRow>
    </TableHeader>
    <TableBody>
      {#each rows as { printer, fact, figure } (printer.device_id)}
        {@const listed = known.get(printer.device_id)}
        {@const share = figure && fleetWh > 0 ? printer.energy_wh / fleetWh : 0}
        <TableRow data-printer={printer.device_id}>
          <TableCell class={NAME_CELL}>
            <div class="flex min-w-0 items-center gap-3">
              {#if listed !== undefined}
                <PrinterIcon
                  model={artworkOf(listed.model)}
                  reported={listed.model}
                  class="hidden size-9 shrink-0 @xl:block"
                />
                <a
                  class="min-w-0 truncate font-medium hover:underline"
                  href={printerHref(printer.device_id)}
                  title={printer.name}
                >
                  {printer.name}
                </a>
              {:else}
                <span class="min-w-0 truncate font-medium" title={printer.name}>
                  {printer.name}
                </span>
              {/if}
            </div>
          </TableCell>
          <TableCell class="text-end">
            <EnergyFigure {fact} energyWh={printer.energy_wh} />
          </TableCell>
          {#if priceSet}
            <TableCell class="text-end tabular-nums">
              {#if figure && printer.cost !== null}
                {formatCost(printer.cost, currency)}
              {/if}
            </TableCell>
          {/if}
          <TableCell class="hidden @xl:table-cell">
            {#if figure}
              <div class="flex items-center gap-2">
                <div class="w-24">
                  <Progress
                    value={share * 100}
                    aria-label={t('power.energy.col.share')}
                  />
                </div>
                <span class="text-muted-foreground tabular-nums">
                  {formatShare(share)}
                </span>
              </div>
            {/if}
          </TableCell>
          <TableCell
            class="hidden text-end tabular-nums @3xl:table-cell"
            data-part="idle"
          >
            {#if figure}
              {@const idle = idleOf(printer)}
              {#if idle === 'unknown'}
                <span class="text-muted-foreground">
                  {t('power.energy.cell.splitUnknown')}
                </span>
              {:else if idle !== undefined}
                <div class="flex flex-col">
                  <span>{idle.energy}</span>
                  {#if idle.cost !== undefined}
                    <span class="text-muted-foreground text-xs">
                      {idle.cost}
                    </span>
                  {/if}
                </div>
              {/if}
            {/if}
          </TableCell>
          <TableCell class="hidden text-end tabular-nums @3xl:table-cell">
            {#if fact !== 'none'}
              {formatHours(printer.on_sec)}
            {/if}
          </TableCell>
          <TableCell class="hidden text-end tabular-nums @3xl:table-cell">
            {printer.prints}
          </TableCell>
        </TableRow>
      {/each}
    </TableBody>
  </Table>
</div>
