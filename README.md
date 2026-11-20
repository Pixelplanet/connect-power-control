# Connect Power Control (`connect-power-control`)

![Connect Power Control](docs/marketplace_logo.png)

Switches and monitors the smart plug of each printer inside Prusa Connect Local: Tasmota, Shelly, Zigbee2MQTT, ESPHome, OpenBeken, any other MQTT plug, and any switch Home Assistant controls. A plug is reached through an MQTT broker, directly by its IP address, or through Home Assistant.

## Screenshots

### Dark Mode

| Settings & Plug Assignments | Printer Power Control & Live Graph |
|---|---|
| ![Settings (Dark Mode)](docs/01_settings_dark.png) | ![Printer Power Control (Dark Mode)](docs/02_printer_graph_dark.png) |

| Turn-Off Confirmation Dialog |
|---|
| ![Turn-Off Confirmation (Dark Mode)](docs/03_turnoff_modal_dark.png) |

<details>
<summary>Light Mode Screenshots</summary>

| Settings & Plug Assignments | Printer Power Control & Live Graph |
|---|---|
| ![Settings (Light Mode)](docs/04_settings_light.png) | ![Printer Power Control (Light Mode)](docs/05_printer_graph_light.png) |

| Turn-Off Confirmation Dialog |
|---|
| ![Turn-Off Confirmation (Light Mode)](docs/06_turnoff_modal_light.png) |

</details>

## Features

- **Ten plug drivers in one extension**: Tasmota (MQTT or by IP address), Shelly Gen1 and Gen2+ (MQTT or by IP address), Zigbee2MQTT, ESPHome, OpenBeken, custom MQTT topics, and Home Assistant for Kasa, Tapo, Tuya, Matter and the rest.
- **Guided setup**: pick the plug, pick how Connect reaches it, fill in only what that plug needs. Plugs the broker finds and Home Assistant entities are picked from a list. The dialog says whether the plug answered.
- **Three ways to connect**:
  - **Your own MQTT broker**, with TLS, credentials and a connection test.
  - **The built-in MQTT broker** (TCP 1883 by default), for a network with no broker. It listens only on a port you approve, and warns when another broker already answers on that address.
  - **Directly by IP address**, with no broker at all.
- **Active-print safety lock**: turning off is refused while the printer prints, is paused or busy, or while its axes still move.
- **Auto-off after a print**: per printer, after a delay you set for all printers or for one. Turning off by hand asks first unless you switch the question off.
- **Power Control page and dashboard card**: what the fleet draws now, which plugs are on, and buttons that switch each plug.
- **Power graph and energy figures**: a 30-minute graph per printer, the energy since the plug was turned on, and the energy of each print.
- **Broad Tasmota compatibility**: multi-relay devices (`POWER1`–`POWER8`), `SetOption4`, `SetOption26`, `SetOption90`, custom `StateText`, custom `FullTopic` and prefixes, stale `LWT` protection, and verified switching.

## Where to find it

| place | what is there |
|---|---|
| Settings, Extensions, Connect Power Control, **Plugs** | every printer with its plug; set up, change, remove |
| the same page, **Connections** | the MQTT broker and Home Assistant |
| the same page, **Automation** | the power-off rules and each printer's auto-off |
| **Power Control** in the sidebar | the whole fleet at once, as a table or as cards |
| a printer's page, **Power Control** | that printer's plug, graph and energy |

## Building & Testing

```sh
make check    # clones core at CORE_REF into .core/, vets & tests Go, lints, type-checks & builds UI
make release  # builds all platforms declared in module.yaml into release/
```

## The built-in MQTT broker in Docker

If Prusa Connect runs in a bridge-networked Docker container and you switch on the built-in broker, publish its port on the container (`-p 1883:1883`) so the plugs on your network can reach it. Approve the port on the extension's Ports tab first; you can also move it there. A plug reached by its IP address needs no port on the container.
