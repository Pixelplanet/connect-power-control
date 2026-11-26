<!--
  What a request was refused with, drawn by its code. It draws nothing while
  there is no fault. `actions` are buttons drawn last inside the note, such
  as one that asks again.
-->
<script lang="ts">
  import type { Snippet } from 'svelte';

  import { describeFault } from '@prusa/connect-ui/i18n';
  import { FaultAlert } from '@prusa/connect-ui/primitives';

  let { fault, actions }: { fault: unknown; actions?: Snippet } = $props();
</script>

{#if fault !== undefined}
  <FaultAlert
    fault={describeFault(fault)}
    {...actions === undefined ? {} : { actions }}
  />
{/if}
