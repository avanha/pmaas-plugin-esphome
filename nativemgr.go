package esphome

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// nativeConn is what the manager needs of a connection to a device, which is *nativeapi.Conn, and which a test
// can stand in for.
type nativeConn interface {
	Send(message proto.Message) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

type nativeDialFunc func(ctx context.Context, cfg nativeapi.Config, handler nativeapi.Handler) (nativeConn, error)

func dialNative(ctx context.Context, cfg nativeapi.Config, handler nativeapi.Handler) (nativeConn, error) {
	return nativeapi.Dial(ctx, cfg, handler)
}

// nativeCallbacks is what the manager reports. They're called from the manager's goroutines, one device's in
// order, and a callback that waits makes that device's connection wait: the plugin's hand them to its own
// goroutine and wait for it to take each, which is what keeps messages in order without letting them queue up
// without bound.
type nativeCallbacks struct {
	onConnected    func(deviceName string, address string)
	onMessage      func(deviceName string, message proto.Message)
	onDisconnected func(deviceName string)
}

// nativeManager keeps a connection to each of a set of devices: it connects, asks each device what it has and to
// report its states, and when the connection ends, for whatever reason, connects again. A device that's down is
// tried again with a growing delay rather than hammered.
type nativeManager struct {
	devices   []NativeDeviceConfig
	dial      nativeDialFunc
	callbacks nativeCallbacks
	log       *slog.Logger

	// How long to wait before trying a device again after a connection fails or ends: from minBackoff, doubling up
	// to maxBackoff. A failure that trying again sooner can't fix (the wrong key, the wrong device) waits
	// maxBackoff straight away. A connection that lasted at least stableAfter starts the sequence over.
	minBackoff  time.Duration
	maxBackoff  time.Duration
	stableAfter time.Duration

	// jitter spreads out reconnection after a network outage, which would otherwise have every device on it try
	// at the same moment. It returns a fraction in [0, 1).
	jitter func() float64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	conns    map[string]nativeConn
	problems map[string]string
}

func newNativeManager(devices []NativeDeviceConfig, callbacks nativeCallbacks, log *slog.Logger) *nativeManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &nativeManager{
		devices:     devices,
		dial:        dialNative,
		callbacks:   callbacks,
		log:         log,
		minBackoff:  time.Second,
		maxBackoff:  time.Minute,
		stableAfter: 30 * time.Second,
		jitter:      rand.Float64,
		ctx:         ctx,
		cancel:      cancel,
		conns:       make(map[string]nativeConn),
		problems:    make(map[string]string),
	}
}

// Start begins connecting to every device.
func (m *nativeManager) Start() {
	for _, device := range m.devices {
		m.wg.Add(1)

		go m.run(device)
	}
}

// Stop ends every connection and waits for the manager's goroutines, which includes callbacks that are running,
// so none is called once it returns. It must not be called from a goroutine a callback is waiting on.
func (m *nativeManager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// conn returns the connection to a device, or nil if there isn't one at the moment.
func (m *nativeManager) conn(deviceName string) nativeConn {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.conns[deviceName]
}

// problem says why a device isn't connected, if it's known: the last attempt failed, or the device is a
// different one. It's empty for a device that's connected, and before the first attempt has failed.
func (m *nativeManager) problem(deviceName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.problems[deviceName]
}

func (m *nativeManager) setConn(deviceName string, conn nativeConn, problem string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if conn == nil {
		delete(m.conns, deviceName)
	} else {
		m.conns[deviceName] = conn
	}

	if problem == "" {
		delete(m.problems, deviceName)
	} else {
		m.problems[deviceName] = problem
	}
}

func (m *nativeManager) run(device NativeDeviceConfig) {
	defer m.wg.Done()

	backoff := m.minBackoff
	address := device.dialAddress()

	cfg := nativeapi.Config{
		Address: address, Key: device.Key, Name: device.Name, ClientInfo: "PMAAS",
		Logf: func(format string, args ...any) { m.log.Debug(fmt.Sprintf(format, args...), "device", device.Name) },
	}

	for m.ctx.Err() == nil {
		conn, err := m.dial(m.ctx, cfg, func(message proto.Message) { m.callbacks.onMessage(device.Name, message) })
		if err != nil {
			if m.ctx.Err() != nil {
				return
			}

			problem := describeNativeError(err)
			m.setConn(device.Name, nil, problem)
			m.logf(slog.LevelError, "%s: unable to connect to %s: %s", device.Name, address, problem)

			if !m.wait(m.delayAfterFailure(err, &backoff)) {
				return
			}

			continue
		}

		started := time.Now()

		m.setConn(device.Name, conn, "")
		m.callbacks.onConnected(device.Name, address)

		// Everything the device has to say arrives through the handler, in the order asked for here: who it is,
		// what it has, then what each is doing.
		for _, request := range []proto.Message{&pb.DeviceInfoRequest{}, &pb.ListEntitiesRequest{}, &pb.SubscribeStatesRequest{}} {
			if err := conn.Send(request); err != nil {
				_ = conn.Close()
				break
			}
		}

		select {
		case <-conn.Done():
		case <-m.ctx.Done():
			_ = conn.Close()
		}

		<-conn.Done()
		m.setConn(device.Name, nil, "")
		m.callbacks.onDisconnected(device.Name)

		if m.ctx.Err() != nil {
			return
		}

		reason := conn.Err()
		m.logf(slog.LevelWarn, "%s: connection to %s ended after %s: %v", device.Name, address, time.Since(started).Round(time.Second), reason)

		if time.Since(started) >= m.stableAfter {
			backoff = m.minBackoff
		}

		// A device that closed the connection itself is rebooting, and comes back within seconds, which the
		// first delay is about right for.
		if !m.wait(m.delayAfterFailure(reason, &backoff)) {
			return
		}
	}
}

// delayAfterFailure is how long to wait before trying again, and moves backoff on. A failure that waiting won't
// fix is waited out slowly, since trying again sooner only fills the log.
func (m *nativeManager) delayAfterFailure(err error, backoff *time.Duration) time.Duration {
	var delay time.Duration

	if isPersistentNativeError(err) {
		delay = m.maxBackoff
	} else {
		delay = *backoff
		*backoff = min(*backoff*2, m.maxBackoff)
	}

	// Up to a fifth longer, so devices that lost the network together don't all return together.
	return delay + time.Duration(float64(delay)*0.2*m.jitter())
}

// wait sleeps, and reports whether the manager is still running at the end of it.
func (m *nativeManager) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-m.ctx.Done():
		return false
	}
}

// isPersistentNativeError is whether retrying soon is pointless because something has to change first: the
// configuration is wrong for the device, or the device isn't the one expected.
func isPersistentNativeError(err error) bool {
	var wrongDevice *nativeapi.WrongDeviceError

	return errors.Is(err, nativeapi.ErrInvalidKey) ||
		errors.Is(err, nativeapi.ErrEncryptionRequired) ||
		errors.Is(err, nativeapi.ErrDeviceNotEncrypted) ||
		errors.Is(err, nativeapi.ErrIncompatibleVersion) ||
		errors.Is(err, nativeapi.ErrPasswordRequired) ||
		errors.As(err, &wrongDevice)
}

// describeNativeError says what went wrong in terms of what to do about it, for the status page and the log.
func describeNativeError(err error) string {
	var wrongDevice *nativeapi.WrongDeviceError

	switch {
	case errors.Is(err, nativeapi.ErrInvalidKey):
		return "the device doesn't accept the configured encryption key"
	case errors.Is(err, nativeapi.ErrEncryptionRequired):
		return "the device requires encryption: set the device's Key to its api: encryption: key"
	case errors.Is(err, nativeapi.ErrPasswordRequired):
		return "the device has an API password, which PMAAS can't use: replace it with an api: encryption: key on the device, " +
			"and give PMAAS the key"
	case errors.Is(err, nativeapi.ErrDeviceNotEncrypted):
		return "a Key is configured but the device isn't using encryption: remove the Key, or set one on the device"
	case errors.As(err, &wrongDevice):
		return fmt.Sprintf("the device at this address calls itself %q, not %q: Name must be the device's ESPHome name "+
			"(use DisplayName to call it something else on the page), and the address must be its address",
			wrongDevice.Got, wrongDevice.Expected)
	default:
		return err.Error()
	}
}

// logf logs a message at the level, worded as it is for a person reading the log; the device it is about is in it.
func (m *nativeManager) logf(level slog.Level, format string, args ...any) {
	m.log.Log(context.Background(), level, fmt.Sprintf(format, args...))
}
