package broker

import "testing"

func TestAllowed(t *testing.T) {
	const prefix = "homeassistant"

	cases := []struct {
		name   string
		device string
		topic  string
		write  bool
		want   bool
	}{
		{"publish own state", "garage1", "garage1/sensor/temp/state", true, true},
		{"publish own status", "garage1", "garage1/status", true, true},
		{"subscribe own commands", "garage1", "garage1/+/+/command", false, true},
		{"subscribe own tree wildcard", "garage1", "garage1/#", false, true},
		{"publish own discovery config", "garage1", "homeassistant/sensor/garage1/temp/config", true, true},
		{"publish other device's tree", "garage1", "garage2/sensor/temp/state", true, false},
		{"subscribe other device's tree", "garage1", "garage2/#", false, false},
		{"publish other device's discovery config", "garage1", "homeassistant/sensor/garage2/temp/config", true, false},
		{"publish node-less discovery config", "garage1", "homeassistant/sensor/temp/config", true, false},
		{"subscribe discovery configs", "garage1", "homeassistant/#", false, false},
		{"subscribe everything", "garage1", "#", false, false},
		{"subscribe plus-led filter", "garage1", "+/sensor/temp/state", false, false},
		{"publish to broker topics", "garage1", "$SYS/broker/uptime", true, false},
		{"name that merely starts the same", "garage1", "garage10/sensor/temp/state", true, false},
		{"empty device", "", "anything", true, false},

		// ESPHome's node discovery, exactly as ESPHome uses it, and nothing wider.
		{"publish the discover topic", "garage1", "esphome/discover", true, true},
		{"publish its own discover answer", "garage1", "esphome/discover/garage1", true, true},
		{"publish another device's discover answer", "garage1", "esphome/discover/garage2", true, false},
		{"publish under discover with a longer path", "garage1", "esphome/discover/garage1/x", true, false},
		{"subscribe to the discover topic", "garage1", "esphome/discover", false, true},
		{"subscribe to everything under discover", "garage1", "esphome/discover/#", false, true},
		{"subscribe to its own ping topic", "garage1", "esphome/ping/garage1", false, true},
		{"subscribe to another device's ping topic", "garage1", "esphome/ping/garage2", false, false},
		{"publish to its own ping topic", "garage1", "esphome/ping/garage1", true, false},
		{"subscribe to a single level under discover", "garage1", "esphome/discover/+", false, false},
		{"subscribe to another device's discover answer", "garage1", "esphome/discover/garage2", false, false},
		{"subscribe to all of esphome", "garage1", "esphome/#", false, false},
		{"publish to all of esphome", "garage1", "esphome/other", true, false},
		{"a device named like the topic's last segment", "discover", "esphome/discover/discover", true, true},
		{"discover for an empty device", "", "esphome/discover", true, false},
	}

	for _, c := range cases {
		if got := allowed(c.device, prefix, c.topic, c.write); got != c.want {
			t.Errorf("%s: allowed(%q, %q, write=%v) = %v, want %v", c.name, c.device, c.topic, c.write, got, c.want)
		}
	}
}

func TestValidDeviceName(t *testing.T) {
	for _, name := range []string{"garage1", "garage-door_2", "Fan"} {
		if !validDeviceName(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}

	for _, name := range []string{"", "a/b", "a+b", "a#", "a b", "$sys", "a\n"} {
		if validDeviceName(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}
