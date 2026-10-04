package entities

import (
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/device"
)

// Door is an ESPHome binary sensor reporting a door's (or garage door's) position. Owned by the
// plugin goroutine.
type Door struct {
	PmaasEntityId string
	Data          device.DoorSensor

	stub *doorStub
}

var _ device.IDoorSensor = (*Door)(nil)

func (d *Door) GetDoorSensorData() device.DoorSensor {
	return d.Data
}

// GetStub returns the thread-safe handle for this door sensor, creating it on first use. Must be
// called on the plugin goroutine, which is where the container invokes the stub factory.
func (d *Door) GetStub(container spi.IPMAASContainer) device.IDoorSensor {
	if d.stub == nil {
		d.stub = &doorStub{link: newLink[device.IDoorSensor](container, d)}
	}

	return d.stub
}

// CloseStubIfPresent invalidates the stub handed out by GetStub, if any. Call after the entity has
// been deregistered.
func (d *Door) CloseStubIfPresent() {
	if d.stub != nil {
		d.stub.link.close()
		d.stub = nil
	}
}

type doorStub struct {
	link *link[device.IDoorSensor]
}

func (s *doorStub) GetDoorSensorData() device.DoorSensor {
	return call(s.link, func(target device.IDoorSensor) device.DoorSensor {
		return target.GetDoorSensorData()
	})
}
