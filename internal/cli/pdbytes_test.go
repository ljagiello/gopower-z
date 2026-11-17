package cli

import (
	"encoding/binary"
	"testing"

	"github.com/ljagiello/gopower-z/hardware/km003c"
)

// PD_PACKET payloads built byte by byte the way the meter frames them, so the
// tests go through the real parser (km003c.ParsePDPacket) instead of
// hand-made records.

var le = binary.LittleEndian

// USB-PD message headers (spec revision 3.x). Source messages carry power
// role source and data role DFP; sink messages sink/UFP.
const (
	hdrSrcCaps   = 0x01A1 // Source_Capabilities, add n<<12 for n objects
	hdrRequest   = 0x1082 // Request, 1 object
	hdrEPRReq    = 0x2089 // EPR_Request, 2 objects
	hdrAccept    = 0x01A3
	hdrReject    = 0x01A4
	hdrPSRdy     = 0x01A6
	hdrWait      = 0x01AC
	hdrSoftReset = 0x01AD
	hdrGoodCRC   = 0x0041
)

// PDOs and RDOs used across tests.
const (
	pdoFixed5V3A  = 0x0001912C
	pdoFixed9V3A  = 0x0002D12C
	pdoPPS3311V3A = 0xC0000000 | 110<<17 | 33<<8 | 60           // PPS 3.3-11 V, 3 A
	pdoEPRAVS     = 0xC0000000 | 1<<28 | 480<<17 | 150<<8 | 140 // EPR AVS 15-48 V, 140 W
	rdoFixed2     = 2<<28 | 300<<10 | 300                       // position 2, 3 A / 3 A
	rdoPPS3       = 3<<28 | 450<<9 | 40                         // position 3, 9 V, 2 A
	rdoAVS8       = 8<<28 | 1<<22 | 1120<<9 | 60                // position 8, EPR, 28 V, 3 A
)

func pdEvent(ts uint32, code byte) []byte {
	return append(le.AppendUint32([]byte{0x45}, ts), code)
}

func pdMsg(ts uint32, sop byte, header uint16, objs ...uint32) []byte {
	n := 1 + 4 + 1 + 2 + 4*len(objs)
	b := le.AppendUint32([]byte{0x80 | byte(n-1)}, ts)
	b = le.AppendUint16(append(b, sop), header)
	for _, o := range objs {
		b = le.AppendUint32(b, o)
	}
	return b
}

// pdLong is a "long" message record (layout unverified; kept raw).
func pdLong(ts uint32, data []byte) []byte {
	b := le.AppendUint32([]byte{0x27}, ts)
	b = le.AppendUint16(append(b, 0), uint16(len(data)))
	return append(b, data...)
}

// pdPayload is a PD_PACKET section payload: status block plus records.
func pdPayload(records ...[]byte) []byte {
	b := make([]byte, 12)
	le.PutUint32(b, 10_000)
	le.PutUint16(b[4:], 5000)
	for _, r := range records {
		b = append(b, r...)
	}
	return b
}

// negotiation is a complete SPR negotiation: attach, capabilities, Request,
// Accept, PS_RDY.
func negotiation() []byte {
	return pdPayload(
		pdEvent(9_000, 0x21), // attach on CC2
		pdMsg(9_100, 0, hdrSrcCaps|3<<12, pdoFixed5V3A, pdoFixed9V3A, pdoPPS3311V3A),
		pdMsg(9_110, 0, hdrRequest, rdoFixed2),
		pdMsg(9_120, 0, hdrAccept),
		pdMsg(9_200, 0, hdrPSRdy),
	)
}

func parsePD(t testing.TB, payload []byte) km003c.PDPacket {
	t.Helper()
	p, err := km003c.ParsePDPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
