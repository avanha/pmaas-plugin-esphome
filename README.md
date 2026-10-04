# pmaas-plugin-esphome

A PMAAS plugin for [ESPHome](https://esphome.io) devices. It embeds an MQTT broker, so PMAAS stays a
single executable: your ESP32s are configured in ESPHome YAML to connect to PMAAS, announce what they
have, and publish their state. There is no custom firmware.

**Status: phase 1.** Thermometers and door position sensors work. Switches/relays, a status page, TLS
and rendering of door sensors are not done yet (see [Roadmap](#roadmap)).

## What you get

| ESPHome entity | Becomes | Seen by |
|---|---|---|
| `sensor` with `device_class: temperature` | a wireless thermometer entity | the environment plugin, which renders and tracks it like a bluetooth thermometer |
| `sensor` with `device_class: humidity` | folded into the thermometer, if the device has exactly one temperature sensor | the same |
| `binary_sensor` with `device_class: garage_door`, `door` or `opening` | a door sensor (`pmaas-spi/device`: `IDoorSensor`) | anything that subscribes to its events |

Anything else a device announces is logged and ignored. A door sensor reports `Open`, `Closed` or
`Unknown`, when it last changed, and whether the device is currently connected; it broadcasts a
`device.DoorPositionChangedEvent` when the position changes.

## How it works

1. A device connects to the broker with its own username and password.
2. It publishes a retained *discovery* config per entity (this is ESPHome's Home Assistant style MQTT
   discovery, on by default): `homeassistant/<component>/<device>/<object>/config`.
3. It publishes state to the topics those configs name, and its birth/last-will messages.
4. The plugin registers an entity per supported config, and turns state messages into PMAAS events.

The broker only lets a device use its own topics: it may publish and subscribe under `<device>/`, and
publish its own discovery configs. It cannot read or write another device's topics. Connectivity comes
from the broker's own view of the connection, so a device that drops off shows as offline as soon as
its keepalive lapses, with its last known state kept.

Retained messages live only in the broker's memory, so after PMAAS restarts the broker is empty until
devices reconnect and republish. ESPHome is expected to republish its discovery configs and state on
every connect; confirm that with `CaptureMessages` on your first device.

## Configuring the plugin

```go
conf := esphome.NewPluginConfig()           // listens on :1883, discovery prefix "homeassistant"
conf.AddDevice(esphome.DeviceConfig{
	Name:        "garage-left",              // must match the device's ESPHome name
	Password:    "a long random password",
	DisplayName: "Left Garage",              // optional
})
coreConfig.AddPlugin(esphome.NewPlugin(conf), config.PluginConfig{})
```

* `Name` is both the MQTT username the device connects with and the root of the topic tree it owns, so
  the device's `topic_prefix` must be left at ESPHome's default, which is its name. Names cannot contain
  `/`, `+`, `#`, `$` or whitespace.
* `DisplayName` is shown in front of each entity's own name ("Left Garage Door"). Without it the name
  the device announces for itself is used.
* Devices not listed here are refused. A device listed twice, an invalid name, a missing password, or an
  address already in use stops the broker from starting; the plugin logs why and the rest of PMAAS runs.
* `CaptureMessages: true` logs every message devices publish. Use it the first time you connect a new
  kind of device to see exactly what it sends.

The broker listens in plain text. Use it on a trusted LAN only, and give each device its own password.

## Example ESPHome configuration

A garage door position detector (a reed switch on GPIO) with a temperature/humidity sensor:

```yaml
esphome:
  name: garage-left            # the PMAAS device Name; also the default topic_prefix

esp32:
  board: esp32dev

wifi:
  ssid: !secret wifi_ssid
  password: !secret wifi_password

logger:

# MQTT only, so there is deliberately no `api:` section: ESPHome reboots a device that has an `api:`
# section but no API client connected, unless that section sets `reboot_timeout: 0s`.
mqtt:
  broker: pmaas.local          # the machine running PMAAS
  port: 1883
  username: garage-left        # same as the name
  password: !secret mqtt_password
  # discovery is on by default and publishes to the "homeassistant" prefix, as the plugin expects.

binary_sensor:
  - platform: gpio
    name: "Door"
    device_class: garage_door
    pin:
      number: GPIO4
      mode: INPUT_PULLUP
      inverted: true           # wire the reed switch so ON means open
    filters:
      - delayed_on_off: 100ms  # debounce

sensor:
  - platform: dht
    pin: GPIO5
    model: DHT22
    temperature:
      name: "Temperature"
    humidity:
      name: "Humidity"
    update_interval: 60s
```

`ON` means open and `OFF` closed, as with Home Assistant's door sensors. If you flip the wiring the
wrong way around, flip `inverted:` rather than changing anything in PMAAS.

This YAML follows ESPHome's documentation but has not been run against a real device yet; treat the
first connection as a test, with `CaptureMessages` on.

## Wiring it into an assembly

```go
import esphome "github.com/avanha/pmaas-plugin-esphome"

func addEsphome(coreConfig *config.Config) {
	conf := esphome.NewPluginConfig()
	conf.CaptureMessages = true // while setting up
	conf.AddDevice(esphome.DeviceConfig{Name: "garage-left", Password: "...", DisplayName: "Left Garage"})
	coreConfig.AddPlugin(esphome.NewPlugin(conf), config.PluginConfig{})
}
```

## Roadmap

1. **Phase 1 (this):** broker with per-device authentication and ACLs, discovery parsing, thermometers
   and door sensors, connectivity, tests.
2. **Phase 2:** switches/relays (`ISwitch`, commands to the device's `command_topic`), a status page
   listing devices and entities, and a renderer for door sensors.
3. **Later:** TLS (the ACME plugin's certificate), a Tasmota dialect for devices that can't run
   ESPHome, signal strength as RSSI.

## Not supported, on purpose

* Home Assistant `value_template` (Jinja). An entity that needs one is skipped with a log line.
  ESPHome publishes plain values and doesn't need one.
* More than one temperature sensor sharing a humidity sensor on one device: with several temperature
  sensors there is no telling which humidity sensor belongs to which, so none is attached.
* ESPHome's native API. Only MQTT is used.
