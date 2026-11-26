<!--
  One energy figure in a table, or the words that stand in its place. A plug
  with no meter says "Not measured" and never 0 Wh, and a figure the meter
  covered only in part is marked. The mark stands above the figure where the
  table has little room, and beside it in a wide table.
-->
<script lang="ts">
  import { Badge } from '@prusa/connect-ui/primitives';

  import { type EnergyFact, factText, hasFigure } from './energy';
  import { formatEnergy } from './format';
  import { heldHost } from './host';

  let {
    fact,
    energyWh,
  }: {
    fact: EnergyFact;
    energyWh: number;
  } = $props();

  const t = heldHost().t;
</script>

{#if hasFigure(fact)}
  <span
    class="inline-flex flex-wrap items-center justify-end gap-2 @5xl:flex-nowrap"
    data-fact={fact}
  >
    {#if fact === 'partly'}
      <Badge variant="outline">{factText(t, fact)}</Badge>
    {/if}
    <span class="tabular-nums">{formatEnergy(energyWh)}</span>
  </span>
{:else}
  <span class="text-muted-foreground" data-fact={fact}>
    {factText(t, fact)}
  </span>
{/if}
