package nativeapi

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// testKey is a valid key: 32 bytes, base64 encoded, as ESPHome writes them.
var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))

// otherKey is a different valid one.
var otherKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x17}, 32))

// fakeDevice is an ESPHome device, enough of one to talk to: it listens, speaks the plaintext or encrypted
// framing, and answers the requests a client makes. It's written against the protocol rather than against the
// client's own code, so that the two agreeing means something; the encryption side in particular is a separate
// responder, not the client's turned around.
type fakeDevice struct {
	t        *testing.T
	listener net.Listener

	key  string // empty for a plaintext device
	name string
	mac  string

	// Behaviour a test can change before connecting.
	serverHelloSuffix bool                                         // include the name and MAC in the encrypted hello
	apiMajor          uint32                                       // in the hello response; default 1
	ignorePings       bool                                         // never answer a ping
	silentAfterHello  bool                                         // answer the hello and then nothing at all
	neverAnswerHello  bool                                         // read the hello and say nothing
	askTimeFirst      bool                                         // ask for the time, as a real device does, before the hello response
	requireAuth       bool                                         // old firmware: ignores requests until authenticated
	invalidPassword   bool                                         // with requireAuth, says the (empty) password is wrong
	sendOtherFirst    bool                                         // send a message that isn't the hello response first
	refuseHandshake   string                                       // answer the encrypted handshake with this, as text
	onMessage         func(d *deviceSession, m proto.Message) bool // return true if handled

	mu       sync.Mutex
	sessions []*deviceSession
	accepted chan *deviceSession
}

// deviceSession is one connection to a fakeDevice.
type deviceSession struct {
	device *fakeDevice
	conn   net.Conn
	reader *bufio.Reader

	writeMutex sync.Mutex
	write      func(id uint32, payload []byte) error

	mu       sync.Mutex
	received []proto.Message
	changed  chan struct{}

	authenticated bool // only touched by the session's own goroutine
}

func newFakeDevice(t *testing.T, key string) *fakeDevice {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	d := &fakeDevice{
		t: t, listener: listener, key: key, name: "plug1", mac: "AA:BB:CC:DD:EE:FF",
		serverHelloSuffix: true, apiMajor: 1, accepted: make(chan *deviceSession, 16),
	}

	t.Cleanup(func() {
		_ = listener.Close()

		d.mu.Lock()
		defer d.mu.Unlock()

		for _, s := range d.sessions {
			_ = s.conn.Close()
		}
	})

	go d.acceptLoop()

	return d
}

func (d *fakeDevice) address() string { return d.listener.Addr().String() }

func (d *fakeDevice) config() Config {
	return Config{Address: d.address(), Key: d.key, Name: d.name, KeepAlive: time.Hour, SetupTimeout: 5 * time.Second}
}

func (d *fakeDevice) acceptLoop() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}

		go d.serve(conn)
	}
}

func (d *fakeDevice) serve(conn net.Conn) {
	s := &deviceSession{device: d, conn: conn, reader: bufio.NewReader(conn), changed: make(chan struct{}, 1)}

	var read func() (uint32, []byte, error)

	if d.key == "" {
		// A plaintext device that's spoken to in the encrypted framing answers in its own, which is how the
		// client knows what's wrong.
		if first, err := s.reader.Peek(1); err != nil || first[0] != 0x00 {
			_, _ = conn.Write([]byte{0x00, 0x00, 0x00})
			_ = conn.Close()

			return
		}

		plain := &plaintextCodec{reader: s.reader, writer: conn}
		s.write = plain.writeMessage
		read = plain.readMessage
	} else {
		codec, ok := d.handshake(s)
		if !ok {
			_ = conn.Close()
			return
		}

		s.write = codec.writeMessage
		read = codec.readMessage
	}

	d.mu.Lock()
	d.sessions = append(d.sessions, s)
	d.mu.Unlock()

	d.accepted <- s

	for {
		id, payload, err := read()
		if err != nil {
			return
		}

		message, known := newMessage(id)
		if !known {
			continue
		}

		if proto.Unmarshal(payload, message) != nil {
			return
		}

		s.mu.Lock()
		s.received = append(s.received, message)
		s.mu.Unlock()

		select {
		case s.changed <- struct{}{}:
		default:
		}

		if d.onMessage != nil && d.onMessage(s, message) {
			continue
		}

		d.respond(s, message)
	}
}

// respond is what a device does by default.
func (d *fakeDevice) respond(s *deviceSession, message proto.Message) {
	// Old firmware answers who it is, and then takes no notice of anything until it's been authenticated.
	if d.requireAuth && !s.authenticated {
		switch message.(type) {
		case *pb.HelloRequest, *pb.PingRequest, *pb.DeviceInfoRequest, *pb.DisconnectRequest, *pb.AuthenticationRequest:
		default:
			return
		}
	}

	switch m := message.(type) {
	case *pb.AuthenticationRequest:
		if d.requireAuth {
			s.authenticated = !d.invalidPassword
			s.send(&pb.AuthenticationResponse{InvalidPassword: d.invalidPassword})
		}
	case *pb.HelloRequest:
		if d.neverAnswerHello {
			return
		}

		if d.askTimeFirst {
			s.send(&pb.GetTimeRequest{})
		}

		if d.sendOtherFirst {
			s.send(&pb.SensorStateResponse{Key: 1, State: 2})
		}

		s.send(&pb.HelloResponse{ApiVersionMajor: d.apiMajor, ApiVersionMinor: 10, ServerInfo: "ESPHome 2026.9.1", Name: d.name})

		if d.silentAfterHello {
			s.device.ignorePings = true
		}
	case *pb.PingRequest:
		if !d.ignorePings {
			s.send(&pb.PingResponse{})
		}
	case *pb.DeviceInfoRequest:
		s.send(&pb.DeviceInfoResponse{
			Name: d.name, MacAddress: d.mac, EsphomeVersion: "2026.9.1", Model: "plf10", Manufacturer: "Kauf",
		})
	case *pb.ListEntitiesRequest:
		s.send(&pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 1, Name: "Relay"})
		s.send(&pb.ListEntitiesSensorResponse{ObjectId: "power", Key: 2, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"})
		s.send(&pb.ListEntitiesDoneResponse{})
	case *pb.SubscribeStatesRequest:
		s.send(&pb.SwitchStateResponse{Key: 1, State: false})
		s.send(&pb.SensorStateResponse{Key: 2, State: 12.5})
	case *pb.SwitchCommandRequest:
		s.send(&pb.SwitchStateResponse{Key: m.Key, State: m.State})
	case *pb.DisconnectRequest:
		s.send(&pb.DisconnectResponse{})
		_ = s.conn.Close()
	}
}

// handshake is the device's side of the encrypted handshake, as ESPHome does it.
func (d *fakeDevice) handshake(s *deviceSession) (*serverNoiseCodec, bool) {
	psk, err := decodeKey(d.key)
	if err != nil {
		d.t.Errorf("the fake device has a bad key: %v", err)
		return nil, false
	}

	// An encrypted device that's spoken to in the plaintext framing says it needs encryption.
	if first, err := s.reader.Peek(1); err != nil || first[0] != 0x01 {
		_, _ = s.conn.Write([]byte{0x01, 0x00, 0x00})
		return nil, false
	}

	// The client's hello, which is empty, and its handshake frame.
	hello, err := readNoiseFrame(s.reader)
	if err != nil || len(hello) != 0 {
		return nil, false
	}

	first, err := readNoiseFrame(s.reader)
	if err != nil || len(first) == 0 || first[0] != 0x00 {
		return nil, false
	}

	serverHello := []byte{0x01}
	if d.serverHelloSuffix {
		serverHello = append(serverHello, []byte(d.name)...)
		serverHello = append(serverHello, 0)
		serverHello = append(serverHello, []byte(d.mac)...)
		serverHello = append(serverHello, 0)
	}

	if writeNoiseFrame(s.conn, serverHello) != nil {
		return nil, false
	}

	state, err := noise.NewHandshakeState(noiseConfig(psk, false))
	if err != nil {
		return nil, false
	}

	if d.refuseHandshake != "" {
		_ = writeNoiseFrame(s.conn, append([]byte{0x01}, d.refuseHandshake...))
		return nil, false
	}

	if _, _, _, err := state.ReadMessage(nil, first[1:]); err != nil {
		// What ESPHome says when the client's key isn't its own.
		_ = writeNoiseFrame(s.conn, append([]byte{0x01}, "Handshake MAC failure"...))
		return nil, false
	}

	response, decrypt, encrypt, err := state.WriteMessage(nil, nil)
	if err != nil || writeNoiseFrame(s.conn, append([]byte{0x00}, response...)) != nil {
		return nil, false
	}

	return &serverNoiseCodec{reader: s.reader, writer: s.conn, encrypt: encrypt, decrypt: decrypt}, true
}

// serverNoiseCodec is the encrypted framing as the device sees it.
type serverNoiseCodec struct {
	reader  *bufio.Reader
	writer  io.Writer
	encrypt *noise.CipherState
	decrypt *noise.CipherState
}

func (c *serverNoiseCodec) writeMessage(id uint32, payload []byte) error {
	plain := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(plain[0:2], uint16(id))
	binary.BigEndian.PutUint16(plain[2:4], uint16(len(payload)))
	plain = append(plain, payload...)

	encrypted, err := c.encrypt.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}

	return writeNoiseFrame(c.writer, encrypted)
}

func (c *serverNoiseCodec) readMessage() (uint32, []byte, error) {
	frame, err := readNoiseFrame(c.reader)
	if err != nil {
		return 0, nil, err
	}

	plain, err := c.decrypt.Decrypt(nil, nil, frame)
	if err != nil {
		return 0, nil, err
	}

	return uint32(binary.BigEndian.Uint16(plain[0:2])), plain[4:], nil
}

// send sends a message to the client.
func (s *deviceSession) send(message proto.Message) {
	id, err := messageID(message)
	if err != nil {
		s.device.t.Errorf("fake device: %v", err)
		return
	}

	payload, err := proto.Marshal(message)
	if err != nil {
		s.device.t.Errorf("fake device: %v", err)
		return
	}

	s.sendRaw(id, payload)
}

// sendRaw sends a message by its id and bytes, so that it can be one this client has never heard of.
func (s *deviceSession) sendRaw(id uint32, payload []byte) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()

	_ = s.write(id, payload)
}

// messages returns what the client has sent so far.
func (s *deviceSession) messages() []proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]proto.Message(nil), s.received...)
}

// waitFor waits until the client has sent a message satisfying match, and returns it.
func (s *deviceSession) waitFor(t *testing.T, what string, match func(proto.Message) bool) proto.Message {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		for _, m := range s.messages() {
			if match(m) {
				return m
			}
		}

		select {
		case <-s.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// nextSession returns the next connection the device accepted.
func (d *fakeDevice) nextSession(t *testing.T) *deviceSession {
	t.Helper()

	select {
	case s := <-d.accepted:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the device never accepted a connection")
		return nil
	}
}
