package esphome

import (
	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
)

// getStatusAndEntities builds what the plugin's page shows: the status block, and below it a card for each device,
// with what the device has inside it. It runs on the plugin's own goroutine.
func (p *plugin) getStatusAndEntities() common.StatusAndEntities {
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
		ThermometerCount:  len(p.registry.thermometerViews()),
		DoorSensorCount:   len(p.registry.doorSensorViews()),
		SwitchCount:       len(p.registry.switchViews()),
		PowerMonitorCount: len(p.registry.powerViews()),
	}

	// The MQTT devices, then the native API ones, each in the order configured.
	var devices []*data.DeviceView

	for _, configured := range p.config.Devices {
		devices = append(devices, p.deviceViewOf(configured.Name, false))
	}

	nativeDevices, _ := p.config.validNativeDevices()

	for _, configured := range nativeDevices {
		devices = append(devices, p.deviceViewOf(configured.Name, true))
	}

	entities := make([]any, 0, len(devices))

	// Native devices are the only ones that can be Bluetooth proxies, and annotate is what says which are.
	for _, device := range devices {
		for _, child := range device.Children {
			if child.Kind() == data.KindBluetoothProxy {
				status.BluetoothProxyCount++
			}
		}
	}

	for _, device := range devices {
		status.DeviceCount++

		if device.Connected {
			status.ConnectedDevices++
		}

		if device.LastMessageTime.After(status.LastMessageTime) {
			status.LastMessageTime = device.LastMessageTime
		}

		entities = append(entities, device)
	}

	return common.StatusAndEntities{Status: status, Entities: entities}
}
