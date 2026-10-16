package nativeapi

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// The API version this client announces. A device uses it to decide what it may send, so it's the version of
// what's understood here, not the newest there is: messages for things this doesn't handle are skipped.
const (
	clientAPIMajor uint32 = 1
	clientAPIMinor uint32 = 14

	// Devices are expected to speak major version 1, and 2 is what Home Assistant accepts; a higher one is a
	// protocol this wasn't written for.
	maxDeviceAPIMajor uint32 = 2
)

const (
	defaultDialTimeout = 10 * time.Second
	defaultSetupTime   = 15 * time.Second
	defaultKeepAlive   = 20 * time.Second
	writeTimeout       = 10 * time.Second

	// A device that has said nothing for this many keepalive intervals, not even to answer a ping, is gone.
	// Home Assistant uses the same ratio.
	keepAliveTimeoutRatio = 4.5
)

// Config says how to reach a device.
type Config struct {
	// Address is host:port. ESPHome devices listen on port 6053.
	Address string

	// Key is the device's api: encryption: key, base64 encoded, or empty if it isn't using encryption.
	Key string

	// Name is the ESPHome name the device is expected to have. If it isn't empty and the device reports a
	// different one, the connection is refused with a *WrongDeviceError, which is what stops PMAAS reading
	// another device's state when an address has been handed to something else.
	Name string

	// ClientInfo is how this client describes itself to the device, which shows in the device's log. It
	// defaults to "PMAAS".
	ClientInfo string

	// Timezone is sent when the device asks for the time, in POSIX TZ format ("MST7MDT,M3.2.0,M11.1.0"). If
	// empty, only the time itself is sent.
	Timezone string

	DialTimeout  time.Duration // for the TCP connection. Default 10s.
	SetupTimeout time.Duration // for the handshake and hello once connected. Default 15s.
	KeepAlive    time.Duration // between pings. Default 20s.

	// Logf receives what's worth knowing but isn't an error: a message skipped for being unknown. May be nil.
	Logf func(format string, args ...any)

	// dial replaces the TCP connection, for tests.
	dial func(ctx context.Context, address string) (net.Conn, error)
}

// Handler receives every message from the device other than the connection housekeeping done here: its entities,
// their states, its answers to requests. It runs on the connection's read loop, so the next message waits for it
// to return, which is what keeps messages in order, and a handler that takes too long makes the device look dead
// (see Config.KeepAlive).
type Handler func(message proto.Message)

// Conn is a connection to one device.
type Conn struct {
	cfg     Config
	conn    net.Conn
	codec   codec
	hello   *pb.HelloResponse
	handler Handler

	writeMutex sync.Mutex

	lastRead atomic.Int64 // when anything was last received, as unix nanoseconds

	finishOnce sync.Once
	err        error         // why the connection ended; set before the socket is closed, read after done
	stop       chan struct{} // closed when the connection is ending, to stop the keepalive
	done       chan struct{} // closed once the read loop has exited, so the handler will never run again
}

// Dial connects to a device, does the encryption handshake if there's a key and the API hello, and starts
// delivering messages to handler. Cancelling ctx abandons a connection that's still being set up; it has no
// effect once Dial returns, and Close ends the connection.
func Dial(ctx context.Context, cfg Config, handler Handler) (*Conn, error) {
	if cfg.ClientInfo == "" {
		cfg.ClientInfo = "PMAAS"
	}

	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}

	if cfg.SetupTimeout <= 0 {
		cfg.SetupTimeout = defaultSetupTime
	}

	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = defaultKeepAlive
	}

	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	conn, err := connect(ctx, cfg)
	if err != nil {
		return nil, err
	}

	c := &Conn{
		cfg:     cfg,
		conn:    conn,
		handler: handler,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	if err := c.setup(ctx); err != nil {
		_ = conn.Close()

		return nil, err
	}

	c.lastRead.Store(time.Now().UnixNano())

	go c.readLoop()
	go c.keepAlive()

	return c, nil
}

func connect(ctx context.Context, cfg Config) (net.Conn, error) {
	if cfg.dial != nil {
		return cfg.dial(ctx, cfg.Address)
	}

	dialer := net.Dialer{Timeout: cfg.DialTimeout}

	return dialer.DialContext(ctx, "tcp", cfg.Address)
}

// setup runs the transport handshake and the API hello, under a deadline and with ctx able to cut it short.
func (c *Conn) setup(ctx context.Context) error {
	if err := c.conn.SetDeadline(time.Now().Add(c.cfg.SetupTimeout)); err != nil {
		return err
	}

	// Closing the socket is what interrupts a read that's waiting on a device that never answers.
	setupDone := make(chan struct{})
	defer close(setupDone)

	go func() {
		select {
		case <-ctx.Done():
			_ = c.conn.Close()
		case <-setupDone:
		}
	}()

	reader := bufio.NewReader(c.conn)

	if c.cfg.Key == "" {
		c.codec = &plaintextCodec{reader: reader, writer: c.conn}
	} else {
		codec, err := newNoiseCodec(reader, c.conn, c.cfg.Key)
		if err != nil {
			return c.setupError(ctx, err)
		}

		c.codec = codec
	}

	if err := c.Send(&pb.HelloRequest{
		ClientInfo:      c.cfg.ClientInfo,
		ApiVersionMajor: clientAPIMajor,
		ApiVersionMinor: clientAPIMinor,
	}); err != nil {
		return c.setupError(ctx, err)
	}

	// What comes before the hello response isn't only the hello response: a real device asks for the time first,
	// before saying hello at all. The connection's housekeeping is answered as it arrives, and anything else
	// that isn't the hello is skipped, since it can't be about anything yet.
	for c.hello == nil {
		message, err := c.readOne()
		if err != nil {
			return c.setupError(ctx, err)
		}

		if message == nil {
			continue
		}

		if c.housekeeping(message) {
			continue
		}

		response, ok := message.(*pb.HelloResponse)
		if !ok {
			c.cfg.Logf("%s: skipping a %T that came before the device's hello", c.cfg.Address, message)

			continue
		}

		c.hello = response
	}

	if c.hello.ApiVersionMajor > maxDeviceAPIMajor {
		return fmt.Errorf("%w: version %d.%d", ErrIncompatibleVersion, c.hello.ApiVersionMajor, c.hello.ApiVersionMinor)
	}

	if c.cfg.Name != "" && c.hello.Name != c.cfg.Name {
		return &WrongDeviceError{Expected: c.cfg.Name, Got: sanitize(c.hello.Name)}
	}

	// Firmware from before ESPHome 2026.1 wants the authentication request, with no password, straight after the
	// hello and before anything else: until it gets one it ignores the requests that follow, and lists nothing.
	// Newer firmware has dropped it and takes no notice. Home Assistant's client sends it for the same reason. Its
	// answer isn't waited for, since newer devices never give one: it's dealt with when it arrives, in housekeeping.
	if err := c.Send(&pb.AuthenticationRequest{}); err != nil { //nolint:staticcheck // legacy authentication
		return c.setupError(ctx, err)
	}

	return c.conn.SetDeadline(time.Time{})
}

// setupError reports a failure during setup as ctx's own error if that's why the connection was closed.
func (c *Conn) setupError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

// readOne reads one message. An id this build doesn't know is skipped, and reported as a nil message.
func (c *Conn) readOne() (proto.Message, error) {
	id, payload, err := c.codec.readMessage()
	if err != nil {
		return nil, err
	}

	message, known := newMessage(id)
	if !known {
		c.cfg.Logf("%s: skipping a message of unknown type %d (%d bytes): the device's firmware is newer than this client",
			c.cfg.Address, id, len(payload))

		return nil, nil
	}

	if err := proto.Unmarshal(payload, message); err != nil {
		return nil, fmt.Errorf("%w: a message of type %d didn't parse: %w", ErrProtocol, id, err)
	}

	return message, nil
}

// Hello is what the device said about itself when the connection was made: its name, its API version and what it
// calls its firmware.
func (c *Conn) Hello() *pb.HelloResponse {
	return c.hello
}

// Send sends a message to the device. It can be called from any goroutine.
func (c *Conn) Send(message proto.Message) error {
	id, err := messageID(message)
	if err != nil {
		return err
	}

	payload, err := proto.Marshal(message)
	if err != nil {
		return err
	}

	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()

	// A device that has stopped reading mustn't be able to block whoever's sending, indefinitely.
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}

	return c.codec.writeMessage(id, payload)
}

// Done is closed when the connection has ended and the handler will never be called again. Err says why.
func (c *Conn) Done() <-chan struct{} {
	return c.done
}

// Err is why the connection ended: what went wrong, a *DeviceDisconnectedError if the device ended it, or
// ErrClosed if Close did. It's nil until Done is closed.
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close ends the connection, and returns once the handler has stopped being called. It tells the device it's
// going, so the device frees the connection at once rather than when it notices.
func (c *Conn) Close() error {
	_ = c.Send(&pb.DisconnectRequest{})

	c.finish(ErrClosed)
	<-c.done

	return nil
}

// finish records why the connection is ending and closes the socket, which ends the read loop. Only the first
// reason is kept.
func (c *Conn) finish(err error) {
	c.finishOnce.Do(func() {
		c.err = err

		close(c.stop)
		_ = c.conn.Close()
	})
}

func (c *Conn) readLoop() {
	defer close(c.done)

	for {
		message, err := c.readOne()
		if err != nil {
			c.finish(err)

			return
		}

		if message == nil {
			continue
		}

		c.lastRead.Store(time.Now().UnixNano())

		if !c.housekeeping(message) {
			c.handler(message)
		}
	}
}

// housekeeping answers the messages that belong to the connection rather than to what's on the device, and
// reports whether it was one.
func (c *Conn) housekeeping(message proto.Message) bool {
	switch m := message.(type) {
	case *pb.PingRequest:
		if err := c.Send(&pb.PingResponse{}); err != nil {
			c.finish(err)
		}
	case *pb.PingResponse:
		// Its arrival is what mattered: it refreshed lastRead.
	case *pb.DisconnectRequest:
		_ = c.Send(&pb.DisconnectResponse{})
		c.finish(&DeviceDisconnectedError{Reason: m.Reason})
	case *pb.DisconnectResponse:
		c.finish(ErrClosed)
	case *pb.AuthenticationResponse: //nolint:staticcheck // legacy authentication
		// The device has a password, which can't be given.
		if m.InvalidPassword {
			c.finish(ErrPasswordRequired)
		}
	case *pb.GetTimeRequest:
		response := &pb.GetTimeResponse{EpochSeconds: uint32(time.Now().Unix())}
		if c.cfg.Timezone != "" {
			response.Timezone = c.cfg.Timezone
		}

		if err := c.Send(response); err != nil {
			c.finish(err)
		}
	default:
		return false
	}

	return true
}

// keepAlive pings the device, and ends the connection if it has gone quiet. Without it, a device that dropped off
// the network without closing the connection would look connected until the operating system gave up on it, which
// can take hours.
func (c *Conn) keepAlive() {
	ticker := time.NewTicker(c.cfg.KeepAlive)
	defer ticker.Stop()

	limit := time.Duration(float64(c.cfg.KeepAlive) * keepAliveTimeoutRatio)

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}

		if time.Since(time.Unix(0, c.lastRead.Load())) > limit {
			c.finish(ErrKeepAliveTimeout)

			return
		}

		if err := c.Send(&pb.PingRequest{}); err != nil {
			c.finish(err)

			return
		}
	}
}
