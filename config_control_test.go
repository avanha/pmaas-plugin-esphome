package esphome

import (
	"strings"
	"testing"
)

func TestControlLevel_Names(t *testing.T) {
	for level, want := range map[ControlLevel]string{
		ControlOpen: "open", ControlConfirm: "confirm", ControlPassword: "password", ControlReadOnly: "readonly",
		ControlLevel(99): "readonly", // an unknown level is as guarded as can be
	} {
		if got := level.String(); got != want {
			t.Errorf("%d: got %q, want %q", level, got, want)
		}
	}

	var zero ControlLevel
	if zero != ControlOpen {
		t.Error("the default must be open: nothing configured means nothing guarded")
	}
}

func TestSwitchControls_OnlyWhatIsGuardedIsListed(t *testing.T) {
	config := NewPluginConfig()
	config.AddDevice(DeviceConfig{Name: "garage", Password: "x"})
	config.AddDevice(DeviceConfig{Name: "garage2", Password: "x", Control: SwitchControl{Level: ControlConfirm}})
	config.AddNativeDevice(NativeDeviceConfig{Name: "lamp", Address: "10.0.0.1"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "rack", Address: "10.0.0.2", Control: SwitchControl{Level: ControlPassword, Password: "hovno"}})
	config.AddNativeDevice(NativeDeviceConfig{Name: "fridge", Address: "10.0.0.3", Control: SwitchControl{Level: ControlReadOnly}})

	controls, problems := config.switchControls()

	if len(problems) != 0 {
		t.Fatalf("unexpected problems %v", problems)
	}

	if len(controls) != 3 || controls["garage2"].Level != ControlConfirm || controls["rack"] != (SwitchControl{ControlPassword, "hovno"}) ||
		controls["fridge"].Level != ControlReadOnly {
		t.Fatalf("unexpected controls %+v", controls)
	}

	// Left open: not mentioned, which is what open means.
	for _, name := range []string{"garage", "lamp", "nobody"} {
		if _, guarded := controls[name]; guarded {
			t.Errorf("%s is guarded", name)
		}
	}
}

// A mistake mustn't unguard a switch that was meant to be guarded.
func TestSwitchControls_AMistakeMakesASwitchReadOnlyNotOpen(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "nopassword", Address: "a", Control: SwitchControl{Level: ControlPassword}})
	config.AddNativeDevice(NativeDeviceConfig{Name: "unknown", Address: "b", Control: SwitchControl{Level: ControlLevel(42), Password: "x"}})
	config.AddDevice(DeviceConfig{Name: "mqttnopassword", Password: "x", Control: SwitchControl{Level: ControlPassword}})

	controls, problems := config.switchControls()

	for _, name := range []string{"nopassword", "unknown", "mqttnopassword"} {
		if controls[name].Level != ControlReadOnly || controls[name].Password != "" {
			t.Errorf("%s: %+v", name, controls[name])
		}
	}

	if len(problems) != 3 {
		t.Fatalf("unexpected problems %v", problems)
	}

	joined := strings.Join(problems, "|")
	for _, want := range []string{`"nopassword" is set to ask for a password but has none`, `"unknown" has an unknown control level 42`, `"mqttnopassword"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, problems)
		}
	}
}

func TestSwitchControls_AProblemNeverSaysThePassword(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "bad", Address: "a", Control: SwitchControl{Level: ControlLevel(7), Password: "hovno"}})

	_, problems := config.switchControls()

	if strings.Contains(strings.Join(problems, "|"), "hovno") {
		t.Fatalf("a problem gives the password away: %v", problems)
	}
}

func TestPlugin_InitTakesTheGuardsFromTheConfiguration(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "rack", Address: "a", Control: SwitchControl{Level: ControlPassword, Password: "hovno"}})

	p := NewPlugin(config).(*plugin)
	p.Init(&fakeContainer{})

	if got := p.registry.controlOf("rack"); got.Level != ControlPassword || got.Password != "hovno" {
		t.Fatalf("unexpected control %+v", got)
	}

	if got := p.registry.controlOf("lamp"); got.Level != ControlOpen {
		t.Fatalf("an unmentioned device should be open, got %+v", got)
	}
}
