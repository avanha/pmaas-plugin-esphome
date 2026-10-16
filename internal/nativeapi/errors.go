package nativeapi

import (
	"errors"
	"fmt"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

var (
	// ErrEncryptionRequired means the device wants an encrypted connection and no key was configured.
	ErrEncryptionRequired = errors.New("the device requires encryption, and no key is configured for it")

	// ErrDeviceNotEncrypted means a key is configured but the device isn't using encryption. A device must
	// have an api: encryption: key set in its firmware to use one.
	ErrDeviceNotEncrypted = errors.New("a key is configured, but the device isn't using encryption")

	// ErrInvalidKey means the device rejected the handshake because the key isn't its key.
	ErrInvalidKey = errors.New("the device doesn't accept the encryption key")

	// ErrHandshake is a handshake that failed for some other reason.
	ErrHandshake = errors.New("the encryption handshake failed")

	// ErrProtocol is the device sending something that isn't the protocol.
	ErrProtocol = errors.New("protocol error")

	// ErrKeepAliveTimeout is a connection that has gone quiet: the device hasn't sent anything, not even a reply
	// to a ping, for long enough that it's taken to be gone.
	ErrKeepAliveTimeout = errors.New("the device stopped responding")

	// ErrClosed is the reason a connection ended when it was closed by the caller.
	ErrClosed = errors.New("the connection was closed")

	// ErrIncompatibleVersion is a device whose API is a major version this client doesn't speak.
	ErrIncompatibleVersion = errors.New("the device's API version isn't supported")
)

// WrongDeviceError is the device at an address not being the one expected there, as when addresses are handed
// out afresh and another device has the old one.
type WrongDeviceError struct {
	Expected string
	Got      string
}

func (e *WrongDeviceError) Error() string {
	return fmt.Sprintf("the device at this address is %q, not %q", e.Got, e.Expected)
}

// DeviceDisconnectedError is the device ending the connection itself, which it does when it's about to reboot
// or run out of room for clients. Reason is what it said, if it said.
type DeviceDisconnectedError struct {
	Reason pb.DisconnectReason
}

func (e *DeviceDisconnectedError) Error() string {
	if e.Reason == pb.DisconnectReason_DISCONNECT_REASON_UNSPECIFIED {
		return "the device closed the connection"
	}

	return fmt.Sprintf("the device closed the connection: %s", e.Reason)
}
