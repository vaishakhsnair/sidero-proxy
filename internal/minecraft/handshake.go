package minecraft

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

type Handshake struct {
	Hostname string
	Port     int
}

func ReadHandshakePacket(r io.Reader) ([]byte, Handshake, error) {
	packetLength, packetLengthBytes, err := readVarInt(r)
	if err != nil {
		return nil, Handshake{}, fmt.Errorf("read packet length: %w", err)
	}
	if packetLength <= 0 {
		return nil, Handshake{}, fmt.Errorf("invalid packet length %d", packetLength)
	}

	packet := make([]byte, packetLength)
	if _, err := io.ReadFull(r, packet); err != nil {
		return nil, Handshake{}, fmt.Errorf("read packet body: %w", err)
	}

	handshake, err := parseHandshake(packet)
	if err != nil {
		return nil, Handshake{}, err
	}

	raw := append(packetLengthBytes, packet...)
	return raw, handshake, nil
}

func parseHandshake(packet []byte) (Handshake, error) {
	var cursor int

	packetID, err := parseVarInt(packet, &cursor)
	if err != nil {
		return Handshake{}, fmt.Errorf("parse packet id: %w", err)
	}
	if packetID != 0 {
		return Handshake{}, fmt.Errorf("unexpected packet id %d", packetID)
	}

	if _, err := parseVarInt(packet, &cursor); err != nil {
		return Handshake{}, fmt.Errorf("parse protocol version: %w", err)
	}

	host, err := parseString(packet, &cursor)
	if err != nil {
		return Handshake{}, fmt.Errorf("parse hostname: %w", err)
	}
	if cursor+2 > len(packet) {
		return Handshake{}, fmt.Errorf("parse port: short packet")
	}
	port := int(binary.BigEndian.Uint16(packet[cursor : cursor+2]))
	cursor += 2

	if _, err := parseVarInt(packet, &cursor); err != nil {
		return Handshake{}, fmt.Errorf("parse next state: %w", err)
	}

	return Handshake{
		Hostname: NormalizeHostname(host),
		Port:     port,
	}, nil
}

func NormalizeHostname(host string) string {
	if idx := strings.IndexByte(host, 0); idx >= 0 {
		host = host[:idx]
	}
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

func readVarInt(r io.Reader) (int, []byte, error) {
	var (
		numRead int
		result  int
		raw     []byte
	)
	for {
		if numRead >= 5 {
			return 0, nil, fmt.Errorf("varint too long")
		}
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		raw = append(raw, b[0])
		value := int(b[0] & 0x7F)
		result |= value << (7 * numRead)

		numRead++
		if b[0]&0x80 == 0 {
			break
		}
	}
	return result, raw, nil
}

func parseVarInt(packet []byte, cursor *int) (int, error) {
	var (
		numRead int
		result  int
	)
	for {
		if *cursor >= len(packet) {
			return 0, io.ErrUnexpectedEOF
		}
		if numRead >= 5 {
			return 0, fmt.Errorf("varint too long")
		}
		b := packet[*cursor]
		*cursor = *cursor + 1
		value := int(b & 0x7F)
		result |= value << (7 * numRead)
		numRead++
		if b&0x80 == 0 {
			return result, nil
		}
	}
}

func parseString(packet []byte, cursor *int) (string, error) {
	size, err := parseVarInt(packet, cursor)
	if err != nil {
		return "", err
	}
	if size < 0 {
		return "", fmt.Errorf("negative string size")
	}
	if *cursor+size > len(packet) {
		return "", io.ErrUnexpectedEOF
	}
	value := string(packet[*cursor : *cursor+size])
	*cursor += size
	return value, nil
}
