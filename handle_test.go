package esphome

import (
	"errors"
	"testing"

	"github.com/avanha/pmaas-spi/common"
	"github.com/avanha/pmaas-spi/device"
)

func (f *fixture) withHandle(deviceName string) *DeviceHandle {
	f.t.Helper()

	handle := newDeviceHandle(deviceName)
	f.registry.handles = map[string]*DeviceHandle{deviceName: handle}

	return handle
}

func TestHandle_TheConfigHandsOutOneHandleForADeviceAndItsCopySharesIt(t *testing.T) {
	conf := NewPluginConfig()
	first := conf.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.1"})
	mqtt := conf.AddDevice(DeviceConfig{Name: "garage1"})

	if first == nil || mqtt == nil || first == mqtt || first.Name() != "plug1" || mqtt.Name() != "garage1" {
		t.Fatalf("handles: %v %v", first, mqtt)
	}

	copied := conf.withDefaults()

	if copied.handles["plug1"] != first || copied.handles["garage1"] != mqtt {
		t.Error("the plugin's copy of the configuration doesn't have the assembly's handles")
	}
}

func TestHandle_ASwitchTakenBeforeTheDeviceHasConnectedLooksLikeOneThatHasNotReportedAndRefusesCommands(t *testing.T) {
	handle := newDeviceHandle("garage1")
	plug := handle.Switch("Garage 1 Plug")

	if got := plug.GetSwitchData(); got.Name != "Garage 1 Plug" || got.HasData || got.State != device.SwitchStateUnknown {
		t.Errorf("data: %+v", got)
	}

	err := plug.SetState(device.SwitchStateOn)
	if !errors.Is(err, common.ErrStubNotAvailable) || !errors.Is(err, errNoSwitch) {
		t.Errorf("set: %v", err)
	}

	// The same goes for the device's only switch, before it has announced any.
	err = handle.Switch("").SetState(device.SwitchStateOn)
	if !errors.Is(err, common.ErrStubNotAvailable) || !errors.Is(err, errNoSwitch) {
		t.Errorf("set the only switch: %v", err)
	}
}

func TestHandle_TheSameProxyWorksOnceTheDeviceHasAnnouncedTheSwitch(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	handle := f.withHandle("garage1")
	plug := handle.Switch("")

	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.state("garage1", "garage1/switch/plug/state", "ON")

	if got := plug.GetSwitchData(); got.Name != "Garage 1 Plug" || got.State != device.SwitchStateOn {
		t.Errorf("data: %+v", got)
	}

	if err := plug.SetState(device.SwitchStateOff); err != nil || len(*sent) != 1 {
		t.Errorf("set: %v, published %v", err, *sent)
	}
}

func TestHandle_SwitchByNameOrTheOnlyOne(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	if got := handle.Switch("").GetSwitchData(); got.Name != "Garage 1 Plug" {
		t.Errorf("the only switch: %+v", got)
	}

	if got := handle.Switch("Garage 1 Plug").GetSwitchData(); got.Name != "Garage 1 Plug" {
		t.Errorf("by name: %+v", got)
	}

	if err := handle.Switch("Nothing").SetState(device.SwitchStateOn); !errors.Is(err, errNoSwitch) {
		t.Errorf("an unknown name: %v", err)
	}

	f.discover("garage1", "switch", "fan", switchConfig("garage1", "fan", "Fan"))

	if err := handle.Switch("").SetState(device.SwitchStateOn); !errors.Is(err, errSeveralSwitches) {
		t.Errorf("two switches and no name: %v", err)
	}

	if got := handle.Switch("").GetSwitchData(); got.HasData || got.Name != "" {
		t.Errorf("data of an ambiguous switch: %+v", got)
	}

	if got := handle.Switch("Garage 1 Fan").GetSwitchData(); got.Name != "Garage 1 Fan" {
		t.Errorf("the fan: %+v", got)
	}
}

func TestHandle_ADeviceWithoutAHandleIsNotAProblem(t *testing.T) {
	f := newFixture(t)
	f.discover("other", "switch", "plug", switchConfig("other", "plug", "Plug"))

	handle := f.withHandle("garage1")

	if err := handle.Switch("").SetState(device.SwitchStateOn); !errors.Is(err, errNoSwitch) {
		t.Errorf("another device's switch reached the handle: %v", err)
	}
}

func TestHandle_ASwitchTheDeviceRemovesAndAnnouncesAgainIsFoundAgainByTheSameProxy(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	handle := f.withHandle("garage1")
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	plug := handle.Switch("")

	f.registry.HandleMessage("garage1", "homeassistant/switch/garage1/plug/config", nil) // the device removes it

	if err := plug.SetState(device.SwitchStateOn); !errors.Is(err, errNoSwitch) {
		t.Fatalf("a removed switch can still be set: %v", err)
	}

	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	if err := plug.SetState(device.SwitchStateOn); err != nil || len(*sent) != 1 {
		t.Errorf("after being announced again: %v, published %v", err, *sent)
	}
}

func TestHandle_ARenamedSwitchIsFoundByItsNewName(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Heater"))

	if got := handle.Switch("Garage 1 Heater").GetSwitchData(); got.Name != "Garage 1 Heater" {
		t.Errorf("by its new name: %+v", got)
	}

	if err := handle.Switch("Garage 1 Plug").SetState(device.SwitchStateOn); !errors.Is(err, errNoSwitch) {
		t.Errorf("by its old name: %v", err)
	}
}

func TestHandle_ShuttingDownTakesTheStubsBackSoNobodyCallsOneThatWouldPanic(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	plug := handle.Switch("")

	f.registry.Shutdown()

	if err := plug.SetState(device.SwitchStateOn); !errors.Is(err, errNoSwitch) {
		t.Errorf("after shutdown: %v", err)
	}

	if got := plug.GetSwitchData(); got.HasData {
		t.Errorf("data after shutdown: %+v", got)
	}
}

func TestHandle_AProxyHonoursReadOnlyAndSkipsThePasswordPrompt(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	f.registry.control = map[string]SwitchControl{"garage1": {Level: ControlPassword, Password: "secret"}}
	handle := f.withHandle("garage1")
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	plug := handle.Switch("")

	if err := plug.SetState(device.SwitchStateOn); err != nil || len(*sent) != 1 {
		t.Fatalf("a password-guarded switch turned on by the assembly: %v, published %v", err, *sent)
	}

	f.registry.control = map[string]SwitchControl{"garage1": {Level: ControlReadOnly}}

	if err := plug.SetState(device.SwitchStateOff); !errors.Is(err, errReadOnly) {
		t.Errorf("a read-only switch: %v", err)
	}
}

func TestHandle_ASwitchToldOfTwiceIsKeptOnceWithTheLatestStub(t *testing.T) {
	handle := newDeviceHandle("garage1")

	var first, second device.ISwitch = &stubSwitch{}, &stubSwitch{}
	handle.switchRegistered("id1", "Plug", first)
	handle.switchRegistered("id1", "Plug", second)

	got, err := handle.find("")
	if err != nil || got != second {
		t.Errorf("got %v, %v", got, err)
	}
}

type stubSwitch struct{ device.ISwitch }
