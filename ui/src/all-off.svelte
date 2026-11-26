<!--
  The button that turns off every printer that is on and idle, from the "Now"
  view. It always asks first, whatever the Automation tab says about asking:
  one press here cuts the mains power of many printers.

  It decides nothing itself. Each plug that is on gets the same request its
  own Turn off button sends, one at a time, and the process refuses a printer
  that is printing, paused, busy or moving, or that Connect cannot reach.
  What was refused is handed to
  the page, which lists it. A plug that is off, offline or unknown is not
  asked.
-->
<script lang="ts">
  import { describeFault } from '@prusa/connect-ui/i18n';
  import {
    AlertDialog,
    AlertDialogAction,
    AlertDialogCancel,
    AlertDialogContent,
    AlertDialogDescription,
    AlertDialogFooter,
    AlertDialogHeader,
    AlertDialogTitle,
    Button,
  } from '@prusa/connect-ui/primitives';

  import { heldHost } from './host';
  import { OWN_MODULE } from './own-module';
  import { powerFor } from './power.svelte';
  import type { AllOffResult, PlugListRow, StayedOn } from './summary';

  let {
    rows,
    onresult,
  }: {
    rows: readonly PlugListRow[];
    /** Undefined when a new run starts, then what the run did. */
    onresult: (result: AllOffResult | undefined) => void;
  } = $props();

  const host = heldHost();
  const t = host.t;
  const power = powerFor(host);

  let asking = $state(false);
  let running = $state(false);
  let sent = $state(0);
  let count = $state(0);

  /** The printers whose plug is on. Only these are asked. */
  const on = $derived(rows.filter((row) => row.state === 'on'));
  const offered = $derived(on.some((row) => power.mayOperate(row.id)));

  /**
   * Why the process left a printer on. Its code for a busy printer carries
   * the extension's own name in front, so only the end is compared.
   */
  const reasonOf = (thrown: unknown): string => {
    const fault: { code?: unknown; params?: unknown } =
      typeof thrown === 'object' && thrown !== null ? thrown : {};
    if (typeof fault.code === 'string' && fault.code.endsWith('PRINTER_BUSY')) {
      const params: { state?: unknown } =
        typeof fault.params === 'object' && fault.params !== null
          ? fault.params
          : {};
      const state =
        typeof params.state === 'string' ? params.state.toUpperCase() : '';
      if (state === 'PRINTING') return t('power.page.allOff.reason.printing');
      if (state === 'PAUSED') return t('power.page.allOff.reason.paused');
      if (state === 'UNREACHABLE') return t('power.page.allOff.reason.away');

      return t('power.page.allOff.reason.busy');
    }

    return describeFault(thrown).title;
  };

  const run = async (): Promise<void> => {
    asking = false;
    const targets = [...on];
    running = true;
    sent = 0;
    count = targets.length;
    onresult(undefined);
    const stayed: StayedOn[] = [];
    let done = 0;
    for (const row of targets) {
      if (power.mayOperate(row.id)) {
        try {
          // The store's own switch reads the overview after every plug. Here
          // it is read once, after the last one.
          // A printer Connect cannot reach may be printing: it stays on.
          await power.api.sendPower(row.id, 'off', true);
          done += 1;
        } catch (thrown) {
          stayed.push({ id: row.id, name: row.name, reason: reasonOf(thrown) });
        }
      } else {
        stayed.push({
          id: row.id,
          name: row.name,
          reason: t('power.page.allOff.reason.rights'),
        });
      }
      sent += 1;
    }
    await power.refresh();
    running = false;
    onresult({ done, stayed });
  };
</script>

{#if offered || running}
  <Button
    variant="secondary"
    disabled={running}
    onclick={() => (asking = true)}
    data-part="all-off"
  >
    {running
      ? t('power.page.allOff.running', { done: sent, count })
      : t('power.page.allOff.button')}
  </Button>
{/if}

<AlertDialog
  open={asking}
  onOpenChange={(next: boolean) => {
    asking = next;
  }}
>
  <AlertDialogContent data-module={OWN_MODULE}>
    <AlertDialogHeader>
      <AlertDialogTitle>{t('power.page.allOff.ask')}</AlertDialogTitle>
      <AlertDialogDescription>
        {t('power.page.allOff.askNote', { count: on.length })}
      </AlertDialogDescription>
    </AlertDialogHeader>
    <AlertDialogFooter>
      <AlertDialogCancel>{t('power.switch.cancel')}</AlertDialogCancel>
      <AlertDialogAction variant="destructive" onclick={() => void run()}>
        {t('power.page.allOff.confirm')}
      </AlertDialogAction>
    </AlertDialogFooter>
  </AlertDialogContent>
</AlertDialog>
