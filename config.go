package esphome

import (
	"fmt"
	"net"
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi"

	"github.com/avanha/pmaas-spi/alert"
)

// defaultNativeAPIPort is the port ESPHome devices serve the native API on.
const defaultNativeAPIPort = "6053"

// ControlLevel is how carefully a device's switches are guarded against being turned on or off by mistake. A switch
// on the status page is one press of its icon away from changing, which is what you want for a lamp and not for the
// plug a server rack is on.
type ControlLevel int

const (
	// ControlOpen is the default: a press of the switch's icon sends the command.
	ControlOpen ControlLevel = iota

	// ControlConfirm has the page ask "Are you sure?" before it sends the command.
	ControlConfirm

	// ControlPassword has the page ask for a password, which the server checks against SwitchControl.Password
	// before it sends the command. Several wrong ones in a row lock the device's switches for a short while.
	ControlPassword

	// ControlReadOnly shows a switch but never changes it: not from the page, and not when another plugin asks.
	ControlReadOnly
)

func (l ControlLevel) String() string {
	switch l {
	case ControlOpen:
		return "open"
	case ControlConfirm:
		return "confirm"
	case ControlPassword:
		return "password"
	default:
		return "readonly"
	}
}

// SwitchControl is how a device's switches are guarded. Confirming and the password are for a person pressing a
// switch on the page. They don't apply when another plugin turns a switch on or off, which has nobody to ask, but
// ControlReadOnly does: nothing can change a read-only switch.
//
// The password is checked by the server and is never sent to the page, but it is in the configuration in plain
// text, and the page sends what's typed over whatever connection it's served on, so serve it over TLS.
type SwitchControl struct {
	Level ControlLevel

	// Password is what ControlPassword asks for. A device set to ControlPassword with none is treated as read-only,
	// rather than as open: a mistake in the configuration mustn't unguard a switch.
	Password string
}

// DeviceConfig is one ESPHome device allowed to connect to the plugin's broker.
type DeviceConfig struct {
	// Name is the device's ESPHome name. It's the MQTT username the device must connect with
	// (the "username" under "mqtt:" in its YAML) and the root of the topic tree it may use, so its
	// "topic_prefix" must be left at its default, which is this same name.
	Name string

	// Password is the MQTT password the device must connect with.
	Password string

	// DisplayName is the name to show for the device. If empty, the name the device announces for
	// itself is used, falling back to Name.
	DisplayName string

	// Control is how carefully the device's switches are guarded. The default is ControlOpen.
	Control SwitchControl

	// OfflineAlert raises an alert while the device is offline, if it is set with alert.WhenOffline: after how long, and
	// how severe. The default is no alert.
	OfflineAlert alert.OfflineAlert
}

// NativeDeviceConfig is one ESPHome device that PMAAS connects to over the native API, which is what stock
// firmware (a Kauf plug, an Athom relay) has enabled, unlike MQTT. The device is the server, so PMAAS is told
// where it is, and nothing on the device needs configuring.
type NativeDeviceConfig struct {
	// Name is the device's ESPHome name. PMAAS checks that the device it reaches at Address says this, so
	// an address that has been handed to a different device is refused rather than trusted.
	Name string

	// Address is the device's host name or IP address, and optionally a port: "plug1.local",
	// "10.0.0.7:6053". The port defaults to 6053.
	Address string

	// Key is the device's api: encryption: key, if it has one: 32 bytes, base64 encoded, as it appears in the
	// device's YAML. Leave it empty for a device that isn't encrypted, which includes stock Kauf and Athom
	// firmware.
	Key string

	// DisplayName is the name to show for the device. If empty, the name the device calls itself is used.
	DisplayName string

	// Control is how carefully the device's switches are guarded. The default is ControlOpen.
	Control SwitchControl

	// OfflineAlert raises an alert while the device is offline, if it is set with alert.WhenOffline: after how long, and
	// how severe. The default is no alert. A device that has never connected counts as offline from when the plugin
	// started.
	OfflineAlert alert.OfflineAlert
}

// dialAddress is Address with the default port added if it has none.
func (d NativeDeviceConfig) dialAddress() string {
	address := strings.TrimSpace(d.Address)

	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}

	// A bare IPv6 address has colons of its own, and needs its brackets to take a port.
	return net.JoinHostPort(strings.Trim(address, "[]"), defaultNativeAPIPort)
}

// PluginConfig configures the plugin.
type PluginConfig struct {
	// ListenAddress is the address the embedded MQTT broker listens on. Defaults to ":1883".
	ListenAddress string

	// DiscoveryPrefix is the MQTT discovery topic prefix devices publish their entity configs
	// under. Defaults to "homeassistant", which is ESPHome's default.
	DiscoveryPrefix string

	// Devices lists the devices allowed to connect to the MQTT broker. A connection from anything else is
	// refused. If there are none, the broker isn't started.
	Devices []DeviceConfig

	// NativeDevices lists the devices PMAAS connects to over the native API.
	NativeDevices []NativeDeviceConfig

	// handles are the assembly's handles on the devices, by the device's name, as AddDevice and AddNativeDevice
	// return them. The plugin copies the configuration, and the copy shares this map.
	handles map[string]*DeviceHandle

	// CaptureMessages logs every message a device publishes. It's for working out what a new kind
	// of device actually sends; leave it off otherwise.
	CaptureMessages bool
}

func NewPluginConfig() PluginConfig {
	return PluginConfig{ListenAddress: ":1883", DiscoveryPrefix: "homeassistant"}
}

// withDefaults returns c with the default listen address and discovery prefix filled in wherever
// they were left empty.
func (c PluginConfig) withDefaults() PluginConfig {
	defaults := NewPluginConfig()

	if c.ListenAddress == "" {
		c.ListenAddress = defaults.ListenAddress
	}

	if c.DiscoveryPrefix == "" {
		c.DiscoveryPrefix = defaults.DiscoveryPrefix
	}

	return c
}

// displayNames maps the name of each device that has a DisplayName to it.
func (c PluginConfig) displayNames() map[string]string {
	names := make(map[string]string)

	for _, device := range c.Devices {
		if device.DisplayName != "" {
			names[device.Name] = device.DisplayName
		}
	}

	for _, device := range c.NativeDevices {
		if device.DisplayName != "" {
			names[device.Name] = device.DisplayName
		}
	}

	return names
}

// offlineAlerts is the alert each device is to have for going offline, by the device's name, for those that are to have one.
func (c PluginConfig) offlineAlerts() map[string]alert.OfflineAlert {
	alerts := make(map[string]alert.OfflineAlert)

	for _, device := range c.Devices {
		if device.OfflineAlert.Enabled() {
			alerts[device.Name] = device.OfflineAlert
		}
	}

	for _, device := range c.NativeDevices {
		if device.OfflineAlert.Enabled() {
			alerts[device.Name] = device.OfflineAlert
		}
	}

	return alerts
}

// deviceCredentials maps each device's name to its password. It's an error for a name to appear more
// than once, since there'd be no telling which password is the right one.
func (c PluginConfig) deviceCredentials() (map[string]string, error) {
	credentials := make(map[string]string, len(c.Devices))

	for _, device := range c.Devices {
		if _, duplicate := credentials[device.Name]; duplicate {
			return nil, fmt.Errorf("device %q is configured more than once", device.Name)
		}

		credentials[device.Name] = device.Password
	}

	return credentials, nil
}

// AddDevice allows a device to connect. The handle it returns is for getting at the device's entities from the
// assembly, and can be ignored.
func (c *PluginConfig) AddDevice(device DeviceConfig) *DeviceHandle {
	c.Devices = append(c.Devices, device)

	return c.handleFor(device.Name)
}

// AddNativeDevice adds a device to connect to over the native API. The handle it returns is for getting at the
// device's entities from the assembly, and can be ignored.
func (c *PluginConfig) AddNativeDevice(device NativeDeviceConfig) *DeviceHandle {
	c.NativeDevices = append(c.NativeDevices, device)

	return c.handleFor(device.Name)
}

// handleFor is the handle for the named device, made the first time it's asked for.
func (c *PluginConfig) handleFor(name string) *DeviceHandle {
	if c.handles == nil {
		c.handles = make(map[string]*DeviceHandle)
	}

	handle, ok := c.handles[name]
	if !ok {
		handle = newDeviceHandle(name)
		c.handles[name] = handle
	}

	return handle
}

// validNativeDevices returns the native devices that can be connected to, and a description of what's wrong with
// each that can't. One bad entry mustn't take the others down: unlike the MQTT broker, which is one thing, each
// is a connection of its own.
func (c PluginConfig) validNativeDevices() (valid []NativeDeviceConfig, problems []string) {
	taken := make(map[string]string, len(c.Devices)+len(c.NativeDevices))

	for _, device := range c.Devices {
		taken[device.Name] = "an MQTT device"
	}

	for _, device := range c.NativeDevices {
		switch {
		case strings.TrimSpace(device.Name) == "":
			problems = append(problems, "a native API device has no name")
		case strings.TrimSpace(device.Address) == "":
			problems = append(problems, fmt.Sprintf("native API device %q has no address", device.Name))
		case taken[device.Name] != "":
			problems = append(problems, fmt.Sprintf("native API device %q is already configured as %s", device.Name, taken[device.Name]))
		default:
			if device.Key != "" {
				if err := nativeapi.CheckKey(device.Key); err != nil {
					problems = append(problems, fmt.Sprintf("native API device %q: %v", device.Name, err))
					continue
				}
			}

			taken[device.Name] = "a native API device"

			valid = append(valid, device)
		}
	}

	return valid, problems
}

// switchControls is how each device's switches are guarded, by the device's name, for the devices that are guarded
// at all, and what's wrong with any that are guarded in a way that makes no sense. Those are made read-only: a
// switch that can't be changed is safe, and one that was meant to be guarded and is open is not.
func (c PluginConfig) switchControls() (controls map[string]SwitchControl, problems []string) {
	controls = make(map[string]SwitchControl)

	guard := func(name string, control SwitchControl) {
		switch control.Level {
		case ControlOpen:
			return
		case ControlConfirm, ControlReadOnly:
		case ControlPassword:
			if control.Password == "" {
				problems = append(problems, fmt.Sprintf(
					"device %q is set to ask for a password but has none, so its switches are read-only", name))
				control = SwitchControl{Level: ControlReadOnly}
			}
		default:
			problems = append(problems, fmt.Sprintf(
				"device %q has an unknown control level %d, so its switches are read-only", name, int(control.Level)))
			control = SwitchControl{Level: ControlReadOnly}
		}

		controls[name] = control
	}

	for _, device := range c.Devices {
		guard(device.Name, device.Control)
	}

	for _, device := range c.NativeDevices {
		// A name that's configured twice is only used once, and the first is the one that is (see
		// validNativeDevices), so its guard is the one that counts.
		if _, taken := controls[device.Name]; !taken {
			guard(device.Name, device.Control)
		}
	}

	return controls, problems
}
