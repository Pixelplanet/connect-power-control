<!--
  The files printed in the period, the one that used most first: how often
  each was printed, how many of those prints did not finish, and what the
  prints used and cost together and on average. The process groups the
  prints by file name and sends the two hundred files that used most; the
  CSV file has all of them.

  The columns follow the room the table has, like the Prints table. In
  little room only the file, the prints, the energy and the cost keep a
  column.
-->
<script lang="ts">
  import {
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
  } from '@prusa/connect-ui/primitives';
  import { formatNumber } from '@prusa/connect-ui/i18n';

  import type { EnergyFile } from './api';
  import EnergyFigure from './energy-figure.svelte';
  import { formatCost, formatEnergy } from './format';
  import { heldHost } from './host';

  let {
    files,
    total,
    currency,
    priceSet,
  }: {
    files: readonly EnergyFile[];
    /** How many files were printed in the period, also those not listed. */
    total: number;
    currency: string;
    /** With no price the Cost column is left out. */
    priceSet: boolean;
  } = $props();

  const t = heldHost().t;

  /** The file column takes the room the other columns leave, see Prints. */
  const FILE_CELL = 'w-full max-w-0';

  const nameOf = (file: EnergyFile): string =>
    file.file_name === '' ? t('power.energy.file.unknown') : file.file_name;
</script>

{#if files.length === 0}
  <p
    class="text-muted-foreground py-6 text-center text-sm"
    data-part="no-files"
  >
    {t('power.energy.noPrints')}
  </p>
{:else}
  <div class="@container">
    <Table data-part="energy-files">
      <TableHeader>
        <TableRow>
          <TableHead class={FILE_CELL}>{t('power.energy.col.file')}</TableHead>
          <TableHead class="text-end!">{t('power.energy.col.prints')}</TableHead
          >
          <TableHead class="hidden text-end! @2xl:table-cell">
            {t('power.energy.col.notFinished')}
          </TableHead>
          <TableHead class="text-end!">{t('power.energy.col.energy')}</TableHead
          >
          {#if priceSet}
            <TableHead class="text-end!">{t('power.energy.col.cost')}</TableHead
            >
          {/if}
          <TableHead class="hidden text-end! @3xl:table-cell">
            {t('power.energy.col.average')}
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {#each files as file (file.file_name)}
          {@const name = nameOf(file)}
          {@const measured = file.measured > 0}
          <TableRow>
            <TableCell class={FILE_CELL}>
              <span
                class={[
                  'block truncate',
                  file.file_name === '' && 'text-muted-foreground',
                ]}
                title={name}
              >
                {name}
              </span>
            </TableCell>
            <TableCell class="text-end tabular-nums">
              {formatNumber(file.prints)}
            </TableCell>
            <TableCell class="hidden text-end tabular-nums @2xl:table-cell">
              {formatNumber(file.not_finished)}
            </TableCell>
            <TableCell class="text-end">
              <EnergyFigure
                fact={measured ? 'measured' : 'notMeasured'}
                energyWh={file.energy_wh}
              />
            </TableCell>
            {#if priceSet}
              <TableCell class="text-end tabular-nums">
                {#if measured && file.cost !== null}
                  {formatCost(file.cost, currency)}
                {/if}
              </TableCell>
            {/if}
            <TableCell class="hidden text-end tabular-nums @3xl:table-cell">
              {#if file.avg_wh !== null}
                <div class="flex flex-col">
                  <span>{formatEnergy(file.avg_wh)}</span>
                  {#if priceSet && file.avg_cost !== null}
                    <span class="text-muted-foreground text-xs">
                      {formatCost(file.avg_cost, currency)}
                    </span>
                  {/if}
                </div>
              {/if}
            </TableCell>
          </TableRow>
        {/each}
      </TableBody>
    </Table>
  </div>
  {#if total > files.length}
    <p class="text-muted-foreground pt-3 text-xs" data-part="more-files">
      {t('power.energy.moreFiles', { shown: files.length, count: total })}
    </p>
  {/if}
{/if}
