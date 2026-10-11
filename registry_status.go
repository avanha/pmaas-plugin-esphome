package esphome

import (
	"regexp"
	"sort"
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/data"
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

	return status
}

// thermometerViews and doorSensorViews describe every entity currently registered, ordered by device and then
// name.
func (r *registry) thermometerViews() []data.ThermometerView {
	var views []data.ThermometerView

	for _, n := range r.nodes {
		for _, ch := range n.temps {
			if ch.thermometer == nil {
				continue
			}

			sensor := ch.thermometer.Data.SensorData

			views = append(views, data.ThermometerView{
				Name:            ch.thermometer.Data.Name,
				DeviceName:      n.name,
				DeviceConnected: n.connected,
				HasData:         sensor.HasData,
				Temperature:     sensor.Temperature,
				HasHumidity:     sensor.HasHumidity,
				Humidity:        sensor.Humidity,
				LastUpdateTime:  sensor.LastUpdateTime,
			})
		}
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].DeviceName != views[j].DeviceName {
			return views[i].DeviceName < views[j].DeviceName
		}

		return views[i].Name < views[j].Name
	})

	return views
}

func (r *registry) doorSensorViews() []data.DoorSensorView {
	var views []data.DoorSensorView

	for _, n := range r.nodes {
		for _, ch := range n.doors {
			if ch.door == nil {
				continue
			}

			door := ch.door.Data

			views = append(views, data.DoorSensorView{
				Name:            door.Name,
				DeviceName:      n.name,
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
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].DeviceName != views[j].DeviceName {
			return views[i].DeviceName < views[j].DeviceName
		}

		return views[i].Name < views[j].Name
	})

	return views
}
