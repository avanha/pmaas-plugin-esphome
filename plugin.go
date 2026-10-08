package esphome

import (
	"fmt"
	"log/slog"
	"net"
	"os"

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
	return &plugin{config: config.withDefaults()}
}

func (p *plugin) ShortName() string {
	return "esphome"
}

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.container = container
	p.registry = newRegistry(container, p.config.DiscoveryPrefix, p.config.displayNames())
}

func (p *plugin) Start() {
	fmt.Printf("%T Starting...\n", *p)

	if len(p.config.Devices) == 0 {
		fmt.Printf("%T WARNING: no devices configured, not starting the MQTT broker\n", *p)
		return
	}

	mqttBroker, address, err := p.startBroker()
	if err != nil {
		fmt.Printf("%T ERROR: not starting the MQTT broker: %v\n", *p, err)
		return
	}

	p.broker = mqttBroker

	fmt.Printf("%T MQTT broker listening on %s for %d device(s)\n", *p, address, len(p.config.Devices))
}

// startBroker binds the configured address and starts the broker on it. If it fails, nothing is left
// listening.
func (p *plugin) startBroker() (*broker.Broker, net.Addr, error) {
	credentials, err := p.config.deviceCredentials()
	if err != nil {
		return nil, nil, err
	}

	// Bind here, rather than leaving it to the broker, so that a port that's in use is reported
	// to the one place that can say so clearly.
	listener, err := net.Listen("tcp", p.config.ListenAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to listen on %s: %w", p.config.ListenAddress, err)
	}

	mqttBroker, err := broker.New(broker.Config{
		Listener:        listener,
		Devices:         credentials,
		DiscoveryPrefix: p.config.DiscoveryPrefix,
		Logger:          brokerLogger(),
	}, p.brokerCallbacks())
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}

	return mqttBroker, listener.Addr(), nil
}

// brokerLogger is where the broker reports what it refuses: a device that fails to authenticate, or touches
// a topic it may not. Those are the things an operator needs to see, since a device that's refused just
// keeps reconnecting and says nothing useful. Warnings and errors only, since the broker's own debug and
// info output is noisy.
func brokerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:       slog.LevelWarn,
		ReplaceAttr: describeUnnamedBrokerWarnings,
	}))
}

// describeUnnamedBrokerWarnings gives the broker's one warning that has no message some words. It logs, with an
// empty message, a connection that ended with an error. An ESP32 that reboots or loses its WiFi ends that
// way, with "error=EOF", and a bare "msg=\"\"" tells whoever's reading nothing about it.
func describeUnnamedBrokerWarnings(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.MessageKey && a.Value.String() == "" {
		a.Value = slog.StringValue("a device's connection ended with an error (a device that rebooted or lost its network ends this way)")
	}

	return a
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

	// Deregister every entity, and from here on ignore anything devices still send. This only touches
	// state owned by this goroutine and doesn't wait on anything, so it's safe to do right here.
	p.registry.Shutdown()

	// Take the broker out of the plugin's state, so that closing it is the job of exactly one place.
	broker := p.broker
	p.broker = nil

	// The broker never started (nothing was configured, or startup failed), so there's nothing to
	// wait for.
	if broker == nil {
		return p.container.ClosedCallbackChannel()
	}

	// Close the broker on its own goroutine, and tell the core when it's finished by closing the
	// channel we hand back. Until then this goroutine stays free to run whatever the broker's
	// connections are still handing over, which the registry now ignores, so they can finish and
	// Close can return.
	done := make(chan func())

	go func() {
		defer close(done)

		if err := broker.Close(); err != nil {
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
