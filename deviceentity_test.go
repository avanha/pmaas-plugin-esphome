package esphome

import (
	"testing"

	"github.com/avanha/pmaas-plugin-esphome/data"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/group"
)

func TestDeviceEntity_IsRegisteredForAConfiguredDeviceAndItsHandleNamesIt(t *testing.T) {
	config := NewPluginConfig()
	handle := config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7", DisplayName: "Heater"})

	if _, ok := handle.RegisteredId(); ok {
		t.Fatal("the handle has an entity id before the plugin has started")
	}

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aKaufPlugDial(conns))
	nextConn(t, conns)

	eventually(t, c, "the device's entity", func() bool { return len(c.registrations) >= 1 })

	var found *registration

	c.onPluginGoroutine(func() {
		for i := range c.registrations {
			if c.registrations[i].uniqueData == "device/plug1" {
				found = &c.registrations[i]
			}
		}
	})

	if found == nil || found.entityType != deviceEntityType || found.name != "Heater" || found.stubFactory != nil {
		t.Fatalf("%+v", found)
	}

	if id, ok := handle.RegisteredId(); !ok || id != found.id {
		t.Errorf("%q %v, want %q", id, ok, found.id)
	}

	stopPlugin(t, p, c)

	if _, ok := handle.RegisteredId(); ok {
		t.Error("the handle still has an entity id after the plugin stopped")
	}
}

func TestDeviceEntity_TheDevicesViewAndThoseOfItsEntitiesAreOneCardOfOneKind(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7", DisplayName: "Heater"})

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aKaufPlugDial(conns))

	defer stopPlugin(t, p, c)

	nextConn(t, conns)
	eventually(t, c, "the switch and its power", func() bool {
		return len(c.registrations) == 3 && p.registry.switchViews()[0].HasData && p.registry.powerViews()[0].HasPower
	})

	views := map[string]spi.EntityView{}

	c.onPluginGoroutine(func() {
		for _, r := range c.registrations {
			fn := c.views[r.id]
			if fn == nil {
				t.Errorf("%s has no view", r.id)

				continue
			}

			view, err := fn()
			if err != nil {
				t.Errorf("%s: %v", r.id, err)
			}

			views[r.uniqueData] = view
		}
	})

	if len(views) != 3 {
		t.Fatalf("%v", views)
	}

	for name, view := range views {
		card, ok := view.Value.(*data.DeviceView)
		if !ok || card.Name != "plug1" || card.Transport != "Native API" || len(card.Children) != 2 {
			t.Errorf("%s: %#v", name, view.Value)
		}

		if view.Key != "esphome/plug1" || view.Category != group.CategorySwitches {
			t.Errorf("%s: key %q category %q", name, view.Key, view.Category)
		}
	}
}

func TestDeviceCategory_IsWhatTheDeviceDoesMostToBeControlledOrWatched(t *testing.T) {
	child := func(kinds ...string) *data.DeviceView {
		view := &data.DeviceView{}

		for _, kind := range kinds {
			switch kind {
			case data.KindSwitch:
				view.Children = append(view.Children, data.SwitchView{})
			case data.KindPowerMonitor:
				view.Children = append(view.Children, data.PowerView{})
			case data.KindDoorSensor:
				view.Children = append(view.Children, data.DoorSensorView{})
			case data.KindThermometer:
				view.Children = append(view.Children, data.ThermometerView{})
			}
		}

		return view
	}

	for want, view := range map[string]*data.DeviceView{
		group.CategorySwitches:     child(data.KindPowerMonitor, data.KindSwitch), // a plug is a switch
		group.CategorySensors:      child(data.KindThermometer, data.KindDoorSensor),
		group.CategoryThermometers: child(data.KindThermometer),
		group.CategoryPower:        child(data.KindPowerMonitor),
		group.CategoryOther:        child(),
	} {
		if got := deviceCategory(view); got != want {
			t.Errorf("want %s, got %s", want, got)
		}
	}
}
