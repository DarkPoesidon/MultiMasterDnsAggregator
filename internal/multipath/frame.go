// ==============================================================================
// MasterDnsVPN – Multipath Overlay Layer
// Package:  masterdns_aggregator/multipath
// Purpose:  MacroFrame wire format – encoding, decoding, and construction.
//
// Every chunk of data that crosses a bearer tunnel is wrapped in a MacroFrame.
// The 21-byte header carries enough information for the remote Aggregator to
// reassemble independent streams even when chunks arrive out-of-order or on
// different physical bearer connections.
//
// Wire layout (big-endian, 21 bytes fixed):
//   [0..3]   Magic      uint32  = 0x4D50564E ("MPVN")
//   [4]      Version    uint8   = 1
//   [5]      Flags      uint8   bit0=SYN  bit1=FIN  bit2=RST  bit3=ACK
//   [6..9]   StreamID   uint32  logical connection identifier
//   [10..17] GlobalSeq  uint64  byte-offset of this chunk in the logical stream
//   [18..19] PayloadLen uint16  byte count of the payload following this header
//   [20]     Checksum   uint8   XOR of bytes [0..19]
// ==============================================================================

package multipath

import (
	"encoding/binary"
	"errors"
	"io"
)

// ──────────────────────────────────────────────────────────────────────────────
// Errors
// ──────────────────────────────────────────────────────────────────────────────

var (
	// ErrInvalidMagic is returned when the first 4 bytes of a frame are not
	// the expected MacroMagic sentinel.
	ErrInvalidMagic = errors.New("multipath: invalid macro frame magic")

	// ErrInvalidVersion is returned when the Version field is not MacroVersion.
	ErrInvalidVersion = errors.New("multipath: unsupported macro frame version")

	// ErrInvalidChecksum is returned when the XOR checksum over header bytes
	// [0..19] does not equal byte [20].
	ErrInvalidChecksum = errors.New("multipath: macro frame header checksum mismatch")
)

// ──────────────────────────────────────────────────────────────────────────────
// MacroFrameHeader
// ──────────────────────────────────────────────────────────────────────────────

// MacroFrameHeader is the decoded representation of the 21-byte fixed header.
type MacroFrameHeader struct {
	Magic      uint32
	Version    uint8
	Flags      uint8
	StreamID   uint32
	GlobalSeq  uint64
	PayloadLen uint16
}

// HasFlag reports whether the given flag bit is set.
func (h MacroFrameHeader) HasFlag(flag uint8) bool {
	return h.Flags&flag != 0
}

// Encode serialises the header into a fixed-size array.
// The checksum byte is computed automatically.
func (h MacroFrameHeader) Encode() [MacroFrameHeaderSize]byte {
	var buf [MacroFrameHeaderSize]byte
	binary.BigEndian.PutUint32(buf[0:4], h.Magic)
	buf[4] = h.Version
	buf[5] = h.Flags
	binary.BigEndian.PutUint32(buf[6:10], h.StreamID)
	binary.BigEndian.PutUint64(buf[10:18], h.GlobalSeq)
	binary.BigEndian.PutUint16(buf[18:20], h.PayloadLen)

	var cs uint8
	for i := 0; i < 20; i++ {
		cs ^= buf[i]
	}
	buf[20] = cs
	return buf
}

// ──────────────────────────────────────────────────────────────────────────────
// Decoding
// ──────────────────────────────────────────────────────────────────────────────

// DecodeFrameHeader reads exactly MacroFrameHeaderSize bytes from r,
// validates the magic sentinel, version, and XOR checksum, then returns the
// decoded header.  Callers must subsequently read Header.PayloadLen bytes to
// obtain the frame payload.
func DecodeFrameHeader(r io.Reader) (MacroFrameHeader, error) {
	var raw [MacroFrameHeaderSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return MacroFrameHeader{}, err
	}

	magic := binary.BigEndian.Uint32(raw[0:4])
	if magic != MacroMagic {
		return MacroFrameHeader{}, ErrInvalidMagic
	}
	if raw[4] != MacroVersion {
		return MacroFrameHeader{}, ErrInvalidVersion
	}

	// Validate XOR checksum over bytes 0..19.
	var cs uint8
	for i := 0; i < 20; i++ {
		cs ^= raw[i]
	}
	if cs != raw[20] {
		return MacroFrameHeader{}, ErrInvalidChecksum
	}

	return MacroFrameHeader{
		Magic:      magic,
		Version:    raw[4],
		Flags:      raw[5],
		StreamID:   binary.BigEndian.Uint32(raw[6:10]),
		GlobalSeq:  binary.BigEndian.Uint64(raw[10:18]),
		PayloadLen: binary.BigEndian.Uint16(raw[18:20]),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Frame construction helpers
// ──────────────────────────────────────────────────────────────────────────────

// BuildFrame constructs a complete macro frame (header + payload) as a single
// contiguous []byte ready to be written to a bearer connection.
//
// Parameters:
//   - streamID  : logical connection identifier
//   - globalSeq : byte-offset of the first payload byte within the stream
//   - flags     : combination of FlagSYN / FlagFIN / FlagRST / FlagACK
//   - payload   : actual data bytes (may be empty for SYN/FIN/RST frames)
func BuildFrame(streamID uint32, globalSeq uint64, flags uint8, payload []byte) []byte {
	h := MacroFrameHeader{
		Magic:      MacroMagic,
		Version:    MacroVersion,
		Flags:      flags,
		StreamID:   streamID,
		GlobalSeq:  globalSeq,
		PayloadLen: uint16(len(payload)),
	}
	hdr := h.Encode()

	frame := make([]byte, MacroFrameHeaderSize+len(payload))
	copy(frame[:MacroFrameHeaderSize], hdr[:])
	if len(payload) > 0 {
		copy(frame[MacroFrameHeaderSize:], payload)
	}
	return frame
}

// BuildSYNFrame returns a zero-payload SYN frame for stream streamID.
func BuildSYNFrame(streamID uint32) []byte {
	return BuildFrame(streamID, 0, FlagSYN, nil)
}

// BuildFINFrame returns a zero-payload FIN frame for stream streamID.
// globalSeq should be the total byte count already sent on this stream.
func BuildFINFrame(streamID uint32, globalSeq uint64) []byte {
	return BuildFrame(streamID, globalSeq, FlagFIN, nil)
}

// BuildRSTFrame returns a zero-payload RST frame for stream streamID.
func BuildRSTFrame(streamID uint32) []byte {
	return BuildFrame(streamID, 0, FlagRST, nil)
}

// ──────────────────────────────────────────────────────────────────────────────
// Option-2 SYN target encoding
// ──────────────────────────────────────────────────────────────────────────────

// EncodeTarget encodes a "host:port" address string into the SYN frame payload
// format used by the multipath overlay (Option 2: SYN carries target).
//
// Wire format:
//
//	[0..1]  uint16 big-endian: byte length of the address string
//	[2..N]  UTF-8 bytes of "host:port"
//
// The server-side counterpart is aggregator.DecodeTarget().
func EncodeTarget(addr string) []byte {
	b := []byte(addr)
	out := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(b)))
	copy(out[2:], b)
	return out
}
