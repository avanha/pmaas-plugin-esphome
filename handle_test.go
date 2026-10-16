package esphome

import (
	"errors"
	"testing"

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

func TestHandle_ListenersAreHandedTheSwitchOnTheServersGoroutineOnceItIsRegistered(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")

	var heard []SwitchStubAvailableEvent
	handle.AddOnSwitchStubAvailableListener(func(event SwitchStubAvailableEvent) { heard = append(heard, event) })

	if len(f.container.serverBatches) != 0 {
		t.Fatal("the server was asked to run something before there was a switch")
	}

	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	if len(f.container.serverBatches) != 1 || len(f.container.serverBatches[0]) != 1 || len(heard) != 0 {
		t.Fatalf("batches %d, heard %d: the listeners must be handed over, not run on the plugin's goroutine",
			len(f.container.serverBatches), len(heard))
	}

	f.container.serverBatches[0][0]()

	if len(heard) != 1 {
		t.Fatalf("heard %d", len(heard))
	}

	event := heard[0]
	if event.DeviceName != "garage1" || event.Name != "Garage 1 Plug" || event.EntityType != device.ISwitchType ||
		event.Id != f.container.registrations[0].id || event.EntityStub == nil {
		t.Fatalf("event: %+v", event)
	}

	// It's the same switch the container knows.
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.state("garage1", "garage1/switch/plug/state", "ON")

	if got := event.EntityStub.GetSwitchData(); got.State != device.SwitchStateOn {
		t.Errorf("the stub in the event is for another switch: %+v", got)
	}
}

func TestHandle_NobodyListeningMeansNothingForTheServerToRun(t *testing.T) {
	f := newFixture(t)
	f.withHandle("garage1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	if len(f.container.serverBatches) != 0 {
		t.Errorf("the server was asked to run %d batches with no listeners", len(f.container.serverBatches))
	}
}

func TestHandle_ADeviceWithoutAHandleIsNotAProblem(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.discover("other", "switch", "plug", switchConfig("other", "plug", "Plug"))

	handle := f.withHandle("garage1")
	f.discover("other", "switch", "plug2", switchConfig("other", "plug2", "Plug 2"))

	if _, err := handle.Switch(""); !errors.Is(err, errNoSwitch) {
		t.Errorf("another device's switch reached the handle: %v", err)
	}
}

func TestHandle_SwitchByNameOrTheOnlyOne(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")

	if _, err := handle.Switch(""); !errors.Is(err, errNoSwitch) {
		t.Errorf("no switches yet: %v", err)
	}

	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	only, err := handle.Switch("")
	if err != nil || only == nil {
		t.Fatalf("the only switch: %v %v", only, err)
	}

	byName, err := handle.Switch("Garage 1 Plug")
	if err != nil || byName != only {
		t.Errorf("by name: %v %v", byName, err)
	}

	if _, err := handle.Switch("Nothing"); !errors.Is(err, errNoSwitch) {
		t.Errorf("an unknown name: %v", err)
	}

	f.discover("garage1", "switch", "fan", switchConfig("garage1", "fan", "Fan"))

	if _, err := handle.Switch(""); !errors.Is(err, errSeveralSwitches) {
		t.Errorf("two switches and no name: %v", err)
	}

	if fan, err := handle.Switch("Garage 1 Fan"); err != nil || fan == only {
		t.Errorf("the fan: %v %v", fan, err)
	}
}

func TestHandle_ARegisteredAgainSwitchReplacesItsStub(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	first, _ := handle.Switch("")

	f.registry.HandleMessage("garage1", "homeassistant/switch/garage1/plug/config", nil) // the device removes it

	if _, err := handle.Switch(""); !errors.Is(err, errNoSwitch) {
		t.Fatalf("a removed switch is still there: %v", err)
	}

	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	second, err := handle.Switch("")

	if err != nil || second == first {
		t.Errorf("after being registered again: %v, same stub as before: %v", err, second == first)
	}

	if len(handle.switches) != 1 {
		t.Errorf("%d switches", len(handle.switches))
	}
}

func TestHandle_ARenamedSwitchIsFoundByItsNewName(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Heater"))

	if _, err := handle.Switch("Garage 1 Heater"); err != nil {
		t.Errorf("by its new name: %v", err)
	}

	if _, err := handle.Switch("Garage 1 Plug"); !errors.Is(err, errNoSwitch) {
		t.Errorf("by its old name: %v", err)
	}
}

func TestHandle_ShuttingDownTakesTheStubsBackSoNobodyCallsOneThatWouldPanic(t *testing.T) {
	f := newFixture(t)
	handle := f.withHandle("garage1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	f.registry.Shutdown()

	if _, err := handle.Switch(""); !errors.Is(err, errNoSwitch) {
		t.Errorf("after shutdown: %v", err)
	}
}

func TestHandle_AStubFromTheHandleHonoursReadOnlyAndSkipsThePasswordPrompt(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	f.registry.control = map[string]SwitchControl{"garage1": {Level: ControlPassword, Password: "secret"}}
	handle := f.withHandle("garage1")
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	stub, _ := handle.Switch("")

	if err := stub.SetState(device.SwitchStateOn); err != nil || len(*sent) != 1 {
		t.Fatalf("a password-guarded switch turned on by the assembly: %v, published %v", err, *sent)
	}

	f.registry.control = map[string]SwitchControl{"garage1": {Level: ControlReadOnly}}

	if err := stub.SetState(device.SwitchStateOff); !errors.Is(err, errReadOnly) {
		t.Errorf("a read-only switch: %v", err)
	}
}

func TestHandle_ASwitchToldOfTwiceIsKeptOnceWithTheLatestStub(t *testing.T) {
	f := newFixture(t)
	handle := newDeviceHandle("garage1")

	var first, second device.ISwitch = &stubSwitch{}, &stubSwitch{}
	handle.switchRegistered(f.container, "id1", "Plug", first)
	handle.switchRegistered(f.container, "id1", "Plug", second)

	got, err := handle.Switch("")
	if err != nil || got != second {
		t.Errorf("got %v, %v", got, err)
	}
}

type stubSwitch struct{ device.ISwitch }
