package km003c

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// SampleRate is the on-device sample-rate code.
type SampleRate uint8

// SamplesPerSecond converts the code to samples per second.
func (r SampleRate) SamplesPerSecond() int {
	switch r {
	case 0:
		return 2
	case 1:
		return 10
	case 2:
		return 50
	case 3:
		return 1000
	}
	return 0
}

func (r SampleRate) String() string {
	if sps := r.SamplesPerSecond(); sps > 0 {
		return fmt.Sprintf("%d SPS", sps)
	}
	return fmt.Sprintf("rate(%d)", uint8(r))
}

// Settings is the decoded SETTINGS block (attribute 0x008, 180 bytes). The
// field meanings come from community reverse engineering; Raw keeps the
// original bytes.
type Settings struct {
	Flags        uint32 `json:"flags"`
	Language     int    `json:"language"` // bit 0
	Brightness   int    `json:"brightness_percent"`
	DimTimeout   int    `json:"dim_timeout_code"`
	Theme        int    `json:"theme"`
	AutoRotate   bool   `json:"auto_rotate"`
	DisplayFlags uint32 `json:"display_flags"`
	Rotation     int    `json:"rotation"`
	SampleRate   SampleRate
	DeviceName   string `json:"device_name"`
	CRCAOK       bool   `json:"crc_a_ok"`
	CRCBOK       bool   `json:"crc_b_ok"`
	Raw          []byte `json:"-"`
}

// ParseSettings decodes a SETTINGS section payload.
func ParseSettings(p []byte) (Settings, error) {
	if len(p) < 0xB4 {
		return Settings{}, fmt.Errorf("km003c: SETTINGS payload too short (%d bytes)", len(p))
	}
	le := binary.LittleEndian
	f := le.Uint32(p[0x00:])
	g := le.Uint32(p[0x60:])
	s := Settings{
		Flags:        f,
		Language:     int(f & 1),
		Brightness:   int(f>>3) & 0x7F,
		DimTimeout:   int(f>>10) & 3,
		Theme:        int(f>>16) & 7,
		AutoRotate:   f&(1<<27) != 0,
		DisplayFlags: g,
		Rotation:     int(g & 3),
		SampleRate:   SampleRate((g >> 2) & 3),
		DeviceName:   cstr(p[0x70:0xB0]),
		CRCAOK:       crc32.ChecksumIEEE(p[0x00:0x5C]) == le.Uint32(p[0x5C:]),
		CRCBOK:       crc32.ChecksumIEEE(p[0x60:0xB0]) == le.Uint32(p[0xB0:]),
		Raw:          append([]byte(nil), p...),
	}
	return s, nil
}
