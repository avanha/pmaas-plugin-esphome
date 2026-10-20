package esphome

import (
	"fmt"
	"log/slog"
	"reflect"

	"github.com/avanha/pmaas-plugin-esphome/data"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/group"
)

// deviceEntityType is what a device is registered as. Nothing is done through it: the entity stands for the device, so that an
// assembly can name it (in a group, say) and a page of entities of many plugins can show its card, with what's inside it. It has no
// stub.
var deviceEntityType = reflect.TypeFor[*data.DeviceView]()

// deviceKey is what the views of a device and of the entities in it share, so that a page which has both draws the card once.
func deviceKey(name string) string {
	return "esphome/" + name
}

// deviceCategory is the kind of thing a device is, on a page that sorts by kind: a plug is a switch whatever else it has, and a
// device with only a sensor is that. The card is one kind, whichever of the entities in it is asked about.
func deviceCategory(view *data.DeviceView) string {
	kinds := map[string]bool{}

	for _, child := range view.Children {
		kinds[child.Kind()] = true
	}

	switch {
	case kinds[data.KindSwitch]:
		return group.CategorySwitches
	case kinds[data.KindDoorSensor]:
		return group.CategorySensors
	case kinds[data.KindThermometer]:
		return group.CategoryThermometers
	case kinds[data.KindPowerMonitor]:
		return group.CategoryPower
	default:
		return group.CategoryOther
	}
}

// deviceViewOf is the card of a configured device, as the plugin's own page shows it. Runs on the plugin goroutine.
func (p *plugin) deviceViewOf(name string, native bool) *data.DeviceView {
	view := p.registry.deviceView(name)

	if !native {
		view.Transport = "MQTT"

		return view
	}

	view.Transport = "Native API"

	if p.native != nil {
		view.Problem = p.native.problem(name)
	}

	p.proxies.annotate(view)

	return view
}

// entityViewOf is the view of a device, and so of the entities in it. Runs on the plugin goroutine.
func (p *plugin) entityViewOf(name string) spi.EntityView {
	view := p.deviceViewOf(name, p.nativeNames[name])

	return spi.EntityView{Key: deviceKey(name), Category: deviceCategory(view), Value: view}
}

// registerDevices registers an entity for each configured device, and gives its handle the entity's id. They are registered
// whether or not the device is connected, so an assembly can group a device that is down. The entities in a device are registered
// as they are announced, by the registry.
func (p *plugin) registerDevices(native []NativeDeviceConfig) {
	p.nativeNames = make(map[string]bool, len(native))

	for _, device := range native {
		p.nativeNames[device.Name] = true
	}

	var names []string

	for _, device := range p.config.Devices {
		names = append(names, device.Name)
	}

	for _, device := range native {
		names = append(names, device.Name)
	}

	for _, name := range names {
		id, err := p.container.RegisterEntity("device/"+name, deviceEntityType, p.registry.deviceLabel(name, ""), nil)
		if err != nil {
			p.log.Error("unable to register a device", "device", name, "error", err)

			continue
		}

		p.deviceEntities = append(p.deviceEntities, deviceEntity{name: name, id: id})

		if handle := p.config.handles[name]; handle != nil {
			handle.setEntityId(id)
		}

		if err := p.container.RegisterEntityView(id, func() (spi.EntityView, error) { return p.entityViewOf(name), nil }); err != nil {
			p.log.Error("unable to register the view of a device", "device", name, "error", err)
		}
	}
}

// deviceEntity is a device's entity.
type deviceEntity struct {
	name string
	id   string
}

// deregisterDevices takes the devices' entities back.
func (p *plugin) deregisterDevices() {
	for _, entity := range p.deviceEntities {
		if handle := p.config.handles[entity.name]; handle != nil {
			handle.setEntityId("")
		}

		if err := p.container.DeregisterEntity(entity.id); err != nil {
			p.log.Error("unable to deregister a device", "device", entity.name, "error", err)
		}
	}

	p.deviceEntities = nil
}

// giveView gives an entity of a device the view of the device's card. Called by the registry when it registers one.
func (r *registry) giveView(entityId string, deviceName string) {
	if r.entityView == nil {
		return
	}

	err := r.container.RegisterEntityView(entityId, func() (spi.EntityView, error) { return r.entityView(deviceName), nil })
	if err != nil {
		r.logf(slog.LevelError, "%s: %s", deviceName, fmt.Sprintf("unable to register the view of an entity: %v", err))
	}
}
