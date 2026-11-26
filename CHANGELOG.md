# Changelog

## 0.6.0 - 2026-11-26

### New

- **Power Control** has a second tab, **Energy**. It shows the energy and the cost of all printers or of one printer over a day, a week, a month or a year, a chart of the period, a table of the printers and a table of the prints. **Download CSV** saves the table on screen. Recording starts when this version is installed; earlier use is not filled in.
- **Settings, Automation** has a new pane, **Energy and cost**: how long records are kept (365 days unless changed), the time zone that days follow, the currency label, the price per kWh and an optional night price with its hours. The first price also covers the energy recorded before it. A later price counts from the moment it is saved, unless you choose to use it for past energy too. Set a price after updating to see cost. With no price, no cost is shown; it is never 0.
- A server in Docker often runs on UTC. Set the time zone in that pane, or accept the offer on the **Energy** tab, so that days and night hours follow your own clock.
- Cost now shows wherever energy does: on the Now tab (today, this month, and cost per hour at the current draw), on the printer's page (this print, last print, since turned on, recent prints) and on the dashboard card (today and this month). All use the price rule of the Energy tab. A printer's page has a link, Energy and cost history, that opens the Energy tab for that printer.
- A small amount reads "< 0.01" in place of "0.00", and the Cost card says why when it has no figure.
- After a price is saved, the Energy and cost pane says what this month costs at that price and links to the Energy tab.
- The Energy tab says what a running month or year is on course for, and the chart can show cost in place of energy.
- The chart on the Energy tab can be used with the keyboard: the arrow keys, Home and End move the period that is read out.
- The Now tab and the dashboard card say "partly measured" beside a figure of today or this month that a plug metered only part of the time, as the Energy tab does.
- A printer that is on and idle can be turned off after a time you set on the Automation tab, for all printers or for one. It is off unless you set it. The printer's page counts down and offers Keep on. It never turns off a printer that is printing, paused, busy or moving, or one Connect cannot reach. While a turn-off after a print waits, the idle time does not run.
- The Energy tab shows how much energy went into printing and how much was used while idle, with the cost of the idle part, and an Idle column for each printer. For energy recorded before this version the split is worked out from the print records.
- The Now tab has a Turn off idle printers button. It asks first, and lists the printers it left on and why. A printer Connect cannot reach stays on.
- The Energy tab has a third table, Files: energy and cost per file name, with the number of prints, the average per print and the prints that did not finish. Download CSV saves it.
- The Plugs tab shows what a Tasmota plug, or a Shelly plug read by its address, does after a power cut: stays off, turns on or returns to the last state. A Tasmota plug set to stay on at all times (PowerOnState 4) gets a warning that says why it cannot be turned off. The setting is only read, never changed.
- Tasmota and Shelly plugs show their Wi-Fi signal, and say so when the plug turned itself off because of too much power, heat or a wrong voltage.
- The extension now tells the server's event stream when it turned a printer off (`auto_off.done`), when a turn-off failed (`auto_off.failed`), and when a plug or its broker went away during a print for more than a minute (`plug.lost`). A webhook or MQTT sink set up on the server receives them. The update asks you to approve these three events; without the approval nothing is sent and nothing else changes.
- The diagnostics bundle an administrator downloads from the server's settings now holds a Power Control file: the driver of each plug and whether it answers, its last error, the state of the MQTT broker and of auto-off. It holds no password, no token and no address.

### Changed

- The extension now keeps energy per printer in 15-minute steps and every finished print in files under `energy/` in its data directory, about 1 MB per printer per year. Shortening the keep time deletes older records for good.
- A plug with no meter shows "Not measured" in the **Energy** tab in place of 0 Wh.
- The wait after a print is at most one day (86400 seconds), for all printers and for one. A larger saved value is brought down to that.
- A Home Assistant plug must be a `switch`, `input_boolean`, `light` or `fan` entity, and its power reading a `sensor`. Other entities, such as covers or locks, are refused when saved and are not switched.
- A port outside 1 to 65535 in a plug, broker or Home Assistant address is no longer accepted; the broker falls back to its default port. The reachability check of the built-in broker runs only when the broker listens on port 1883 or 8883.
- Emptying the price per kWh now says, before you save, that it deletes every saved price and the cost of all past energy.
- The MQTT form keeps Save off while the broker address changed and the saved password is not typed again, so a save no longer drops the password by mistake.
- The forms and the extension now agree on what is the same address: `https://ha.example.com` and `https://ha.example.com:443`, a broker written as `broker.lan`, `broker.lan:1883` or `mqtt://broker.lan`, and a plug written with or without `http://`. Before, a save could be refused or ask for a kept secret that it would have kept.

### Fixed

- A print is no longer recorded as finished and split in two when its printer drops off the network or the server restarts. The record stays open for a short absence and goes on when the printer is back on the same print. A print that was cut, by a power cut for one, gets the outcome `interrupted`, in the Prints table and in the CSV.
- Auto-off after a print now holds through a restart of the server or the extension. Before, a printer whose print ended during the restart stayed on. After a restart the printer is turned off only once Connect has seen it again.
- A printer that drops off the network right after its print stopped is no longer turned off before Connect had time to record how the print ended. A print recorded as cancelled or failed keeps it on, as it does for a printer that is still connected.
- Print start, end and outcome are taken from the server's own job record where it offers one.
- After the extension was stopped for more than ten minutes, the wait after a print starts again once the printer is seen. Before, a printer turned on by hand could be turned off right after the restart for a print that ended before it.
- A printer's settings saved while auto-off was turning that printer off are no longer overwritten by the older settings.
- A plug that stopped answering, or whose MQTT broker is not connected, no longer adds energy. Before, its last reading kept counting until the plug came back. This also lowers the session and print figures on the **Now** tab and on the printer's page for such a time.
- A print that ended while the extension was not running now ends at the last moment it was seen, and is marked as estimated (`end_estimated`), which the **Prints** table says under its end time.
- Energy records are not deleted for their age while the saved energy settings cannot be read, or while fewer than three days of record lie inside the keep time, which is what a server clock that jumped ahead looks like. In the first case the **Energy and cost** pane says so and asks you to check the number of days; saving it ends the hold (`prune_paused` in `GET /energy/settings`).
- A power, voltage or current value that is not a number, such as the `nan` some plugs send for a sensor with no value, is no longer taken as a reading. Before, the answers that carry that plug's readings were empty until the plug sent a number again. A power reading above 100 kW is not counted as energy.
- Usage and energy records are now saved when the extension stops, and two saves can no longer share one temporary file. A temporary file that a cut-off save left in the data directory is removed at the next start.
- Fix a Home Assistant power sensor that stopped answering. When the sensor is `unavailable` or `unknown`, the plug now shows no power reading and no energy is counted; before, the last reading stood and energy went on counting from it. The sensor's own unit is now read, so a sensor in kW or mW is no longer taken as watts, and a sensor that is no power sensor gives no reading.

### Security

- A saved secret is now used only with the address it was saved for. A save or a connection test that keeps the saved MQTT password, Home Assistant token or plug password but names another address is refused with `INVALID_INPUT` and sends nothing there; the forms ask for the secret again as soon as the address changes. For the MQTT broker, another port or turning TLS off counts as another address. Before, a person who may configure the extension could point a test at a server of their own and receive a secret they never saw. This also holds when `mqtt_host` or `ha_url` is changed in the server's settings for the extension: the saved password or token is not used with the new address until it is entered again, and comes back when the setting is removed.
- The password of a Tasmota plug set up by address is no longer part of the error text when the plug does not answer. Before, the plug's page, the answer of a refused switch and the server log could show it.
- A plug or Home Assistant server that answers with a redirect is no longer followed to the other address. The plug's page says so; enter the final address instead.
- Switching between the built-in broker and an external one now asks for the password again, in both directions. Before, the external broker's saved password became the built-in broker's password.
- The built-in broker closes a connection whose first packet is too large, takes at most 32 connections at a time that have not signed in yet, and answers a wrong password after one second.

### API

- New routes: `GET /energy`, `GET /energy/export`, `GET /energy/settings` and `PUT /energy/settings`. Changing the energy settings takes `configure_resource`. `GET /energy/export` answers CSV text in parts, each with an `as_of` to send back with the next part, so that a record made meanwhile does not move the rows.
- New route `GET /energy/summary` (today, this month and the price that counts now). `GET /devices/{device_id}` answers `cost` for the session and the prints, with `currency` and `price_set`. `GET /energy` answers `files`, `files_total` and, on every figure, `print_wh`, `idle_wh`, `idle_cost`, `split_unknown_wh` and `split_estimated`. `GET /energy/export` takes `table=files`.
- New route `POST /devices/{device_id}/idle-off`; new fields `idle_off_min`, `idle_off_pending`, `idle_off_remaining_sec`, `idle_off_error`, `wifi_dbm` and `protection`. `POST /devices/{device_id}/power` takes `idle_only`, which also refuses a printer Connect cannot reach.

## 0.5.0 - 2026-11-20

- Add multi-plug driver support inside a single unified extension sharing safety interlocks, auto-off cooldown, 30-minute power graph, and session/print energy tracking:
  - **Shelly Gen1 (MQTT)** with `shellies/announce` discovery, online LWT, overpower detection, and per-relay power readings.
  - **Shelly Gen2 / Plus / Pro (MQTT)** (`<prefix>/command/switch:<id>` + `<prefix>/status/switch:<id>` JSON telemetry).
  - **Shelly Gen1 & Gen2+ (Direct HTTP)** using Gen2+ RPC (`/rpc/Switch.GetStatus`, `/rpc/Switch.Set`) with automatic fallback to Gen1 REST (`/status`, `/relay/<i>?turn=on|off`).
  - **Zigbee2MQTT** (`<topic>/set`, `<topic>/get`, string & JSON availability, multi-endpoint `state_l<n>`, and power/voltage/current telemetry).
  - **ESPHome (MQTT)** (`<prefix>/command`, `<prefix>/state`, `<node>/status`).
  - **OpenBeken (MQTT)** (`<name>/<ch>/set`, `<name>/<ch>/get`, `<name>/connected`, power/voltage/current).
  - **Generic MQTT** with custom command/state/availability/power topics, custom ON/OFF payloads, and optional JSON key extraction.
  - **Home Assistant (REST)** optional integration (`ha_url` + Long-Lived Access Token, live `POST /ha/test` entity picker for switch + optional power sensor) for TP-Link Kasa/Tapo, Matter, Thread, Z-Wave, and Tuya plugs.
- Split the extension's settings into three tabs: **Plugs** (every printer with its plug and live state), **Connections** (MQTT broker and Home Assistant, each tested and saved on its own) and **Automation** (the power-off rules, and each printer's auto-off choice and own delay).
- Set up a plug in a guided dialog: pick the plug, pick how Connect reaches it, then fill in only the fields that plug needs. The dialog waits for the plug and says whether it answered. A plug and relay another printer already has is refused before saving.
- Add a **Power Control** page in the sidebar with what the fleet draws now, which plugs are on, and buttons that switch each plug. It shows the printers as a table or as cards with the printer's picture.
- A printer can now turn off after every print. The Automation tab and the Power Control page set that default; the switch on the printer's own page still decides for a single print.
- A printer now turns off by itself only after a print that finished. After a cancelled or a failed print it stays on, and a choice made for one print stays armed for the retry. A paused print never turns it off.
- The printer's page says when the plug could not be turned off after a print, and why the printer stayed on.
- Fix auto-off cutting the power of a printer that lost its connection during a print. Auto-off now waits until the printer is back and reports the print as over.
- Add a **Printer power** dashboard card. With room for it, it draws each printer's power over the last 5, 15, 30 or 60 minutes, over the same span the core's temperature card offers, so the two line up. The graph can be switched off in the card's settings. `GET /overview?history=<minutes>` carries the samples, and an hour of samples is kept per printer.
- Hide what a person may not use: the switch buttons and auto-off switches for a person who may not operate the printer, and the plug set-up links for one who may not configure the extension. The settings tabs say when a person may look but not change. `GET /overview` answers `can_configure` and each printer's `can_operate` for this; the routes still ask the host on every change.
- Rebuild the printer's **Power Control** section and the listing card line on the app's own components, in light and dark theme. A plug that does not answer is now said on the card instead of leaving the line empty.
- Read every plug in one request (`GET /overview`), kept to the printers the caller may see, in place of one request per printer card.
- Declare the built-in MQTT broker's port (`needs.listen`, TCP 1883). **The built-in broker now starts only on a port you approved**: after updating, approve it on the extension's Ports tab if you use the built-in broker. The port can be moved there.
- Ask the host whether a person may do what a request asks before it changes anything. Switching a plug and the auto-off of a print take `operate_resource` on the extension and `operate` on that printer. Plugs, connections, tests, discovery and the plug list take `configure_resource` on the extension. A person who may not is refused with `FORBIDDEN`, and the actions are declared under `checks` in `module.yaml`.
- **The built-in MQTT broker now needs a user name and a password**, and it lets in no client that does not sign in with them. After updating, an install whose built-in broker had none stops the broker and says so on the Connections tab: set both there, with **Generate password** if you like, and set the same two values in each plug.
- This extension's own client reaches the built-in broker in process instead of over a loopback socket, and testing the built-in broker no longer opens a second listener on a random port.
- Fix a start failure when `http_host` was written on the extension's host Settings tab. A value written there for `mqtt_host` or `ha_url` now fixes that value, and the Connections tab shows it as read-only.
- Fix a new install starting with an auto-off delay of 0 seconds instead of the default 120.
- Preserve full backwards compatibility with existing `mode: "mqtt"` and `mode: "http"` printer assignments.
- Add unit tests in `drivers_test.go` covering all new plug drivers and legacy config compatibility, and in `host_test.go` covering the declared options, the granted port and the overview.
- Show the Plugs tab as a card for each printer, with the printer's picture, its plug and the buttons that set it up, change it and remove it, in place of a row across the whole page.
- The dashboard card needs no setting up: it shows every printer that has a plug, and with one such printer it is named after that printer and says the state of its plug.
- Draw right in the app's new look (Ember) as well as in 0.1.0-beta.1: the bundle now carries the style rules of the library components it uses, the settings panes pad their own content, and the dashboard card measures the room it has before it draws the graph and shows its figures in its heading while it does.
- Hold the browser bundle to the app's own lint and formatting rules (`pnpm run check`).

## 0.4.1 - 2026-11-19

- Hardwire the optional built-in MQTT 3.1.1 broker to TCP port 1883 and warn when an existing MQTT broker or missing Docker `-p 1883:1883` port mapping is detected.
- Fix **Test Connection** button state when built-in broker mode is active with an empty external host field.
- Fix dark mode error alert text contrast, prevent German/locale thousands separators on port numbers in status badges, and preserve unsaved printer mode drafts during plug scans.
- Declare `require.core`, `options`, and `egress` in `module.yaml` and prepare standalone Marketplace build metadata.

## 0.4.0 - 2026-11-19

- Add optional built-in pure-Go MQTT 3.1/3.1.1 broker (`mqtt_builtin`) in Settings so Tasmota plugs can connect directly without running an external Mosquitto broker.
- Add per-printer Direct HTTP plug connection mode (`mode: "http"`, `http_host`, optional Tasmota Web Password) polling `/cm?cmnd=Status 0` every 5s and switching via verified `POWER<n>` HTTP commands without needing MQTT at all.
- Add end-to-end Go tests (`TestEmbeddedMQTTBrokerEndToEnd` and `TestDirectHTTPTasmotaPollAndSwitch`).

## 0.3.2 - 2026-11-19

- Remove the duplicate sampling interval badge above the power usage graph while keeping the explanatory caption below the chart.
- Add full multi-platform release builds (`linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64`, `windows_amd64`), MIT `LICENSE`, and Prusa shape/checksum verification in CI.

## 0.3.1 - 2026-11-19

- Replace the inline Turn Off confirmation bar with Prusa Connect's native `AlertDialog` modal.
- Display the smart plug power telemetry sampling step (`5s`) on the power usage graph and expose `poll_interval_sec` in the device status API.

## 0.3.0 - 2026-11-19

- Support multi-relay Tasmota devices and `SetOption26 1` (`POWER1`..`POWER8`) with a Relay selector in Settings and per-relay double-assignment locking.
- Send `"1"`/`"0"` commands with QoS 1 (never retained) and honor custom `StateText1..4` and LWT `onln`/`ofln` values from Tasmota discovery.
- Parse JSON and plain-text payloads across `RESULT`, `STATE`, `POWER<n>`, `STATUS8`, and `STATUS10` (`SetOption4 1` and `SetOption90 1`), while ignoring empty `PowerRetain 0` clear payloads.
- Support custom `FullTopic` and `Prefix1..3` layouts from Tasmota discovery and ignore stale `LWT` messages from superseded topic patterns.
- Verify power switch replies within 2.5s (`PowerLock`, `PowerOnState 4`, or invalid relay errors) and display an explicit `Offline` state when LWT is `Offline`.
- Add Go unit tests (`main_test.go`) covering all Tasmota MQTT variants and run them in the extension release workflow.

## 0.2.3 - 2026-11-19

- Fix power usage graph aspect-ratio distortion by moving axis labels and the live endpoint dot to HTML and using `vector-effect="non-scaling-stroke"` with a `100 W` minimum Y-axis scale.
- Add a confirmation step to the **Turn Off** button on the printer page, configurable in **Settings** (`confirm_turn_off`, enabled by default).

## 0.2.2 - 2026-11-18

- Poll assigned Tasmota plugs every 5 seconds via `Status 8` for responsive energy telemetry.
- Add a live SVG power usage graph (`W` over time) to each printer's **Tasmota Power Control** section.
- Track total energy (`Wh`/`kWh`), duration, average power, and peak power while the printer's smart plug is turned on.
- Track per-print file energy usage (`current_print_usage`) until both printing and post-print axis movement stop, and keep the last 10 completed prints (`print_history`) persisted in `usage.json`.

## 0.2.1 - 2026-11-18

- Show connection test and save results in green on success and red on failure.
- Clarify the Auto-off delay after movement stops field and Client ID with helpful descriptions.
- Redesign the Printer Plug Assignments list with explicit Save Assignment button and saved confirmation badge.
- Remove the redundant Toggle Power action from the printer header menu.
- Detect axis movement (`/axis/x`, `/axis/y`, `/axis/z`) so power-off waits until the printer stops moving after a print finishes.
- Log printer human-readable names alongside IDs under `tasmota-power-control`.

## 0.2.0 - 2026-11-18

- Rename extension title to **Tasmota Power Control**.
- Unify extension configuration into a single **Settings** tab with separate Broker Host, Port, and Use TLS controls, inline Username/Password fields, and a live **Test Connection** action.
- Centralize printer-to-Tasmota plug assignment in Settings with double-assignment locking.
- Redesign the printer **Power Control** card using Prusa Connect UI primitives and remove configuration inputs from the printer page.

## 0.1.0 - 2026-11-17

- Initial release with Tasmota MQTT power control, live plug status/power monitoring, active-print safety interlock, and auto-off after print.
