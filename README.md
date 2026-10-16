# pmaas-plugin-esphome

A PMAAS plugin for [ESPHome](https://esphome.io) devices. It embeds an MQTT broker, so PMAAS stays a
single executable: your ESP32s are configured in ESPHome YAML to connect to PMAAS, announce what they
have, and publish their state. There is no custom firmware.

**Status: phase 3.** Thermometers, door position sensors, switches (relays and smart plugs, which can be turned
on and off from PMAAS) and power monitoring work over either of two transports: the embedded MQTT broker, or the
ESPHome **native API**, which stock firmware (Kauf plugs, Athom relays) speaks with no MQTT and no reflashing. There
is an **ESPHome** status page. Finding native devices by mDNS, adopting them from the page, and TLS are not done
yet (see [Roadmap](#roadmap)).

The MQTT transport has been verified on real hardware (below). The native API, switches and power monitoring have
only been tested against simulated devices so far.

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

| `switch` | a switch (`pmaas-spi/device`: `ISwitch`): on, off, or unknown, and a way to ask for either | anything that subscribes to its events, and the buttons on the status page |
| `sensor` with `device_class: power`, `voltage`, `current` or `energy` | one **power monitor** per device (`device.IPowerMonitor`), carrying whichever of those the device has | the same |

Anything else a device announces is logged and ignored. A door sensor reports `Open`, `Closed` or
`Unknown`, when it last changed, and whether the device is currently connected; it broadcasts a
`device.DoorPositionChangedEvent` when the position changes.

## Switches

A switch reports `On`, `Off` or `Unknown` (a switch that hasn't said yet isn't assumed to be off). Asking it to
change, from the status page or through `ISwitch.SetState`, publishes to the switch's command topic and returns
at once; the switch's state changes when the *device* reports it has, so a command that's ignored leaves it as
it was. Asking is refused, with the reason, if the device is offline, didn't announce a command topic, or the
broker isn't running.

PMAAS publishes commands itself, with no ACL in the way, so it only ever does so to the device's own topics: an
entity whose announced command topic is outside the device's tree (`<name>/...`) is refused, so one device can't
make PMAAS command another.

## Guarding a switch

A switch on the page is one press of its icon from changing, which is what you want for a lamp and not for the plug a
server rack is on. Each device, MQTT or native, can say how carefully its switches are guarded, with `Control`:

```go
conf.AddNativeDevice(esphome.NativeDeviceConfig{
	Name: "kauf-plug-7aad7c", Address: "10.47.6.71", DisplayName: "Server Rack",
	Control: esphome.SwitchControl{Level: esphome.ControlPassword, Password: "a password"},
})
```

| Level | What a person at the page has to do | What another plugin has to do |
|---|---|---|
| `ControlOpen` (the default) | press the icon | nothing |
| `ControlConfirm` | press it, and say yes when asked | nothing |
| `ControlPassword` | press it, and type the password | nothing |
| `ControlReadOnly` | nothing helps: the switch shows its state and won't change | nothing helps either |

* **Confirm and password are for people.** They apply to the icon on the page, and not when another plugin turns a
  switch on or off through its `ISwitch`, since an automation has nobody to ask. Read-only is for everyone: nothing
  can change the switch.
* **The server decides.** The page asks, but what's allowed is checked on the server from what the request carries, so
  a request that skips the question is refused the same way. The password is never sent to the page, and is compared
  in constant time.
* **Wrong passwords are limited.** Five wrong ones in a row lock all of the device's switches for 30 seconds, during
  which even the right password is refused; a right one starts the count again. Each wrong one is logged, without
  what was typed.
* **A mistake is read-only, never open.** A device set to `ControlPassword` with no password, or with a level that
  doesn't exist, has its switches made read-only and the plugin says why when it starts.
* **The password is in the configuration in plain text,** and the page sends what's typed over whatever connection
  it's served on, so serve the page over TLS if the network isn't entirely yours. The prompt is a dialog of its own,
  not the browser's, so that what's typed is masked.

A guarded switch shows a lock (read-only), a key (password) or a shield (confirm) beside its state.

## Power monitors

A device's power, voltage, current and energy sensors become one entity, with a flag for each reading saying
whether the device has reported it: a device with no voltage sensor has no voltage, which isn't zero volts.
Readings are converted to watts, volts, amps and kilowatt-hours whatever unit the device uses (`kW`, `mW`, `mV`,
`mA`, `Wh` and `MWh` are understood; a sensor with any other unit is ignored and logged). If a device has two
sensors of one kind the first by object id is used. What energy counts from (the device's lifetime, or since
midnight) is up to the device.

## Alerting when a device goes offline

A device can raise an alert on the alert console while it is offline. It is off unless a device asks for it, with
`alert.WhenOffline` from `pmaas-spi/alert`, on either kind of device:

```go
conf.AddDevice(esphome.DeviceConfig{
	Name: "garagedoor1", Password: "...",
	OfflineAlert: alert.WhenOffline(5*time.Minute, alert.Always(alert.SeverityCritical)),
})
```

The first argument is how long the device has to have been offline (zero is as soon as it is noticed). The second says how
severe the alert is, and may depend on the time of day (`alert.ByTimeOfDay`); with `nil` it is a warning. The alert clears
when the device connects again. A device that has never connected counts as offline from when the plugin started. The
alert is `esphome` / `offline:<device name>`, and is looked at once a minute as well as when the device connects or
disconnects, so that it comes due without anything happening.

## The status page

The **ESPHome** entry in the menu opens a page like the other plugins': a status bar, then a card for each device.

* **The status bar** shows whether the broker is running (and why not, if it should be, or that it isn't needed
  when every device is on the native API), where it listens, how many of the configured devices are connected, how
  many entities of each kind there are, and when a device last sent anything.
* **A device's card** shows its name, how it's reached (MQTT or Native API), whether it's connected and since when,
  and what it said about itself: address, firmware, model, MAC. If a native API device can't be reached, the reason
  is on its card. Inside the card is everything of the device's that's tracked, each in a block of its own. A device
  that has nothing tracked yet, or has never connected, still has its card.
* **What's inside** is ordered by kind and then by name. Switches come first, then power monitors, door sensors and
  thermometers, as set by the priorities in `childorder.go`: the higher the number the earlier, and a kind that isn't
  listed comes after all that are, so a new kind appears at the end until someone decides where it belongs. To move a
  kind up, raise its number; a negative one puts it below the unlisted.
* **A switch** shows its state, and the toggle icon is the control: press it to ask for the other state. It's only a
  picture while the device is offline or can't be commanded. The state changes when the device reports it, so the page
  reloads a second after the request to show it. A thermometer shows its temperature and humidity, a door sensor its
  position and for how long, and a power monitor the readings the device has. What an offline device last said is shown
  dimmed.

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
2. **Phase 2 (done):** the status page, door sensor renderer, switches (`ISwitch`, commands to the device's
   `command_topic`) and power monitoring.
3. **Phase 3 (done, apart from real hardware):** the ESPHome native API as a second transport next to the MQTT
   broker, so stock firmware that has no MQTT (Kauf and Athom plugs and relays) works without reflashing, with
   Noise encryption and the entity mapping above shared by both transports.
4. **Next:** finding native devices by mDNS (`_esphomelib._tcp`), listing them on the status page, and adopting one
   from the page, with what's adopted kept in the core's config store.
5. **Later:** BLE advertisements, TLS (the ACME plugin's certificate), a Tasmota dialect for devices that can't
   run ESPHome, signal strength as RSSI.

## Devices on the native API

A stock Kauf plug or Athom relay has the native API on port 6053 and no MQTT. PMAAS connects to it, so there's
nothing on the device to set up: tell PMAAS where it is.

```go
conf := esphome.NewPluginConfig()
conf.AddNativeDevice(esphome.NativeDeviceConfig{
	Name:        "kauf-plug-1",  // the device's ESPHome name; refused if the device at Address says something else
	Address:     "10.0.0.7",     // or "plug1.local"; the port defaults to 6053
	DisplayName: "Space Heater", // optional
	// Key: "...",               // the device's api: encryption: key, if it has one
})
```

A device on both lists is an error. With only native devices configured the broker isn't started, and the page says
so. Each device is its own connection: one that can't be reached, or has the wrong key, is shown on the page with
what's wrong, and retried with a growing delay (a minute at most) without affecting the others. A device that
reboots is reconnected and asked again for everything it has, so entities it no longer has are dropped.

PMAAS asks each device who it is, what entities it has, and to report their states, then maps the entities exactly
as it does for MQTT: `switch` entities, `sensor` entities with a `device_class` of temperature, humidity, power,
voltage, current or energy, and `binary_sensor` entities with a `device_class` of `garage_door`, `door` or
`opening`. Anything else is logged and ignored. Turning a switch on or off sends the device a command, and the
switch's state changes when the device reports it.

Not every device sends each entity's object id, which is what the plugin keys things by: a Kauf plug on ESPHome
2025.8.2 sent none for its sensors. When there isn't one it's worked out from the entity's name the way ESPHome does
(`Total Daily Energy` is `total_daily_energy`), so the entity is the same either way, and two entities that would end
up with the same one have their key put on the end, so readings can never land on the wrong sensor.

**Security.** Stock Kauf and Athom firmware runs the native API unencrypted, with no password, so anyone on the
network can read it and command the device. Keep such devices on a network you trust (an IoT VLAN), or put an
`api: encryption: key:` in their firmware and give PMAAS the key. Encryption is Noise
(`Noise_NNpsk0_25519_ChaChaPoly_SHA256`) with that 32 byte key. If the wrong key, or none, is configured the page
says which.

### How the client works

`internal/nativeapi` is the client. It connects, does the encryption handshake if there's a key and the API hello,
and delivers every message to a handler, answering pings and time requests itself, with a keepalive that gives up on
a device that has gone quiet. A message it doesn't recognise is skipped rather than ending the connection, since a
device on newer firmware than this was generated from is normal.

The protobuf bindings (`internal/nativeapi/pb`) are generated from ESPHome's `api.proto` as vendored by Home
Assistant's client library, which is MIT licensed, pinned to the tag in `internal/nativeapi/proto/SOURCE`. To
update them, replace the two `.proto` files with a newer tag's and run `internal/nativeapi/generate.sh`, which
needs only Go. There was no Go client worth depending on: the maintained candidate ends the connection on a
message type it doesn't know and doesn't check the device's reason for refusing a key.

It's tested against a fake device written from the protocol, in both framings. The encrypted side has not yet been
tried against real firmware or Home Assistant's own client (that library couldn't be installed where this was
written), so a first connection to a device with an `api: encryption:` key is the real test of it.

## Not supported, on purpose

* Home Assistant `value_template` (Jinja). An entity that needs one is skipped with a log line.
  ESPHome publishes plain values and doesn't need one.
* More than one temperature sensor sharing a humidity sensor on one device: with several temperature
  sensors there is no telling which humidity sensor belongs to which, so none is attached.
* ESPHome's native API. Only MQTT is used.
