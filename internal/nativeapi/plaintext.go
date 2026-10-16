package nativeapi

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// maxPlaintextFrame bounds how much a device can make us allocate for one message by what it claims the length
// to be. Real messages are far smaller: even a device with hundreds of entities describes each in its own.
const maxPlaintextFrame = 1 << 20

// codec reads and writes whole messages, as a type id and a serialized payload, in whichever framing the
// connection uses. Reads are only ever done from one goroutine, and writes are serialized by the caller.
type codec interface {
	writeMessage(id uint32, payload []byte) error
	readMessage() (id uint32, payload []byte, err error)
}

// plaintextCodec is the unencrypted framing: a zero byte, the payload's length and the message's type id as
// varints, then the payload.
type plaintextCodec struct {
	reader *bufio.Reader
	writer io.Writer
}

func (c *plaintextCodec) writeMessage(id uint32, payload []byte) error {
	frame := make([]byte, 0, 1+2*binary.MaxVarintLen32+len(payload))
	frame = append(frame, 0x00)
	frame = binary.AppendUvarint(frame, uint64(len(payload)))
	frame = binary.AppendUvarint(frame, uint64(id))
	frame = append(frame, payload...)

	_, err := c.writer.Write(frame)

	return err
}

func (c *plaintextCodec) readMessage() (uint32, []byte, error) {
	preamble, err := c.reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}

	switch preamble {
	case 0x00:
	case 0x01:
		// The device is speaking the encrypted framing to a client that isn't.
		return 0, nil, ErrEncryptionRequired
	default:
		return 0, nil, fmt.Errorf("%w: a frame starts with %#x", ErrProtocol, preamble)
	}

	length, err := binary.ReadUvarint(c.reader)
	if err != nil {
		return 0, nil, err
	}

	id, err := binary.ReadUvarint(c.reader)
	if err != nil {
		return 0, nil, err
	}

	if length > maxPlaintextFrame {
		return 0, nil, fmt.Errorf("%w: a frame claims to be %d bytes long", ErrProtocol, length)
	}

	if id > 0xFFFFFFFF {
		return 0, nil, fmt.Errorf("%w: a frame has message type %d", ErrProtocol, id)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, nil, err
	}

	return uint32(id), payload, nil
}
