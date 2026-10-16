package entities

import (
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/device"
)

// PowerMonitor is the electrical measurements of one ESPHome device: its power, voltage, current and energy
// sensors, whichever it has, gathered into one entity. Owned by the plugin goroutine.
type PowerMonitor struct {
	PmaasEntityId string
	Data          device.PowerMonitor

	stub *powerMonitorStub
}

var _ device.IPowerMonitor = (*PowerMonitor)(nil)

func (p *PowerMonitor) GetPowerMonitorData() device.PowerMonitor {
	return p.Data
}

// GetStub returns the thread-safe handle for this power monitor, creating it on first use. Must be called
// on the plugin goroutine, which is where the container invokes the stub factory.
func (p *PowerMonitor) GetStub(container spi.IPMAASContainer) device.IPowerMonitor {
	if p.stub == nil {
		p.stub = &powerMonitorStub{link: newLink[device.IPowerMonitor](container, p)}
	}

	return p.stub
}

// CloseStubIfPresent invalidates the stub handed out by GetStub, if any. Call after the entity has been
// deregistered.
func (p *PowerMonitor) CloseStubIfPresent() {
	if p.stub != nil {
		p.stub.link.close()
		p.stub = nil
	}
}

type powerMonitorStub struct {
	link *link[device.IPowerMonitor]
}

func (s *powerMonitorStub) GetPowerMonitorData() device.PowerMonitor {
	return call(s.link, func(target device.IPowerMonitor) device.PowerMonitor {
		return target.GetPowerMonitorData()
	})
}
