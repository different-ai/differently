package cloudhttp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// maxEventMessage bounds one event-stream message (AWS allows 16 MiB).
const maxEventMessage = 16 << 20

// Event is one message of an AWS event stream
// (application/vnd.amazon.eventstream).
type Event struct {
	Headers map[string]string // string-typed headers, such as :event-type
	Payload []byte
}

// ReadEvents decodes an AWS event stream, calling fn for each message until
// the stream ends. Each message is: total length (4 bytes), headers length
// (4), prelude CRC32 (4), headers, payload, message CRC32 (4), big-endian.
func ReadEvents(r io.Reader, fn func(Event) error) error {
	prelude := make([]byte, 12)
	for {
		if _, err := io.ReadFull(r, prelude); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("event stream: %w", err)
		}
		total := binary.BigEndian.Uint32(prelude[0:4])
		hlen := binary.BigEndian.Uint32(prelude[4:8])
		if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
			return errors.New("event stream: prelude checksum mismatch")
		}
		if total < 16 || total > maxEventMessage || hlen > total-16 {
			return fmt.Errorf("event stream: bad message length %d", total)
		}
		msg := make([]byte, total)
		copy(msg, prelude)
		if _, err := io.ReadFull(r, msg[12:]); err != nil {
			return fmt.Errorf("event stream: %w", err)
		}
		if crc32.ChecksumIEEE(msg[:total-4]) != binary.BigEndian.Uint32(msg[total-4:]) {
			return errors.New("event stream: message checksum mismatch")
		}
		headers, err := eventHeaders(msg[12 : 12+hlen])
		if err != nil {
			return err
		}
		if err := fn(Event{Headers: headers, Payload: msg[12+hlen : total-4]}); err != nil {
			return err
		}
	}
}

// eventHeaders keeps string headers and skips the other value types.
func eventHeaders(b []byte) (map[string]string, error) {
	out := map[string]string{}
	bad := errors.New("event stream: malformed headers")
	for len(b) > 0 {
		n := int(b[0])
		if len(b) < 2+n {
			return nil, bad
		}
		name, typ := string(b[1:1+n]), b[1+n]
		b = b[2+n:]
		var size int
		switch typ {
		case 0, 1: // bool true, bool false
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8: // int64, timestamp
			size = 8
		case 9: // uuid
			size = 16
		case 6, 7: // bytes, string: 2-byte length prefix
			if len(b) < 2 {
				return nil, bad
			}
			l := int(binary.BigEndian.Uint16(b))
			if len(b) < 2+l {
				return nil, bad
			}
			if typ == 7 {
				out[name] = string(b[2 : 2+l])
			}
			b = b[2+l:]
			continue
		default:
			return nil, bad
		}
		if len(b) < size {
			return nil, bad
		}
		b = b[size:]
	}
	return out, nil
}

// EncodeEvent builds one event-stream message with string headers. The
// adapters only read streams; tests and fake servers use this to write them.
func EncodeEvent(headers map[string]string, payload []byte) []byte {
	var h []byte
	for k, v := range headers {
		h = append(h, byte(len(k)))
		h = append(h, k...)
		h = append(h, 7)
		h = binary.BigEndian.AppendUint16(h, uint16(len(v)))
		h = append(h, v...)
	}
	total := 16 + len(h) + len(payload)
	msg := binary.BigEndian.AppendUint32(nil, uint32(total))
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(h)))
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
	msg = append(msg, h...)
	msg = append(msg, payload...)
	return binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
}
