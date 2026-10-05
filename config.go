package esphome

import "fmt"

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

// PluginConfig configures the plugin.
type PluginConfig struct {
	// ListenAddress is the address the embedded MQTT broker listens on. Defaults to ":1883".
	ListenAddress string

	// DiscoveryPrefix is the MQTT discovery topic prefix devices publish their entity configs
	// under. Defaults to "homeassistant", which is ESPHome's default.
	DiscoveryPrefix string

	// Devices lists the devices allowed to connect. A connection from anything else is refused.
	Devices []DeviceConfig

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
