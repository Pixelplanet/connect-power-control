<!--
  The prints that ended in the period, newest first: when each ended, on
  which printer, how it ended, how long it ran and what it used and cost.
  The process sends the newest two hundred; the CSV file has all of them.

  The columns follow the room the table has, not the width of the window:
  beside the app's sidebar a wide window still leaves a narrow table. In
  little room only the file, the energy and the cost keep a column, and the
  printer and the end time are lines under the file name. With more room the
  end time and the printer get their columns, and with still more the outcome
  and the duration. The file name takes the room that is left and is cut
  when it is longer, so the table needs no sideways scroll.
-->
<script lang="ts">
  import {
    Badge,
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
  } from '@prusa/connect-ui/primitives';

  import type { EnergyPrint } from './api';
  import { hasFigure, printFactOf, siteTime } from './energy';
  import EnergyFigure from './energy-figure.svelte';
  import { formatCost, formatSpan } from './format';
  import { heldHost } from './host';

  let {
    prints,
    total,
    currency,
    priceSet,
  }: {
    prints: readonly EnergyPrint[];
    /** How many prints ended in the period, also those not listed here. */
    total: number;
    currency: string;
    /** With no price the Cost column is left out. */
    priceSet: boolean;
  } = $props();

  const t = heldHost().t;

  const OUTCOMES: Readonly<Record<string, string>> = {
    finished: 'power.energy.outcome.finished',
    cancelled: 'power.energy.outcome.cancelled',
    failed: 'power.energy.outcome.failed',
    interrupted: 'power.energy.outcome.interrupted',
  };

  /**
   * The file column. It asks for all the room and has no least width, so it
   * gets what the other columns leave and its text is cut to fit.
   */
  const FILE_CELL = 'w-full max-w-0';

  /** How a print ended, in words. An outcome with no word of its own "ended". */
  const outcomeText = (outcome: string | undefined): string =>
    t(OUTCOMES[(outcome ?? '').toLowerCase()] ?? 'power.energy.outcome.other');
</script>

{#if prints.length === 0}
  <p
    class="text-muted-foreground py-6 text-center text-sm"
    data-part="no-prints"
  >
    {t('power.energy.noPrints')}
  </p>
{:else}
  <div class="@container">
    <Table data-part="energy-prints">
      <TableHeader>
        <TableRow>
          <TableHead class="hidden @2xl:table-cell">
            {t('power.energy.col.ended')}
          </TableHead>
          <TableHead class="hidden @2xl:table-cell">
            {t('power.energy.col.printer')}
          </TableHead>
          <TableHead class={FILE_CELL}>{t('power.energy.col.file')}</TableHead>
          <TableHead class="hidden @4xl:table-cell">
            {t('power.energy.col.outcome')}
          </TableHead>
          <TableHead class="hidden text-end! @4xl:table-cell">
            {t('power.energy.col.duration')}
          </TableHead>
          <TableHead class="text-end!">
            {t('power.energy.col.energy')}
          </TableHead>
          {#if priceSet}
            <TableHead class="text-end!">
              {t('power.energy.col.cost')}
            </TableHead>
          {/if}
        </TableRow>
      </TableHeader>
      <TableBody>
        {#each prints as print (`${print.device_id} ${print.started_at}`)}
          {@const fact = printFactOf(print)}
          <TableRow data-printer={print.device_id}>
            <TableCell class="hidden tabular-nums @2xl:table-cell">
              <div class="flex flex-col">
                <span>{siteTime(print.ended_local)}</span>
                {#if print.end_estimated}
                  <span
                    class="text-muted-foreground text-xs"
                    data-part="estimated"
                  >
                    {t('power.energy.print.estimated')}
                  </span>
                {/if}
              </div>
            </TableCell>
            <TableCell class="hidden @2xl:table-cell">
              <span class="block max-w-40 truncate" title={print.name}>
                {print.name}
              </span>
            </TableCell>
            <TableCell class={FILE_CELL}>
              <div class="flex min-w-0 flex-col">
                <span class="truncate" title={print.file_name}>
                  {print.file_name}
                </span>
                <span
                  class="text-muted-foreground truncate text-xs @2xl:hidden"
                >
                  {print.name}
                </span>
                <span
                  class="text-muted-foreground truncate text-xs tabular-nums @2xl:hidden"
                >
                  {siteTime(print.ended_local)}
                </span>
                {#if print.end_estimated}
                  <span
                    class="text-muted-foreground truncate text-xs @2xl:hidden"
                  >
                    {t('power.energy.print.estimated')}
                  </span>
                {/if}
              </div>
            </TableCell>
            <TableCell class="hidden @4xl:table-cell">
              <Badge variant="outline">{outcomeText(print.outcome)}</Badge>
            </TableCell>
            <TableCell class="hidden text-end tabular-nums @4xl:table-cell">
              {formatSpan(print.duration_sec)}
            </TableCell>
            <TableCell class="text-end">
              <EnergyFigure {fact} energyWh={print.energy_wh} />
            </TableCell>
            {#if priceSet}
              <TableCell class="text-end tabular-nums">
                {#if hasFigure(fact) && print.cost !== null}
                  {formatCost(print.cost, currency)}
                {/if}
              </TableCell>
            {/if}
          </TableRow>
        {/each}
      </TableBody>
    </Table>
  </div>
  {#if total > prints.length}
    <p class="text-muted-foreground pt-3 text-xs" data-part="more-prints">
      {t('power.energy.more', { shown: prints.length, count: total })}
    </p>
  {/if}
{/if}
