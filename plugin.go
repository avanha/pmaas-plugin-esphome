package esphome

import (
	"fmt"
	"net"

	"github.com/avanha/pmaas-plugin-esphome/internal/broker"
	spi "github.com/avanha/pmaas-spi"
)

type Plugin interface {
	spi.IPMAASPlugin
}

type plugin struct {
	config    PluginConfig
	container spi.IPMAASContainer
	registry  *registry
	broker    *broker.Broker
}

// Force implementation of spi.IPMAASPlugin
var _ spi.IPMAASPlugin = (*plugin)(nil)

func NewPlugin(config PluginConfig) Plugin {
	defaults := NewPluginConfig()

	if config.ListenAddress == "" {
		config.ListenAddress = defaults.ListenAddress
	}

	if config.DiscoveryPrefix == "" {
		config.DiscoveryPrefix = defaults.DiscoveryPrefix
	}

	return &plugin{config: config}
}

func (p *plugin) ShortName() string {
	return "esphome"
}

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.container = container

	displayNames := make(map[string]string)

	for _, device := range p.config.Devices {
		if device.DisplayName != "" {
			displayNames[device.Name] = device.DisplayName
		}
	}

	p.registry = newRegistry(container, p.config.DiscoveryPrefix, displayNames)
}

func (p *plugin) Start() {
	fmt.Printf("%T Starting...\n", *p)

	if len(p.config.Devices) == 0 {
		fmt.Printf("%T WARNING: no devices configured, not starting the MQTT broker\n", *p)
		return
	}

	devices := make(map[string]string, len(p.config.Devices))

	for _, device := range p.config.Devices {
		if _, duplicate := devices[device.Name]; duplicate {
			fmt.Printf("%T ERROR: device %q is configured more than once, not starting the MQTT broker\n", *p, device.Name)
			return
		}

		devices[device.Name] = device.Password
	}

	// Bind here, rather than leaving it to the broker, so that a port that's in use is reported
	// to the one place that can say so clearly.
	listener, err := net.Listen("tcp", p.config.ListenAddress)
	if err != nil {
		fmt.Printf("%T ERROR: unable to listen on %s, not starting the MQTT broker: %v\n", *p, p.config.ListenAddress, err)
		return
	}

	b, err := broker.New(broker.Config{
		Listener:        listener,
		Devices:         devices,
		DiscoveryPrefix: p.config.DiscoveryPrefix,
	}, p.brokerCallbacks())
	if err != nil {
		_ = listener.Close()
		fmt.Printf("%T ERROR: unable to start the MQTT broker: %v\n", *p, err)

		return
	}

	p.broker = b

	fmt.Printf("%T MQTT broker listening on %s for %d device(s)\n", *p, listener.Addr(), len(devices))
}

// brokerCallbacks hands everything the broker reports over to the plugin goroutine. The callbacks
// run on the broker's goroutines and wait for the plugin goroutine to take each call, which keeps a
// device's messages in the order it sent them, and makes a slow plugin slow the devices down rather
// than queueing without bound.
func (p *plugin) brokerCallbacks() broker.Callbacks {
	return broker.Callbacks{
		OnMessage: func(device string, topic string, payload []byte, retain bool) {
			if p.config.CaptureMessages {
				fmt.Printf("esphome capture: device=%s retain=%v topic=%s payload=%s\n",
					device, retain, topic, truncate(payload, 2048))
			}

			p.enqueue(func() { p.registry.HandleMessage(device, topic, payload) })
		},
		OnDeviceConnected: func(device string) {
			p.enqueue(func() { p.registry.HandleConnected(device) })
		},
		OnDeviceDisconnected: func(device string) {
			p.enqueue(func() { p.registry.HandleDisconnected(device) })
		},
	}
}

func (p *plugin) enqueue(f func()) {
	if err := p.container.EnqueueOnPluginGoRoutine(f); err != nil {
		// The plugin is stopping; there's nothing left to tell.
		fmt.Printf("%T dropping broker event: %v\n", *p, err)
	}
}

// Stop runs on the plugin goroutine, so it must not wait for the broker: closing it waits for its
// connections to finish, and one of them may be waiting for this goroutine to take a message.
// Entities are deregistered here, and the broker is closed on a background goroutine; the returned
// channel closes when that's done, and the plugin goroutine is free in the meantime.
func (p *plugin) Stop() chan func() {
	fmt.Printf("%T Stopping...\n", *p)

	p.registry.Shutdown()

	b := p.broker
	p.broker = nil

	if b == nil {
		return p.container.ClosedCallbackChannel()
	}

	done := make(chan func())

	go func() {
		defer close(done)

		if err := b.Close(); err != nil {
			fmt.Printf("%T error closing the MQTT broker: %v\n", *p, err)
		}
	}()

	return done
}

func truncate(payload []byte, max int) string {
	if len(payload) <= max {
		return string(payload)
	}

	return string(payload[:max]) + "...(truncated)"
}
