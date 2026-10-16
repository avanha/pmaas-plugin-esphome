package esphome

import (
	"sort"

	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
)

// getStatusAndEntities builds what the plugin's page shows. It runs on the plugin's own goroutine.
func (p *plugin) getStatusAndEntities() common.StatusAndEntities {
	thermometers := p.registry.thermometerViews()
	doors := p.registry.doorSensorViews()
	switches := p.registry.switchViews()
	powers := p.registry.powerViews()

	// Where the broker is listening once it has started, and until then where it's meant to.
	listenAddress := p.listenAddress
	if listenAddress == "" {
		listenAddress = p.config.ListenAddress
	}

	status := data.PluginStatus{
		BrokerRunning:     p.broker != nil,
		BrokerProblem:     p.brokerProblem,
		BrokerNotUsed:     p.brokerNotUsed,
		ListenAddress:     listenAddress,
		DiscoveryPrefix:   p.config.DiscoveryPrefix,
		CaptureMessages:   p.config.CaptureMessages,
		ThermometerCount:  len(thermometers),
		DoorSensorCount:   len(doors),
		SwitchCount:       len(switches),
		PowerMonitorCount: len(powers),
	}

	// The MQTT devices, then the native API ones, each in the order configured.
	var devices []data.DeviceStatus

	for _, configured := range p.config.Devices {
		device := p.registry.deviceStatus(configured.Name)
		device.Transport = "MQTT"
		devices = append(devices, device)
	}

	nativeDevices, _ := p.config.validNativeDevices()

	for _, configured := range nativeDevices {
		device := p.registry.deviceStatus(configured.Name)
		device.Transport = "Native API"

		if p.native != nil {
			device.Problem = p.native.problem(configured.Name)
		}

		devices = append(devices, device)
	}

	status.Devices = make([]data.DeviceStatus, 0, len(devices))

	for _, device := range devices {
		status.Devices = append(status.Devices, device)

		if device.Connected {
			status.ConnectedDevices++
		}

		if device.LastMessageTime.After(status.LastMessageTime) {
			status.LastMessageTime = device.LastMessageTime
		}
	}

	return common.StatusAndEntities{Status: status, Entities: orderEntities(thermometers, doors, switches, powers)}
}

// orderEntities puts the entities in one list, grouped by device and then by kind (thermometers, door sensors,
// switches, power), and by name within a kind. A plug's switch and its power reading therefore sit together.
func orderEntities(
	thermometers []data.ThermometerView,
	doors []data.DoorSensorView,
	switches []data.SwitchView,
	powers []data.PowerView) []any {
	type entry struct {
		device string
		kind   int
		name   string
		value  any
	}

	entries := make([]entry, 0, len(thermometers)+len(doors)+len(switches)+len(powers))

	for i := range thermometers {
		entries = append(entries, entry{thermometers[i].DeviceName, 0, thermometers[i].Name, &thermometers[i]})
	}

	for i := range doors {
		entries = append(entries, entry{doors[i].DeviceName, 1, doors[i].Name, &doors[i]})
	}

	for i := range switches {
		entries = append(entries, entry{switches[i].DeviceName, 2, switches[i].Name, &switches[i]})
	}

	for i := range powers {
		entries = append(entries, entry{powers[i].DeviceName, 3, powers[i].Name, &powers[i]})
	}

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]

		switch {
		case a.device != b.device:
			return a.device < b.device
		case a.kind != b.kind:
			return a.kind < b.kind
		default:
			return a.name < b.name
		}
	})

	result := make([]any, len(entries))

	for i, e := range entries {
		result[i] = e.value
	}

	return result
}
