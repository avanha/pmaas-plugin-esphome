package esphome

import (
	"reflect"
	"testing"
)

func TestPluginConfig_WithDefaults(t *testing.T) {
	got := PluginConfig{}.withDefaults()
	if got.ListenAddress != ":1883" || got.DiscoveryPrefix != "homeassistant" {
		t.Errorf("defaults not applied: %+v", got)
	}

	custom := PluginConfig{ListenAddress: "127.0.0.1:18830", DiscoveryPrefix: "ha", CaptureMessages: true}
	if got := custom.withDefaults(); !reflect.DeepEqual(got, custom) {
		t.Errorf("explicit values changed: %+v", got)
	}

	// A partly filled in config only gets defaults for what's missing.
	partial := PluginConfig{ListenAddress: "127.0.0.1:18830"}.withDefaults()
	if partial.ListenAddress != "127.0.0.1:18830" || partial.DiscoveryPrefix != "homeassistant" {
		t.Errorf("unexpected result: %+v", partial)
	}
}

func TestPluginConfig_DisplayNames(t *testing.T) {
	config := NewPluginConfig()
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "a", DisplayName: "Left Garage"})
	config.AddDevice(DeviceConfig{Name: "garage2", Password: "b"})

	want := map[string]string{"garage1": "Left Garage"}
	if got := config.displayNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if got := NewPluginConfig().displayNames(); len(got) != 0 {
		t.Errorf("expected no names, got %v", got)
	}
}

func TestPluginConfig_DeviceCredentials(t *testing.T) {
	config := NewPluginConfig()
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "a"})
	config.AddDevice(DeviceConfig{Name: "garage2", Password: "b"})

	got, err := config.deviceCredentials()
	if err != nil {
		t.Fatal(err)
	}

	if want := map[string]string{"garage1": "a", "garage2": "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	config.AddDevice(DeviceConfig{Name: "garage1", Password: "c"})

	if got, err := config.deviceCredentials(); err == nil || got != nil {
		t.Errorf("expected an error and no credentials for a duplicate device, got %v, %v", got, err)
	}
}
