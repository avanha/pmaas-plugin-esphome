package esphome

import (
	"fmt"
	"net"
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi"
)

// defaultNativeAPIPort is the port ESPHome devices serve the native API on.
const defaultNativeAPIPort = "6053"

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

// AddDevice allows a device to connect.
func (c *PluginConfig) AddDevice(device DeviceConfig) {
	c.Devices = append(c.Devices, device)
}

// AddNativeDevice adds a device to connect to over the native API.
func (c *PluginConfig) AddNativeDevice(device NativeDeviceConfig) {
	c.NativeDevices = append(c.NativeDevices, device)
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
