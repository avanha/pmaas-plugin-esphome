package esphome

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
)

// macPattern matches the identifier ESPHome gives a device, which is its MAC address as twelve hex digits.
var macPattern = regexp.MustCompile(`^[0-9a-fA-F]{12}$`)

// formatMAC returns identifier as a MAC address ("20:50:0d:e4:64:ec") if that's what it is, and "" if not:
// some other kind of device might use something else as its identifier, which isn't worth showing as one.
func formatMAC(identifier string) string {
	if !macPattern.MatchString(identifier) {
		return ""
	}

	identifier = strings.ToLower(identifier)
	parts := make([]string, 0, 6)

	for i := 0; i < len(identifier); i += 2 {
		parts = append(parts, identifier[i:i+2])
	}

	return strings.Join(parts, ":")
}

// deviceStatus describes the configured device called name. A device that has never connected has no node
// yet, and shows as such.
func (r *registry) deviceStatus(name string) data.DeviceStatus {
	status := data.DeviceStatus{Name: name, Label: r.deviceLabel(name, "")}

	n, ok := r.nodes[name]
	if !ok {
		return status
	}

	status.Label = r.deviceLabel(name, n.info.Name)
	status.Connected = n.connected
	status.Address = n.address
	status.LastMessageTime = n.lastMessage
	status.Model = n.info.Model
	status.Manufacturer = n.info.Manufacturer
	status.Firmware = n.info.SWVersion

	if len(n.info.Identifiers) > 0 {
		status.MAC = formatMAC(n.info.Identifiers[0])
	}

	if n.connected {
		status.ConnectedSince = n.onlineSince
	}

	// DisconnectedSince is only worth showing for a device that's disconnected: for a connected one it's the
	// end of an earlier session.
	if !n.connected {
		status.DisconnectedSince = n.offlineSince
	}

	if n.chip != nil {
		status.ChipTemperature = &data.ChipTemperature{HasData: n.chip.hasValue, Celsius: n.chip.value, LastUpdate: n.chip.lastUpdate}
	}

	for _, ch := range n.temps {
		if ch.thermometer != nil {
			status.EntityCount++
		}
	}

	for _, ch := range n.doors {
		if ch.door != nil {
			status.EntityCount++
		}
	}

	for _, ch := range n.switches {
		if ch.sw != nil {
			status.EntityCount++
		}
	}

	if n.power != nil {
		status.EntityCount++
	}

	return status
}

// shortName is what an entity is called on its own, without the device it's on, for where the device is already on
// show: the card of a device, which has its children inside it. It's worked out when it's wanted, from the same
// things the full name is, so that there's nothing to keep up to date when the device renames something.
//
// It's the name the device gave the entity, without the device's name in front if it had that too. An entity that
// has no name of its own, because it's the device's main entity (a plug's relay is named for the plug, or for the
// plug without the end of its MAC address) is called by what it is, which is fallback.
func (r *registry) shortName(deviceName string, e hadiscovery.Entity, fallback string) string {
	label := r.deviceLabel(deviceName, e.Device.Name)

	itself, own := splitEntityName(e.Name, label, e.Device.Name)
	if itself {
		return fallback
	}

	return own
}

// The views of what a device has, in no particular order. The ones that follow put them all in order, for the
// whole plugin, and deviceView puts one device's in order, as the page shows them.

func (r *registry) thermometerViewsOf(n *node) []data.ThermometerView {
	var views []data.ThermometerView

	for objectID, ch := range n.temps {
		if ch.thermometer == nil {
			continue
		}

		sensor := ch.thermometer.Data.SensorData

		views = append(views, data.ThermometerView{
			Name:            ch.thermometer.Data.Name,
			ShortName:       r.shortName(n.name, ch.disc, "Temperature"),
			DeviceName:      n.name,
			ObjectID:        objectID,
			DeviceConnected: n.connected,
			HasData:         sensor.HasData,
			Temperature:     sensor.Temperature,
			HasHumidity:     sensor.HasHumidity,
			Humidity:        sensor.Humidity,
			LastUpdateTime:  sensor.LastUpdateTime,
		})
	}

	return views
}

func (r *registry) doorSensorViewsOf(n *node) []data.DoorSensorView {
	var views []data.DoorSensorView

	for objectID, ch := range n.doors {
		if ch.door == nil {
			continue
		}

		door := ch.door.Data

		views = append(views, data.DoorSensorView{
			Name:            door.Name,
			ShortName:       r.shortName(n.name, ch.disc, "Door"),
			DeviceName:      n.name,
			ObjectID:        objectID,
			DeviceConnected: door.Connectivity == environment.ConnectivityOnline,
			HasData:         door.HasData,
			Position:        door.Position.String(),
			PositionClass:   strings.ToLower(door.Position.String()),
			LastChangeTime:  door.LastChangeTime,
			LastUpdateTime:  door.LastUpdateTime,
			OfflineSince:    door.OfflineSince,
			OnlineSince:     door.OnlineSince,
		})
	}

	return views
}

func (r *registry) switchViewsOf(n *node) []data.SwitchView {
	var views []data.SwitchView

	for objectID, ch := range n.switches {
		if ch.sw == nil {
			continue
		}

		sw := ch.sw.Data
		connected := sw.Connectivity == environment.ConnectivityOnline

		views = append(views, data.SwitchView{
			Name:            sw.Name,
			ShortName:       r.shortName(n.name, ch.disc, "Switch"),
			DeviceName:      n.name,
			ObjectID:        objectID,
			DeviceConnected: connected,
			Control:         r.controlOf(n.name).Level.String(),
			CanControl:      connected && ch.commandable() && r.controlOf(n.name).Level != ControlReadOnly,
			HasData:         sw.HasData,
			State:           sw.State.String(),
			StateClass:      strings.ToLower(sw.State.String()),
			LastChangeTime:  sw.LastChangeTime,
			LastUpdateTime:  sw.LastUpdateTime,
			OfflineSince:    sw.OfflineSince,
			OnlineSince:     sw.OnlineSince,
		})
	}

	return views
}

func (r *registry) powerViewsOf(n *node) []data.PowerView {
	if n.power == nil {
		return nil
	}

	p := n.power.Data

	return []data.PowerView{{
		Name:            p.Name,
		ShortName:       "Power",
		DeviceName:      n.name,
		DeviceConnected: p.Connectivity == environment.ConnectivityOnline,
		HasData:         p.HasData,
		HasPower:        p.HasPower,
		Power:           p.Power,
		HasVoltage:      p.HasVoltage,
		Voltage:         p.Voltage,
		HasCurrent:      p.HasCurrent,
		Current:         p.Current,
		HasEnergy:       p.HasEnergy,
		Energy:          p.Energy,
		LastUpdateTime:  p.LastUpdateTime,
		OfflineSince:    p.OfflineSince,
		OnlineSince:     p.OnlineSince,
	}}
}

// deviceView describes the configured device called name and everything of its that's tracked, with the children in
// order. A device that has never connected has none, and shows as just its status.
func (r *registry) deviceView(name string) *data.DeviceView {
	view := &data.DeviceView{DeviceStatus: r.deviceStatus(name)}

	n, ok := r.nodes[name]
	if !ok {
		return view
	}

	for _, v := range r.switchViewsOf(n) {
		view.Children = append(view.Children, v)
	}

	for _, v := range r.powerViewsOf(n) {
		view.Children = append(view.Children, v)
	}

	for _, v := range r.doorSensorViewsOf(n) {
		view.Children = append(view.Children, v)
	}

	for _, v := range r.thermometerViewsOf(n) {
		view.Children = append(view.Children, v)
	}

	sortChildren(view.Children)

	return view
}

// The whole plugin's views of each kind, ordered by device and then name.

func (r *registry) thermometerViews() []data.ThermometerView {
	var views []data.ThermometerView

	for _, n := range r.nodes {
		views = append(views, r.thermometerViewsOf(n)...)
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].DeviceName != views[j].DeviceName {
			return views[i].DeviceName < views[j].DeviceName
		}

		return views[i].SortKey() < views[j].SortKey()
	})

	return views
}

func (r *registry) doorSensorViews() []data.DoorSensorView {
	var views []data.DoorSensorView

	for _, n := range r.nodes {
		views = append(views, r.doorSensorViewsOf(n)...)
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].DeviceName != views[j].DeviceName {
			return views[i].DeviceName < views[j].DeviceName
		}

		return views[i].SortKey() < views[j].SortKey()
	})

	return views
}

func (r *registry) switchViews() []data.SwitchView {
	var views []data.SwitchView

	for _, n := range r.nodes {
		views = append(views, r.switchViewsOf(n)...)
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].DeviceName != views[j].DeviceName {
			return views[i].DeviceName < views[j].DeviceName
		}

		return views[i].SortKey() < views[j].SortKey()
	})

	return views
}

func (r *registry) powerViews() []data.PowerView {
	var views []data.PowerView

	for _, n := range r.nodes {
		views = append(views, r.powerViewsOf(n)...)
	}

	sort.Slice(views, func(i, j int) bool { return views[i].DeviceName < views[j].DeviceName })

	return views
}

// setSwitch asks the switch the device calls objectID to go to state, for a person at the page, who has to have done
// what guards the switch. Runs on the plugin goroutine.
func (r *registry) setSwitch(deviceName string, objectID string, state device.SwitchState, confirmation common.Confirmation) error {
	n, ok := r.nodes[deviceName]
	if !ok {
		return fmt.Errorf("there's no device called %q", deviceName)
	}

	ch, ok := n.switches[objectID]
	if !ok || ch.sw == nil {
		return fmt.Errorf("%s has no switch called %q", deviceName, objectID)
	}

	if err := r.authorizeFromPage(deviceName, ch.sw.Data.Name, confirmation); err != nil {
		return err
	}

	return ch.sw.SetState(state)
}
