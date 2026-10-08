// Package broker embeds an MQTT broker (mochi-mqtt) with per-device authentication and topic
// ACLs, and reports what devices publish and when they connect or disconnect.
//
// Every callback is invoked on one of the broker's own goroutines, not the caller's. A callback
// that blocks holds up that device's connection, which is acceptable back-pressure but is the
// caller's decision to make.
package broker

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// Config configures a Broker.
type Config struct {
	// Listener is the already-bound listener to accept device connections on. Binding it is the
	// caller's job so that a bad address is reported to the caller, and so TLS is just a
	// different net.Listener.
	Listener net.Listener

	// Devices maps each allowed device name to its password. The device name is both the MQTT
	// username it must connect with and the root of the topic tree it may use.
	Devices map[string]string

	// DiscoveryPrefix is the discovery topic prefix devices publish their configs under.
	DiscoveryPrefix string

	// Logger receives the broker's own log output. Nil discards it.
	Logger *slog.Logger
}

// Callbacks is how a Broker reports what happens on it. Any may be nil.
type Callbacks struct {
	// OnMessage is called for every message a device publishes that the ACL allowed, retained
	// or not. payload is the caller's to keep.
	OnMessage func(device string, topic string, payload []byte, retain bool)

	// OnDeviceConnected and OnDeviceDisconnected are called when a device's session starts and
	// ends. If a device reconnects before its old connection is noticed to be dead, only the new
	// connection counts: the old one's end is not reported.
	OnDeviceConnected    func(device string)
	OnDeviceDisconnected func(device string)
}

// Broker is an embedded MQTT broker.
type Broker struct {
	server *mqtt.Server
}

// New starts a broker serving cfg.Listener.
func New(cfg Config, callbacks Callbacks) (*Broker, error) {
	if cfg.Listener == nil {
		return nil, fmt.Errorf("no listener")
	}

	for name, password := range cfg.Devices {
		if !validDeviceName(name) {
			return nil, fmt.Errorf("invalid device name %q", name)
		}

		if password == "" {
			return nil, fmt.Errorf("device %q has no password", name)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	server := mqtt.New(&mqtt.Options{InlineClient: true, Logger: logger})

	h := &hook{
		devices:         cfg.Devices,
		discoveryPrefix: cfg.DiscoveryPrefix,
		callbacks:       callbacks,
		logger:          logger,
		sessions:        make(map[string]*session, len(cfg.Devices)),
	}

	for name := range cfg.Devices {
		h.sessions[name] = &session{}
	}

	if err := server.AddHook(h, nil); err != nil {
		return nil, err
	}

	if err := server.AddListener(listeners.NewNet("esphome", cfg.Listener)); err != nil {
		return nil, err
	}

	if err := server.Serve(); err != nil {
		return nil, err
	}

	return &Broker{server: server}, nil
}

// Publish publishes a message from the broker itself, bypassing the ACL. Safe to call from any
// goroutine, but not from inside a Callbacks function (the broker may be waiting on it).
func (b *Broker) Publish(topic string, payload []byte, retain bool) error {
	return b.server.Publish(topic, payload, retain, 0)
}

// Close stops the broker, disconnecting every device.
func (b *Broker) Close() error {
	return b.server.Close()
}

// esphomeNamespacePrefix is the start of the topics ESPHome itself uses, as opposed to the ones under a
// device's own name (esphome/discover, esphome/ping/<name>).
const esphomeNamespacePrefix = "esphome/"

type hook struct {
	mqtt.HookBase

	devices         map[string]string
	discoveryPrefix string
	callbacks       Callbacks
	logger          *slog.Logger

	// sessions has an entry per configured device and is never modified after construction, so it
	// can be read without a lock.
	sessions map[string]*session
}

// session tracks which connection is a device's current one.
type session struct {
	// mu is held while updating current *and* while delivering the connect or disconnect callback
	// that goes with it. Otherwise a device that reconnects could have the old connection's
	// "disconnected" delivered after the new connection's "connected", leaving a connected device
	// looking offline.
	mu      sync.Mutex
	current *mqtt.Client
}

func (h *hook) ID() string {
	return "pmaas-esphome"
}

func (h *hook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnectAuthenticate,
		mqtt.OnACLCheck,
		mqtt.OnPublish,
		mqtt.OnPublished,
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
	}, []byte{b})
}

func (h *hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	name := string(pk.Connect.Username)
	password, known := h.devices[name]

	// Compare even for an unknown device, so how long this takes doesn't say which names exist.
	matches := subtle.ConstantTimeCompare([]byte(password), pk.Connect.Password) == 1

	if !known || !matches {
		h.logger.Warn("rejecting device connection", "device", name, "remote", cl.Net.Remote)
		return false
	}

	return true
}

func (h *hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	name := string(cl.Properties.Username)

	if allowed(name, h.discoveryPrefix, topic, write) {
		return true
	}

	attrs := []any{"device", name}

	if write {
		attrs = append(attrs, "topic", topic)
	} else {
		attrs = append(attrs, "filter", topic)
	}

	// The ESPHome topics a device legitimately uses are allowed (see allowed), so another one is something
	// else in ESPHome's own namespace, which the device's configuration should be asked not to use.
	if strings.HasPrefix(topic, esphomeNamespacePrefix) {
		attrs = append(attrs, "hint", "this is in ESPHome's own namespace, not the device's: "+
			"the broker only tolerates esphome/discover and esphome/ping/<device>, which come from "+
			"ESPHome's node discovery (set discover_ip: false in the device's mqtt: section to stop it)")
	}

	if write {
		// The reason a device that does this keeps reconnecting: mochi drops a denied QoS 0 publish quietly, but
		// an MQTT 3.1.1 client has no way to be told "no" to a QoS 1 or 2 publish, so it's disconnected.
		h.logger.Warn("denying publish to a topic the device may not use "+
			"(an MQTT 3.1.1 client that sent it at QoS 1 or 2 is disconnected)", attrs...)
	} else {
		h.logger.Warn("denying subscription to a topic filter the device may not use", attrs...)
	}

	return false
}

// OnPublish runs for a publish the ACL allowed. It's where ESPHome's node discovery is thrown away: the
// publisher gets the acknowledgement it's waiting for, but the message is neither delivered to anyone nor
// retained. (OnPublished still reports it, so capturing messages shows what a device sends.)
func (h *hook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if !cl.Net.Inline && isNodeDiscoveryPublish(string(cl.Properties.Username), pk.TopicName) {
		return pk, packets.CodeSuccessIgnore
	}

	return pk, nil
}

func (h *hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if cl.Net.Inline || h.callbacks.OnMessage == nil {
		return
	}

	// The broker may reuse the packet's buffers once we return.
	payload := bytes.Clone(pk.Payload)

	h.callbacks.OnMessage(string(cl.Properties.Username), pk.TopicName, payload, pk.FixedHeader.Retain)
}

func (h *hook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	name, ok := h.deviceName(cl)
	if !ok {
		return
	}

	s := h.sessions[name]
	s.mu.Lock()
	defer s.mu.Unlock()

	s.current = cl

	if h.callbacks.OnDeviceConnected != nil {
		h.callbacks.OnDeviceConnected(name)
	}
}

func (h *hook) OnDisconnect(cl *mqtt.Client, _ error, _ bool) {
	name, ok := h.deviceName(cl)
	if !ok {
		return
	}

	s := h.sessions[name]
	s.mu.Lock()
	defer s.mu.Unlock()

	// Not the current connection: it was replaced by a newer one, whose connect has been (or is
	// about to be) reported. The device is still there.
	if s.current != cl {
		return
	}

	s.current = nil

	if h.callbacks.OnDeviceDisconnected != nil {
		h.callbacks.OnDeviceDisconnected(name)
	}
}

// deviceName returns the device a client is connected as, if it's one of ours (the inline client
// and anything that failed authentication aren't).
func (h *hook) deviceName(cl *mqtt.Client) (string, bool) {
	if cl.Net.Inline {
		return "", false
	}

	name := string(cl.Properties.Username)
	_, known := h.devices[name]

	return name, known
}
