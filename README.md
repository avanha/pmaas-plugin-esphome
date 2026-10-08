# pmaas-plugin-esphome

A PMAAS plugin for [ESPHome](https://esphome.io) devices. It embeds an MQTT broker, so PMAAS stays a
single executable: your ESP32s are configured in ESPHome YAML to connect to PMAAS, announce what they
have, and publish their state. There is no custom firmware.

**Status: phase 1.** Thermometers and door position sensors work. Switches/relays, a status page, TLS
and rendering of door sensors are not done yet (see [Roadmap](#roadmap)).

Verified on a real ESP32 running ESPHome 2026.8.0, with an SHT3x temperature and humidity sensor and a
reed switch: the device connects and its entities are discovered; temperature and humidity reach the
environment plugin and update as the sensor reports; the door sensor reports its position, announces each
open and close, and goes offline and back online as the device does (see Troubleshooting for the one
setting it needs).

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
publish its own discovery configs. It cannot read or write another device's topics. The one exception is
ESPHome's own node discovery (`esphome/discover`, `esphome/ping/<device>`): those topics are shared by
every ESPHome device, and a device left at its defaults uses them on every connect. The broker takes in what
a device sends there and throws it away, so the device is satisfied but nothing is delivered or retained, and
devices can't see each other through it. Set `discover_ip: false` on the device to stop it sending them.
Connectivity comes from the broker's own view of the connection, so a device that drops off shows as
offline as soon as its keepalive lapses, with its last known state kept.

Retained messages live only in the broker's memory, so after PMAAS restarts the broker is empty until
devices reconnect and republish. ESPHome does: on every connect it publishes its birth message
(`<device>/status`), the retained discovery configs, and the current state of every entity, so a restart
of PMAAS repopulates everything within moments of the devices reconnecting. ESPHome also forwards its own log
lines to `<device>/debug`, which is harmless but shows up when `CaptureMessages` is on.

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

  # Recommended. ESPHome's own node discovery (on by default) lets tools find a node's address over MQTT.
  # PMAAS doesn't use it, and the broker accepts and discards what a device sends for it, so leaving it on
  # works, but it's traffic that does nothing for you.
  discover_ip: false

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

The name you give each entity in ESPHome is shown after the device's name, so with `DisplayName: "Left
Garage"` an entity named "Temperature" is shown as "Left Garage Temperature". Name the entities
relative to the device ("Temperature", "Door"), the way ESPHome and Home Assistant expect, rather than
repeating where they are.

## Troubleshooting

**A device connects and is immediately disconnected, over and over.** The device's log shows
`transport_read(): EOF` followed by a reconnect, and PMAAS shows nothing from it except its own debug log
lines. Look in the PMAAS log for a line like this:

```
level=WARN msg="denying publish to a topic the device may not use (an MQTT 3.1.1 client that sent it at QoS 1 or 2 is disconnected)" device=garagedoor1 topic=homeassistant/sensor/other-name/temp/config
```

The broker refuses anything a device does outside its own topics. For a subscription, that just fails.
But MQTT 3.1.1 has no way to refuse a publish, so for one sent at QoS 1 or 2 the broker disconnects the
device, which reconnects and sends it again. (At QoS 0 the message is dropped quietly, so the device stays
connected.) The log line names the topic:

* **Another topic under `esphome/`**: ESPHome's node discovery is tolerated (see above), but nothing else in
  ESPHome's own namespace is. Look at what the device is configured to publish there.
* **Anything under `homeassistant/` for a different name than the device's**: the device is announcing
  itself under a different node name than the one it connects with. Its ESPHome `name` must be the name
  configured for it here, with no MAC suffix (`name_add_mac_suffix: false`, the default).
* **Anything else**: the device's `topic_prefix` isn't its name. Leave it at the default.

**A sensor connects but doesn't appear in PMAAS.** Look for a line like this in the PMAAS log:

```
esphome: garagedoor1: ignoring unsupported binary_sensor "garage_door_position" (device_class ""): if it's a door, set device_class: garage_door (or door) on it in the device's YAML
```

PMAAS decides what an entity is from its `device_class`, which ESPHome only announces if the YAML sets one.
A `binary_sensor` with no `device_class` could be a door, a motion sensor, or anything else, so it isn't
tracked. Set `device_class: garage_door` (or `door`, or `opening`) on it, as in the example above. Likewise a
`sensor` needs `device_class: temperature` or `humidity`. Anything else (lights, switches, and so on) is
ignored for now, and logged the same way.

If the log shows nothing at all, check that the device is listed in the plugin's config, and that its
username and password are right: a failed login is logged too, as `rejecting device connection`.

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
