package entities

import (
	"fmt"

	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/device"
)

// Switch is an ESPHome switch: a relay or the relay of a smart plug. Owned by the plugin goroutine.
type Switch struct {
	PmaasEntityId string
	Data          device.Switch

	// Command asks the device to go to a state that has already been checked as On or Off. It runs on the
	// plugin goroutine, where SetState is called, and returns once the request is queued for delivery.
	Command func(state device.SwitchState) error

	stub *switchStub
}

var _ device.ISwitch = (*Switch)(nil)

func (s *Switch) GetSwitchData() device.Switch {
	return s.Data
}

func (s *Switch) SetState(state device.SwitchState) error {
	if !state.IsSettable() {
		return fmt.Errorf("unable to set %q to %s: %w", s.Data.Name, state, device.ErrInvalidSwitchState)
	}

	if s.Command == nil {
		return fmt.Errorf("unable to set %q: it can't be controlled", s.Data.Name)
	}

	return s.Command(state)
}

// GetStub returns the thread-safe handle for this switch, creating it on first use. Must be called on the
// plugin goroutine, which is where the container invokes the stub factory.
func (s *Switch) GetStub(container spi.IPMAASContainer) device.ISwitch {
	if s.stub == nil {
		s.stub = &switchStub{link: newLink[device.ISwitch](container, s)}
	}

	return s.stub
}

// CloseStubIfPresent invalidates the stub handed out by GetStub, if any. Call after the entity has been
// deregistered.
func (s *Switch) CloseStubIfPresent() {
	if s.stub != nil {
		s.stub.link.close()
		s.stub = nil
	}
}

type switchStub struct {
	link *link[device.ISwitch]
}

func (s *switchStub) GetSwitchData() device.Switch {
	return call(s.link, func(target device.ISwitch) device.Switch {
		return target.GetSwitchData()
	})
}

func (s *switchStub) SetState(state device.SwitchState) error {
	return call(s.link, func(target device.ISwitch) error {
		return target.SetState(state)
	})
}
