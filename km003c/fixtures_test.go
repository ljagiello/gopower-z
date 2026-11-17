package km003c

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Frames captured from a real KM003C (application firmware 1.9.5, hardware
// 2.2) with nothing attached; see testdata/fw1.9.5/INDEX.txt.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fw1.9.5", name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return b
}

func TestFixtureRequestsMatchEncoder(t *testing.T) {
	cases := []struct {
		file string
		want []byte
	}{
		{"bulk_W1_1_req", ADCRequest(1)},
		{"bulk_W3_1_req", PDRequest(7)},
		{"bulk_W4_1_req", ADCAndPDRequest(10)},
		{"cdc_W1_1_req", ADCRequest(18)},
	}
	for _, c := range cases {
		if got := fixture(t, c.file); !bytes.Equal(got, c.want) {
			t.Errorf("%s: captured % X, encoder % X", c.file, got, c.want)
		}
	}
	set := ControlHeader(TypeGetData, 6, AttSettings)
	if got := fixture(t, "bulk_W2_1_req"); !bytes.Equal(got, set[:]) {
		t.Errorf("settings request % X vs % X", got, set)
	}
	con := ControlHeader(TypeConnect, 11, 0)
	if got := fixture(t, "bulk_W5_1_req"); !bytes.Equal(got, con[:]) {
		t.Errorf("connect request % X vs % X", got, con)
	}
	// HID buffers carry hidapi's leading report-ID byte.
	hid := fixture(t, "hid_W1_1_req")
	if len(hid) != 65 || hid[0] != 0 || !bytes.Equal(hid[1:5], ADCRequest(15)) {
		t.Errorf("hid request % X", hid)
	}
}

func TestFixtureADC(t *testing.T) {
	files := []string{
		"bulk_W1_1_resp", "bulk_W1_2_resp", "bulk_W1_3_resp", "bulk_W1_4_resp", "bulk_W1_5_resp",
		"hid_W1_1_resp", "hid_W1_2_resp", "hid_W1_3_resp",
		"cdc_W1_1_resp", "cdc_W1_2_resp", "cdc_W1_3_resp",
	}
	for _, f := range files {
		frame := fixture(t, f)
		req := fixture(t, strings.Replace(f, "_resp", "_req", 1))
		if len(req) == 65 { // hidapi buffer: drop the report-ID byte
			req = req[1:5]
		}
		if frame[1] != req[1] {
			t.Errorf("%s: reply id %d != request id %d", f, frame[1], req[1])
		}
		if err := CheckRequest(req, HID); err != nil {
			t.Errorf("%s: captured request refused by guard: %v", f, err)
		}
		a, err := ParseADCFrame(frame)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// Screen at capture time: VBUS ≈ 5 mV, IBUS 0, ~27 °C,
		// D+ 0.086, D- 0.090, CC1 0.068, CC2 0.027.
		within := func(name string, v, lo, hi float64) {
			if v < lo || v > hi {
				t.Errorf("%s: %s = %v, want [%v, %v]", f, name, v, lo, hi)
			}
		}
		within("VBUS", a.VBus(), 0.004, 0.007)
		within("IBUS", a.IBus(), -0.0002, 0.0002)
		within("temp", a.TempC(), 25, 28)
		within("D+", a.DPV(), 0.084, 0.088)
		within("D-", a.DMV(), 0.088, 0.092)
		within("CC1", a.CC1V(), 0.066, 0.070)
		within("CC2", a.CC2V(), 0.026, 0.029)
		within("VDD", a.VDDV(), 3.1, 3.3)
		if !a.HasLineAvg {
			t.Fatalf("%s: no line averages", f)
		}
		within("CC2 avg", float64(a.CC2AvgMV)/1000, 0.024, 0.030)
		within("D+ avg", float64(a.DPAvgMV)/1000, 0.083, 0.088)
		within("D- avg", float64(a.DMAvgMV)/1000, 0.087, 0.092)
	}
}

func TestFixtureSettings(t *testing.T) {
	_, secs, err := ParsePutData(fixture(t, "bulk_W2_1_resp"))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := FindSection(secs, AttSettings)
	if !ok {
		t.Fatal("no settings section")
	}
	set, err := ParseSettings(s.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !set.CRCAOK || !set.CRCBOK {
		t.Errorf("CRC mismatch: %v %v", set.CRCAOK, set.CRCBOK)
	}
	if set.SampleRate.SamplesPerSecond() != 50 || set.DeviceName != "POWER-Z" {
		t.Errorf("settings: rate %v name %q", set.SampleRate, set.DeviceName)
	}
}

func TestFixturePD(t *testing.T) {
	for _, f := range []string{"bulk_W3_1_resp", "bulk_W3_2_resp", "bulk_W3_3_resp"} {
		_, secs, err := ParsePutData(fixture(t, f))
		if err != nil {
			t.Fatal(err)
		}
		s, ok := FindSection(secs, AttPDPacket)
		if !ok {
			t.Fatalf("%s: no PD section", f)
		}
		pd, err := ParsePDPacket(s.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(pd.Records) != 0 || pd.Status.VBusMV > 10 || pd.Status.TimestampMS == 0 {
			t.Errorf("%s: %+v", f, pd)
		}
	}
	// ADC + PD in one reply: two chained sections.
	_, secs, err := ParsePutData(fixture(t, "bulk_W4_1_resp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 2 || secs[0].Att != AttADC || !secs[0].Next || secs[1].Att != AttPDPacket || len(secs[1].Payload) != 12 {
		t.Fatalf("sections %+v", secs)
	}
}

func TestFixtureMemoryRead(t *testing.T) {
	cases := []struct {
		n    string
		id   uint8
		addr uint32
	}{
		{"1", 12, addrInfo},
		{"2", 13, addrFirmware},
		{"3", 14, addrOTP},
	}
	var info DeviceInfo
	for _, c := range cases {
		req, err := MemoryReadRequest(c.id, c.addr, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := fixture(t, "bulk_W6_"+c.n+"_req"); !bytes.Equal(got, req) {
			t.Errorf("W6_%s: captured % X\n encoder % X", c.n, got, req)
		}
		conf, err := ParseMemoryReadConfirm(fixture(t, "bulk_W6_"+c.n+"_resp"))
		if err != nil {
			t.Fatal(err)
		}
		if conf.Addr != c.addr || conf.Length != 64 || !conf.Encrypted {
			t.Errorf("confirm %+v", conf)
		}
		plain, err := DecodeMemoryData(conf, fixture(t, "bulk_W6_"+c.n+"_respdata"))
		if err != nil {
			t.Fatal(err)
		}
		if want := fixture(t, "bulk_W6_"+c.n+"_plain"); !bytes.Equal(plain, want) {
			t.Errorf("W6_%s plaintext mismatch", c.n)
		}
		switch c.addr {
		case addrInfo:
			ParseInfoBlock(plain, &info)
		case addrFirmware:
			ParseFirmwareBlock(plain, &info)
		case addrOTP:
			ParseOTPBlock(plain, &info)
		}
	}
	want := DeviceInfo{
		Model: "KM003C", HardwareVersion: "2.2", HardwareDate: "2024.9.23",
		FirmwareModel: "KM003C", FirmwareVersion: "1.9.5", FirmwareDate: "2025.1.7",
		Serial: "050002", UUID: "0123456789ABCDEF0123456789ABCDEF",
	}
	if info != want {
		t.Errorf("info\n got %+v\nwant %+v", info, want)
	}
}

func TestFixtureConnect(t *testing.T) {
	h, err := ParseHeader(fixture(t, "bulk_W5_1_resp"))
	if err != nil || h.Type != TypeAccept || h.ID != 11 {
		t.Errorf("connect reply %+v %v", h, err)
	}
}
