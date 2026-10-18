package esphome

import (
	"context"
	"errors"
	"fmt"
	"github.com/avanha/pmaas-spi/logging"
	"log/slog"
	"net"

	"github.com/avanha/pmaas-common/mailbox"
	"github.com/avanha/pmaas-plugin-esphome/internal/broker"
	httphandler "github.com/avanha/pmaas-plugin-esphome/internal/http"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/clock"
	"github.com/avanha/pmaas-spi/events"
	"google.golang.org/protobuf/proto"
)

var (
	errNativeNotRunning   = errors.New("the native API isn't running")
	errNativeNotConnected = errors.New("the device isn't connected")
)

type Plugin interface {
	spi.IPMAASPlugin
}

type plugin struct {
	log       *slog.Logger
	config    PluginConfig
	container spi.IPMAASContainer
	registry  *registry
	broker    *broker.Broker
	native    *nativeManager

	// nativeDial replaces how connections to devices are made, for tests.
	nativeDial nativeDialFunc
	// outbox carries commands to the broker and to devices, so that the plugin goroutine never waits on either.
	// It's started with whichever of them there is, and stopped just before they're closed.
	outbox      *mailbox.Outbox
	httpHandler *httphandler.Handler

	// listenAddress is where the broker is listening, or was configured to; brokerProblem is why it isn't
	// running, if it should be; brokerNotUsed is that there's no MQTT device for it to serve. All are for the
	// status page.
	listenAddress string
	brokerProblem string
	brokerNotUsed bool

	// clockReceiver is the registration to be told each minute, which is what has the devices with an alert for going
	// offline looked at again, and the Bluetooth proxies made to do what is wanted of them. interestReceiver is the one for
	// what the bluetooth plugin wants.
	clockReceiver              int
	clockReceiverRegistered    bool
	interestReceiver           int
	interestReceiverRegistered bool

	// proxies are the devices that are Bluetooth proxies, which are sources of advertisements.
	proxies *bluetoothProxies
}

// Force implementation of spi.IPMAASPlugin
var _ spi.IPMAASPlugin = (*plugin)(nil)

func NewPlugin(config PluginConfig) Plugin {
	return &plugin{config: config.withDefaults(), httpHandler: httphandler.NewHandler()}
}

func (p *plugin) ShortName() string {
	return "esphome"
}

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.container = container
	p.log = logging.For(container)
	p.registry = newRegistry(container, p.config.DiscoveryPrefix, p.config.displayNames())
	p.registry.control, _ = p.config.switchControls() // what's wrong with it is reported when the plugin starts
	p.registry.handles = p.config.handles
	p.registry.offlineAlerts = p.config.offlineAlerts()
	p.registry.publish = p.publishCommand
	p.registry.nativeSend = p.sendNative
	p.proxies = newBluetoothProxies(container, p.sendNative, p.registry.deviceDisplayName, p.config.bluetoothDisabled())
	p.proxies.reconnect = p.reconnectNative
	p.httpHandler.Init(container, &entityStoreAdapter{parent: p})
}

func (p *plugin) Start() {
	p.log.Info("starting")

	p.listenAddress = p.config.ListenAddress

	nativeDevices, problems := p.config.validNativeDevices()
	for _, problem := range problems {
		p.log.Error("invalid configuration", "problem", problem)
	}

	_, controlProblems := p.config.switchControls()
	for _, problem := range controlProblems {
		p.log.Error("invalid configuration", "problem", problem)
	}

	if len(p.config.Devices) == 0 && len(nativeDevices) == 0 {
		p.brokerProblem = "no devices are configured"
		p.log.Warn("no devices configured, not starting the MQTT broker")

		return
	}

	p.outbox = mailbox.NewOutbox()

	p.startMQTT()
	p.startNative(nativeDevices)
	p.startOfflineChecks(len(nativeDevices) > 0)
}

// startOfflineChecks has the alerts for devices going offline looked at each minute, by the clock's event: one that waits for
// a device to have been offline for a time comes due with nothing happening. The same minute is when the Bluetooth proxies
// are made to do what is wanted of them, which notices what is wanted lapsing, so it is also listened for when there are
// devices that might be proxies. Those, and what the bluetooth plugin wants, are listened for here too.
func (p *plugin) startOfflineChecks(mayHaveProxies bool) {
	p.registry.startOfflineWatches()

	if mayHaveProxies {
		handle, err := p.container.RegisterEventReceiver(bluetooth.InterestPredicate, p.proxies.onInterest)
		if err != nil {
			p.log.Error("unable to listen for what Bluetooth advertisements are wanted", "error", err)
		} else {
			p.interestReceiver, p.interestReceiverRegistered = handle, true
		}
	}

	if len(p.registry.offlineWatches) == 0 && !mayHaveProxies {
		return
	}

	handle, err := p.container.RegisterEventReceiver(isMinuteChanged, p.onClockEvent)
	if err != nil {
		p.log.Error("unable to register for clock events, so offline alerts will only follow connections", "error", err)

		return
	}

	p.clockReceiver, p.clockReceiverRegistered = handle, true
}

func isMinuteChanged(eventInfo *events.EventInfo) bool {
	_, ok := eventInfo.Event.(clock.MinuteChangedEvent)

	return ok
}

// onClockEvent is told each minute, on the plugin goroutine.
func (p *plugin) onClockEvent(eventInfo *events.EventInfo) error {
	if _, ok := eventInfo.Event.(clock.MinuteChangedEvent); ok {
		p.registry.checkOfflineAlerts()
		p.proxies.reconcile()
	}

	return nil
}

// startMQTT starts the broker, if there are MQTT devices for it to serve.
func (p *plugin) startMQTT() {
	if len(p.config.Devices) == 0 {
		p.brokerNotUsed = true
		return
	}

	mqttBroker, address, err := p.startBroker()
	if err != nil {
		p.brokerProblem = err.Error()
		p.log.Error("not starting the MQTT broker", "error", err)

		return
	}

	p.broker = mqttBroker
	p.listenAddress = address.String()

	p.log.Info("MQTT broker listening", "address", address, "devices", len(p.config.Devices))
}

// startNative connects to the devices reached over the native API.
func (p *plugin) startNative(devices []NativeDeviceConfig) {
	if len(devices) == 0 {
		return
	}

	p.native = newNativeManager(devices, p.nativeCallbacks(), logging.For(p.container, "native"))

	if p.nativeDial != nil {
		p.native.dial = p.nativeDial
	}

	p.native.Start()

	p.log.Info("connecting to devices over the native API", "devices", len(devices))
}

// nativeCallbacks hands everything the native API connections report over to the plugin goroutine, the same way
// and for the same reasons as the broker's.
func (p *plugin) nativeCallbacks() nativeCallbacks {
	return nativeCallbacks{
		onConnected: func(deviceName string, address string) {
			p.enqueue(func() { p.registry.HandleConnected(deviceName, address) })
		},
		onMessage: func(deviceName string, message proto.Message) {
			// A Bluetooth proxy sends a lot of these, which are not the registry's business. Decoded and forwarded here, on
			// the connection's goroutine, they never queue for the plugin's.
			switch m := message.(type) {
			case *pb.BluetoothLERawAdvertisementsResponse:
				p.proxies.advertisements(deviceName, m)

				return
			case *pb.BluetoothScannerStateResponse:
				p.enqueue(func() { p.proxies.scannerState(deviceName, m) })

				return
			case *pb.DeviceInfoResponse:
				p.enqueue(func() { p.proxies.deviceInfo(deviceName, m) })
			}

			if p.config.CaptureMessages {
				p.log.Debug("native message", "device", deviceName, "type", fmt.Sprintf("%T", message), "message", truncate([]byte(fmt.Sprint(message)), 2048))
			}

			p.enqueue(func() { p.registry.HandleNativeMessage(deviceName, message) })
		},
		onDisconnected: func(deviceName string) {
			p.enqueue(func() {
				p.registry.HandleDisconnected(deviceName)
				p.proxies.disconnected(deviceName)
			})
		},
	}
}

// reconnectNative ends the connection to a device reached over the native API, so that the manager makes it again. It runs on the
// plugin goroutine and returns at once, since closing can wait on a device that has stopped reading.
func (p *plugin) reconnectNative(deviceName string) {
	if p.native == nil || p.outbox == nil {
		return
	}

	conn := p.native.conn(deviceName)
	if conn == nil {
		return
	}

	if err := p.outbox.Post(func() { _ = conn.Close() }); err != nil {
		p.log.Error("unable to reconnect to a device", "device", deviceName, "error", err)
	}
}

// sendNative queues a message for a device reached over the native API. It runs on the plugin goroutine and
// returns at once; sending can wait on a device that has stopped reading, so it's done on the outbox's.
func (p *plugin) sendNative(deviceName string, message proto.Message) error {
	if p.native == nil || p.outbox == nil {
		return errNativeNotRunning
	}

	conn := p.native.conn(deviceName)
	if conn == nil {
		return errNativeNotConnected
	}

	return p.outbox.Post(func() {
		p.log.Debug("sending a command", "device", deviceName, "type", fmt.Sprintf("%T", message))

		if err := conn.Send(message); err != nil {
			p.log.Error("unable to send a command", "device", deviceName, "error", err)
		}
	})
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
		Logger:          brokerLogger(logging.For(p.container, "broker")),
	}, p.brokerCallbacks())
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}

	return mqttBroker, listener.Addr(), nil
}

// publishCommand queues a message for the broker to publish, to a device that's subscribed to the topic. It
// runs on the plugin goroutine and returns at once, with an error only if there's nothing to publish with:
// delivery happens on the outbox's goroutine, where a failure is logged, since by then nobody is waiting.
func (p *plugin) publishCommand(topic string, payload []byte) error {
	if p.broker == nil || p.outbox == nil {
		return errBrokerNotRunning
	}

	mqttBroker := p.broker

	return p.outbox.Post(func() {
		if err := mqttBroker.Publish(topic, payload, false); err != nil {
			p.log.Error("unable to publish a command", "topic", topic, "error", err)
		}
	})
}

// brokerLogger is where the broker reports what it refuses: a device that fails to authenticate, or touches
// a topic it may not. Those are the things an operator needs to see, since a device that's refused just
// keeps reconnecting and says nothing useful. Warnings and errors only, since the broker's own debug and
// info output is noisy.
func brokerLogger(log *slog.Logger) *slog.Logger {
	return slog.New(dropUnnamedBrokerWarnings{log.Handler()})
}

// dropUnnamedBrokerWarnings discards the broker's one warning that has no message. It logs, with an empty
// message, every connection that ended with an error, and an ESP32 that reboots or loses its WiFi ends that
// way ("error=EOF"). That's routine, and the registry already logs the device as disconnected.
type dropUnnamedBrokerWarnings struct {
	slog.Handler
}

// WithAttrs and WithGroup have to wrap what they return, or the filter is lost: the broker derives its
// loggers with With, which would otherwise hand back the bare handler underneath.
func (h dropUnnamedBrokerWarnings) WithAttrs(attrs []slog.Attr) slog.Handler {
	return dropUnnamedBrokerWarnings{h.Handler.WithAttrs(attrs)}
}

func (h dropUnnamedBrokerWarnings) WithGroup(name string) slog.Handler {
	return dropUnnamedBrokerWarnings{h.Handler.WithGroup(name)}
}

func (h dropUnnamedBrokerWarnings) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "" {
		return nil
	}

	return h.Handler.Handle(ctx, record)
}

// brokerCallbacks hands everything the broker reports over to the plugin goroutine. The callbacks
// run on the broker's goroutines and wait for the plugin goroutine to take each call, which keeps a
// device's messages in the order it sent them, and makes a slow plugin slow the devices down rather
// than queueing without bound.
func (p *plugin) brokerCallbacks() broker.Callbacks {
	return broker.Callbacks{
		OnMessage: func(device string, topic string, payload []byte, retain bool) {
			if p.config.CaptureMessages {
				p.log.Debug("mqtt message", "device", device, "retain", retain, "topic", topic, "payload", truncate(payload, 2048))
			}

			p.enqueue(func() { p.registry.HandleMessage(device, topic, payload) })
		},
		OnDeviceConnected: func(device string, address string) {
			p.enqueue(func() { p.registry.HandleConnected(device, address) })
		},
		OnDeviceDisconnected: func(device string) {
			p.enqueue(func() { p.registry.HandleDisconnected(device) })
		},
	}
}

func (p *plugin) enqueue(f func()) {
	if err := p.container.EnqueueOnPluginGoRoutine(f); err != nil {
		// The plugin is stopping; there's nothing left to tell.
		p.log.Warn("dropping broker event", "error", err)
	}
}

// Stop runs on the plugin goroutine, so it must not wait for the broker: closing it waits for its
// connections to finish, and one of them may be waiting for this goroutine to take a message.
// Entities are deregistered here, and the broker is closed on a background goroutine; the returned
// channel closes when that's done, and the plugin goroutine is free in the meantime.
func (p *plugin) Stop() chan func() {
	p.log.Info("stopping")

	// Deregister every entity, and from here on ignore anything devices still send. This only touches
	// state owned by this goroutine and doesn't wait on anything, so it's safe to do right here.
	p.registry.Shutdown()
	p.proxies.shutdown()

	if p.interestReceiverRegistered {
		p.interestReceiverRegistered = false

		if err := p.container.DeregisterEventReceiver(p.interestReceiver); err != nil {
			p.log.Error("unable to stop listening for what Bluetooth advertisements are wanted", "error", err)
		}
	}

	// Take what's running out of the plugin's state, so that closing it is the job of exactly one place. With
	// it gone, nothing more is queued: publishCommand and sendNative fail instead.
	broker := p.broker
	p.broker = nil
	nativeDevices := p.native
	p.native = nil
	commands := p.outbox
	p.outbox = nil

	if p.clockReceiverRegistered {
		p.clockReceiverRegistered = false

		if err := p.container.DeregisterEventReceiver(p.clockReceiver); err != nil {
			p.log.Error("unable to deregister the clock event receiver", "error", err)
		}
	}

	// Nothing ever started (nothing was configured, or startup failed), so there's nothing to wait for.
	if broker == nil && nativeDevices == nil && commands == nil {
		return p.container.ClosedCallbackChannel()
	}

	// Close it all on its own goroutine, and tell the core when it's finished by closing the channel we hand
	// back. Until then this goroutine stays free to run whatever the connections are still handing over, which
	// the registry now ignores, so they can finish and closing can return.
	done := make(chan func())

	go func() {
		defer close(done)

		// Hand the commands already queued on first, while there's still somewhere to send them. That's all
		// that's promised: the connection may be closed before a message reaches the device.
		if commands != nil {
			commands.Stop()
		}

		if nativeDevices != nil {
			nativeDevices.Stop()
		}

		if broker != nil {
			if err := broker.Close(); err != nil {
				p.log.Error("error closing the MQTT broker", "error", err)
			}
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
