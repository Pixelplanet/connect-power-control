// Addresses of the app's own screens this module links to. The host builds the
// address of a module's page; it has no helper for a tab of the module's page
// under Settings, Extensions, so that one is written here.
import type { ModuleHost } from '@prusa/connect-ui/slots';

import { OWN_MODULE } from './own-module';

export type SettingsTab = 'plugs' | 'connections' | 'automation';

const EXTENSIONS = '/settings/extensions';

const ownPage = (): string =>
  OWN_MODULE === ''
    ? EXTENSIONS
    : `${EXTENSIONS}/${encodeURIComponent(OWN_MODULE)}`;

/** One of this module's tabs on its page under Settings, Extensions. */
export const settingsTabHref = (tab: SettingsTab): string =>
  OWN_MODULE === ''
    ? EXTENSIONS
    : `${ownPage()}/${encodeURIComponent(`${OWN_MODULE}.${tab}`)}`;

/** The host's own Ports tab of this module, where a port is approved. */
export const portsTabHref = (): string =>
  OWN_MODULE === '' ? EXTENSIONS : `${ownPage()}/ports`;

/** What the address of the Power Control page may carry. */
export const PANE_PARAM = 'pane';
export const DEVICE_PARAM = 'device';

/**
 * The Energy tab of the Power Control page, narrowed to one printer when
 * one is given. The choice rides in the address, so it also works in a new
 * tab and does not change what the person last chose there.
 */
export const energyHref = (
  host: Pick<ModuleHost, 'pageHref'>,
  deviceId = '',
): string => {
  const base = `${host.pageHref('power')}?${PANE_PARAM}=energy`;

  return deviceId === ''
    ? base
    : `${base}&${DEVICE_PARAM}=${encodeURIComponent(deviceId)}`;
};

/** What the address of the page asks for, undefined when it asks nothing. */
export const askedInAddress = (name: string): string | undefined =>
  new URLSearchParams(window.location.search).get(name) ?? undefined;
