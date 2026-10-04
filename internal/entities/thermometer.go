package entities

import (
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/environment"
)

// Thermometer is an ESPHome temperature sensor, with the humidity sensor of the same device folded in
// when there is an unambiguous one. It's advertised as an environment.IWirelessThermometer, so the
// environment plugin re-hosts and renders it exactly like a bluetooth thermometer (RSSI and battery
// simply stay empty). Owned by the plugin goroutine.
type Thermometer struct {
	PmaasEntityId string
	Data          environment.WirelessThermometer

	stub *thermometerStub
}

var _ environment.IWirelessThermometer = (*Thermometer)(nil)

func (t *Thermometer) GetWirelessThermometerData() environment.WirelessThermometer {
	return t.Data
}

// GetStub returns the thread-safe handle for this thermometer, creating it on first use. Must be
// called on the plugin goroutine, which is where the container invokes the stub factory.
func (t *Thermometer) GetStub(container spi.IPMAASContainer) environment.IWirelessThermometer {
	if t.stub == nil {
		t.stub = &thermometerStub{link: newLink[environment.IWirelessThermometer](container, t)}
	}

	return t.stub
}

// CloseStubIfPresent invalidates the stub handed out by GetStub, if any. Call after the entity has
// been deregistered.
func (t *Thermometer) CloseStubIfPresent() {
	if t.stub != nil {
		t.stub.link.close()
		t.stub = nil
	}
}

type thermometerStub struct {
	link *link[environment.IWirelessThermometer]
}

func (s *thermometerStub) GetWirelessThermometerData() environment.WirelessThermometer {
	return call(s.link, func(target environment.IWirelessThermometer) environment.WirelessThermometer {
		return target.GetWirelessThermometerData()
	})
}
