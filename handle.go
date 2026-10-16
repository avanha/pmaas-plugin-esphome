package esphome

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/events"
)

// SwitchStubAvailableEvent is how the assembly is handed one of a device's switches, once the device has announced
// it. It's delivered on the server's goroutine, the one that called Run, and again if the switch is registered
// again, with a new stub, which replaces the old.
type SwitchStubAvailableEvent struct {
	events.EntityEvent

	// DeviceName is the device's ESPHome name.
	DeviceName string

	// EntityStub is the switch. Like any stub, it hops onto the plugin's goroutine to do whatever it's asked, so
	// don't call it from a function the plugin itself is running, and don't call it after the plugin has stopped.
	//
	// Turning the switch on or off through it skips the confirmation and the password: those are for a person at the
	// page. The device's ControlReadOnly is not skipped.
	EntityStub device.ISwitch
}

// DeviceHandle is the assembly's handle on a device it configured, for getting at the device's entities from the
// assembly, rather than from a plugin. It's what AddDevice and AddNativeDevice return. A device only has any
// entities once it has connected and announced them, so there's nothing to use until the stub is available:
//
//	kp4 := conf.AddNativeDevice(esphome.NativeDeviceConfig{Name: "kauf-plug-7981c8", Address: "10.47.6.74"})
//	kp4.AddOnSwitchStubAvailableListener(func(event esphome.SwitchStubAvailableEvent) { ... })
type DeviceHandle struct {
	name string

	mu        sync.Mutex
	listeners []func(event SwitchStubAvailableEvent)
	switches  []*handleSwitch
}

type handleSwitch struct {
	id   string
	name string
	stub device.ISwitch
}

var (
	errNoSwitch        = errors.New("the device has no such switch (yet)")
	errSeveralSwitches = errors.New("the device has several switches, so say which")
)

func newDeviceHandle(name string) *DeviceHandle {
	return &DeviceHandle{name: name}
}

// Name is the device's ESPHome name.
func (h *DeviceHandle) Name() string {
	return h.name
}

// AddOnSwitchStubAvailableListener adds a function to be called, on the server's goroutine, with each of the
// device's switches as it becomes available. A listener added after a switch has become available is not called
// for it; use Switch for those.
func (h *DeviceHandle) AddOnSwitchStubAvailableListener(listener func(event SwitchStubAvailableEvent)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.listeners = append(h.listeners, listener)
}

// Switch is the stub of one of the device's switches, by its name as shown on the page, or the device's only
// switch if name is empty. It fails if the device hasn't announced the switch yet, or at all.
func (h *DeviceHandle) Switch(name string) (device.ISwitch, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if name == "" {
		switch len(h.switches) {
		case 0:
			return nil, fmt.Errorf("%s: %w", h.name, errNoSwitch)
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

	return nil, fmt.Errorf("%s: %q: %w", h.name, name, errNoSwitch)
}

// switchRegistered is for the registry: a switch has been registered, and here is its stub. Called on the plugin
// goroutine. The listeners are handed to the server's goroutine, never run here, since they'll want to call the
// stub, which has to wait for this goroutine.
func (h *DeviceHandle) switchRegistered(container spi.IPMAASContainer, id string, name string, stub device.ISwitch) {
	h.mu.Lock()

	h.switches = slices.DeleteFunc(h.switches, func(s *handleSwitch) bool { return s.id == id })
	h.switches = append(h.switches, &handleSwitch{id: id, name: name, stub: stub})
	listeners := slices.Clone(h.listeners)

	h.mu.Unlock()

	if len(listeners) == 0 {
		return
	}

	event := SwitchStubAvailableEvent{
		EntityEvent: events.EntityEvent{Id: id, EntityType: device.ISwitchType, Name: name},
		DeviceName:  h.name,
		EntityStub:  stub,
	}

	invocations := make([]func(), len(listeners))
	for i, listener := range listeners {
		invocations[i] = func() { listener(event) }
	}

	if err := container.EnqueueOnServerGoRoutine(invocations); err != nil {
		fmt.Printf("esphome: unable to tell the assembly about %s's switch %q: %v\n", h.name, name, err)
	}
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
