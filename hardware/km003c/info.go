package km003c

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
)

// MemoryRead addresses of the device-information blocks (64 bytes each).
const (
	addrInfo     uint32 = 0x00000420 // model, hardware/bootloader version, date
	addrFirmware uint32 = 0x00004420 // application firmware model, version, date
	addrOTP      uint32 = 0x03000C00 // serial number and factory UUID
)

// key0 encrypts MemoryRead requests and replies (AES-128-ECB). It is a fixed
// key shared by all KM002C/KM003C units, published by several
// interoperability projects.
var key0 = []byte("Lh2yfB7n6X7d9a5Z")

func encryptECB(key, src []byte) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(src)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("km003c: ECB input not block aligned (%d)", len(src))
	}
	dst := make([]byte, len(src))
	for i := 0; i < len(src); i += aes.BlockSize {
		c.Encrypt(dst[i:], src[i:])
	}
	return dst, nil
}

func decryptECB(key, src []byte) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(src)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("km003c: ECB input not block aligned (%d)", len(src))
	}
	dst := make([]byte, len(src))
	for i := 0; i < len(src); i += aes.BlockSize {
		c.Decrypt(dst[i:], src[i:])
	}
	return dst, nil
}

// memoryReadPlain builds the 32-byte plaintext of a MemoryRead request.
func memoryReadPlain(addr, length uint32) []byte {
	p := make([]byte, 32)
	binary.LittleEndian.PutUint32(p[0:], addr)
	binary.LittleEndian.PutUint32(p[4:], length)
	binary.LittleEndian.PutUint32(p[8:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(p[12:], crc32.ChecksumIEEE(p[:12]))
	for i := 16; i < 32; i++ {
		p[i] = 0xFF
	}
	return p
}

// MemoryReadRequest builds the 36-byte MemoryRead frame for one of the
// whitelisted information blocks.
func MemoryReadRequest(id uint8, addr, length uint32) ([]byte, error) {
	enc, err := encryptECB(key0, memoryReadPlain(addr, length))
	if err != nil {
		return nil, err
	}
	// Bytes 2-3 are a raw word (0x0101), not an attribute.
	frame := append([]byte{byte(TypeMemoryRead), id, 0x01, 0x01}, enc...)
	return frame, nil
}

// MemoryReadConfirm is the framed reply to a MemoryRead request.
type MemoryReadConfirm struct {
	Encrypted bool
	Addr      uint32
	Length    uint32
}

// ParseMemoryReadConfirm decodes the 20-byte confirmation (type 0x44 with the
// extend bit set, followed by the plaintext echo of the request fields).
func ParseMemoryReadConfirm(b []byte) (MemoryReadConfirm, error) {
	h, err := ParseHeader(b)
	if err != nil {
		return MemoryReadConfirm{}, err
	}
	switch h.Type {
	case TypeMemoryRead:
	case TypeReject:
		return MemoryReadConfirm{}, fmt.Errorf("km003c: MemoryRead rejected (CONNECT missing?)")
	case TypeNotReadable:
		return MemoryReadConfirm{}, fmt.Errorf("km003c: MemoryRead: address not readable")
	default:
		return MemoryReadConfirm{}, fmt.Errorf("km003c: unexpected reply %v to MemoryRead", h.Type)
	}
	if len(b) < 12 {
		return MemoryReadConfirm{}, fmt.Errorf("km003c: MemoryRead confirmation too short (%d)", len(b))
	}
	return MemoryReadConfirm{
		Encrypted: h.Word&1 != 0,
		Addr:      binary.LittleEndian.Uint32(b[4:8]),
		Length:    binary.LittleEndian.Uint32(b[8:12]),
	}, nil
}

// DecodeMemoryData turns the unframed data that follows a confirmation into
// plaintext of the requested length.
func DecodeMemoryData(c MemoryReadConfirm, data []byte) ([]byte, error) {
	if c.Encrypted {
		var err error
		if data, err = decryptECB(key0, data[:len(data)/aes.BlockSize*aes.BlockSize]); err != nil {
			return nil, err
		}
	}
	if uint32(len(data)) < c.Length {
		return nil, fmt.Errorf("km003c: MemoryRead returned %d of %d bytes", len(data), c.Length)
	}
	return data[:c.Length], nil
}

// cstr extracts a printable string from a fixed-size field terminated by NUL,
// 0xFF or padding spaces.
func cstr(b []byte) string {
	if i := bytes.IndexAny(b, "\x00\xff"); i >= 0 {
		b = b[:i]
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7E {
			return -1
		}
		return r
	}, string(b))
	return strings.TrimSpace(s)
}

// DeviceInfo is assembled from the three information blocks.
type DeviceInfo struct {
	Model           string `json:"model"`
	HardwareVersion string `json:"hardware_version"` // from the 0x420 block (hardware or bootloader version)
	HardwareDate    string `json:"hardware_date,omitempty"`
	FirmwareModel   string `json:"firmware_model,omitempty"`
	FirmwareVersion string `json:"firmware_version"`
	FirmwareDate    string `json:"firmware_date,omitempty"`
	Serial          string `json:"serial"`
	UUID            string `json:"uuid,omitempty"`
	USBSerial       string `json:"usb_serial,omitempty"` // iSerial string descriptor
}

// ParseInfoBlock decodes the 0x420 block.
func ParseInfoBlock(b []byte, d *DeviceInfo) {
	if len(b) < 0x34 {
		return
	}
	d.Model = cstr(b[0x10:0x1C])
	d.HardwareVersion = cstr(b[0x1C:0x28])
	d.HardwareDate = cstr(b[0x28:0x34])
}

// ParseFirmwareBlock decodes the 0x4420 block.
func ParseFirmwareBlock(b []byte, d *DeviceInfo) {
	if len(b) < 0x34 {
		return
	}
	if binary.LittleEndian.Uint32(b[0:4]) == 0xFFFFFFFF {
		return // no application firmware descriptor
	}
	d.FirmwareModel = cstr(b[0x10:0x1C])
	d.FirmwareVersion = cstr(b[0x1C:0x28])
	d.FirmwareDate = cstr(b[0x28:0x34])
}

// ParseOTPBlock decodes the serial number (first token) and the factory UUID
// from the 0x03000C00 block.
func ParseOTPBlock(b []byte, d *DeviceInfo) {
	if len(b) == 0 {
		return
	}
	end := bytes.IndexAny(b, " \x00\xff")
	if end < 0 {
		end = len(b)
	}
	d.Serial = cstr(b[:end])
	if len(b) >= 0x27 {
		u := cstr(b[0x07:0x27])
		if isHex(u) && len(u) == 32 {
			d.UUID = u
		}
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return s != ""
}
