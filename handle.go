package esphome

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/avanha/pmaas-spi/common"
	"github.com/avanha/pmaas-spi/device"
)

var (
	errNoSwitch        = errors.New("the device has no such switch")
	errSeveralSwitches = errors.New("the device has several switches, so say which")
)

// DeviceHandle is the assembly's handle on a device it configured, for using the device's entities from the
// assembly. It's what AddDevice and AddNativeDevice return, and it's usable at once: a device only has entities once
// it has connected and announced them, so what's taken from the handle doesn't need the device to be there yet. A
// call made before then, or while the device is gone, fails; it doesn't wait.
//
//	kp4 := conf.AddNativeDevice(esphome.NativeDeviceConfig{Name: "kauf-plug-7981c8", Address: "10.47.6.74"})
//	plug := kp4.Switch("") // a device.ISwitch, to give to whatever turns it on and off
type DeviceHandle struct {
	name string

	mu       sync.Mutex
	switches []*handleSwitch
}

type handleSwitch struct {
	id   string
	name string
	stub device.ISwitch
}

func newDeviceHandle(name string) *DeviceHandle {
	return &DeviceHandle{name: name}
}

// Name is the device's ESPHome name.
func (h *DeviceHandle) Name() string {
	return h.name
}

// Switch is one of the device's switches, by its name as shown on the page, or the device's only switch if name is
// empty. Which one is worked out at each call, from what the device has announced by then.
//
// Turning it on or off skips the confirmation and the password: those are for a person at the page. The device's
// ControlReadOnly is not skipped.
func (h *DeviceHandle) Switch(name string) device.ISwitch {
	return &switchProxy{handle: h, name: name}
}

// find is the stub of the switch name stands for, or an error saying why there isn't one.
func (h *DeviceHandle) find(name string) (device.ISwitch, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if name == "" {
		switch len(h.switches) {
		case 0:
			return nil, fmt.Errorf("%s: %w: %w", h.name, errNoSwitch, common.ErrStubNotAvailable)
		case 1:
			return h.switches[0].stub, nil
		default:
			return nil, fmt.Errorf("%s: %w", h.name, errSeveralSwitches)
		}
	}

	for _, candidate := range h.switches {
		if candidate.name == name {
			return candidate.stub, nil
		}
	}

	return nil, fmt.Errorf("%s: %q: %w: %w", h.name, name, errNoSwitch, common.ErrStubNotAvailable)
}

// switchRegistered is for the registry: a switch has been registered, and here is its stub. Called on the plugin
// goroutine.
func (h *DeviceHandle) switchRegistered(id string, name string, stub device.ISwitch) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.switches = slices.DeleteFunc(h.switches, func(s *handleSwitch) bool { return s.id == id })
	h.switches = append(h.switches, &handleSwitch{id: id, name: name, stub: stub})
}

// switchRenamed is for the registry: the switch has a new name.
func (h *DeviceHandle) switchRenamed(id string, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, s := range h.switches {
		if s.id == id {
			s.name = name
		}
	}
}

// switchDeregistered is for the registry: the switch is gone, and its stub mustn't be used again.
func (h *DeviceHandle) switchDeregistered(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.switches = slices.DeleteFunc(h.switches, func(s *handleSwitch) bool { return s.id == id })
}

// switchProxy is a device.ISwitch for a switch that may not exist yet. It forwards to the switch's stub, looked up
// at each call, so it keeps working across the device connecting, disconnecting and being registered again.
type switchProxy struct {
	handle *DeviceHandle
	name   string
}

var _ device.ISwitch = (*switchProxy)(nil)

// GetSwitchData is the switch's data, or, when there is no switch to ask, a Switch that has not reported anything,
// which is how a switch that is not there yet looks.
func (p *switchProxy) GetSwitchData() device.Switch {
	stub, err := p.handle.find(p.name)
	if err != nil {
		return device.Switch{Name: p.name}
	}

	return stub.GetSwitchData()
}

func (p *switchProxy) SetState(state device.SwitchState) error {
	stub, err := p.handle.find(p.name)
	if err != nil {
		return fmt.Errorf("unable to set %s's switch %q to %s: %w", p.handle.name, p.name, state, err)
	}

	return stub.SetState(state)
}
