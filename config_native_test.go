package esphome

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestNativeDeviceConfig_DialAddress(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.7":             "10.0.0.7:6053",
		"10.0.0.7:7000":        "10.0.0.7:7000",
		"plug1.local":          "plug1.local:6053",
		"plug1.local:6053":     "plug1.local:6053",
		" plug1.local ":        "plug1.local:6053",
		"fe80::1":              "[fe80::1]:6053",
		"[fe80::1]":            "[fe80::1]:6053",
		"[fe80::1]:6054":       "[fe80::1]:6054",
		"plug1.home.arpa:6053": "plug1.home.arpa:6053",
	} {
		if got := (NativeDeviceConfig{Address: in}).dialAddress(); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestValidNativeDevices(t *testing.T) {
	goodKey := base64.StdEncoding.EncodeToString(make([]byte, 32))

	config := NewPluginConfig()
	config.AddDevice(DeviceConfig{Name: "mqtt1", Password: "x"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug2", Address: "10.0.0.8", Key: goodKey, DisplayName: "Lamp"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "", Address: "10.0.0.9"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "noaddress"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "badkey", Address: "10.0.0.10", Key: "hunter2"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "mqtt1", Address: "10.0.0.11"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.12"})

	valid, problems := config.validNativeDevices()

	if len(valid) != 2 || valid[0].Name != "plug1" || valid[1].Name != "plug2" {
		t.Fatalf("unexpected valid devices %+v", valid)
	}

	// The first of two with the same name is the one kept, and each of the rest says what's wrong.
	if len(problems) != 5 {
		t.Fatalf("unexpected problems %v", problems)
	}

	for i, want := range []string{
		"has no name", `"noaddress" has no address`, `"badkey"`, `"mqtt1" is already configured as an MQTT device`,
		`"plug1" is already configured as a native API device`,
	} {
		if !strings.Contains(problems[i], want) {
			t.Errorf("problem %d is %q, want one containing %q", i, problems[i], want)
		}
	}
}

func TestPluginConfig_DisplayNamesIncludeNativeDevices(t *testing.T) {
	config := NewPluginConfig()
	config.AddDevice(DeviceConfig{Name: "a", Password: "x", DisplayName: "Alpha"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "b", Address: "x", DisplayName: "Beta"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "c", Address: "x"})

	if got, want := config.displayNames(), map[string]string{"a": "Alpha", "b": "Beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
