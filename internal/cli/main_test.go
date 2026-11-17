package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/spf13/pflag"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err       error
		code      string
		exit      int
		retryable bool
	}{
		{fmt.Errorf("x: %w", km003c.ErrNotFound), "not_found", exitNotFound, false},
		{fmt.Errorf("x: %w", km003c.ErrBusy), "busy", exitBusy, true},
		{fmt.Errorf("x: %w", km003c.ErrTimeout), "timeout", exitTimeout, true},
		{fmt.Errorf("x: %w", km003c.ErrPermission), "permission_denied", exitPermission, false},
		{fmt.Errorf("x: %w", km003c.ErrRejected), "error", exitError, false},
		{fmt.Errorf("x: %w", km003c.ErrStuck), "error", exitError, false},
		{context.Canceled, "interrupted", exitInterrupt, false},
		{partialErr{"half"}, "partial", exitPartial, true},
		{usageErr("bad"), "usage", exitUsage, false},
		{errors.New("boom"), "error", exitError, false},
	}
	for _, c := range cases {
		ce := classify(c.err)
		if ce.Code != c.code || ce.exit != c.exit || ce.Retryable != c.retryable || ce.Hint == "" {
			t.Errorf("classify(%v) = %+v, want %s/%d/%v with a hint", c.err, ce, c.code, c.exit, c.retryable)
		}
	}
	if h := classify(km003c.ErrStuck).Hint; !strings.Contains(h, "unplug") {
		t.Errorf("stuck hint %q does not say to reconnect the meter", h)
	}
}

// noMeterEnv fails the test if anything tries to open the meter.
func noMeterEnv(t *testing.T) (*env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &env{stdout: &out, stderr: &errb,
		openUSB: func(string) (km003c.Transport, km003c.USBDescription, error) {
			t.Error("usage error path opened the meter")
			return nil, km003c.USBDescription{}, km003c.ErrNotFound
		},
		listUSB: func() ([]km003c.USBDescription, error) {
			t.Error("usage error path listed meters")
			return nil, nil
		},
	}, &out, &errb
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"frobnicate"},
		{"read", "--bogus"},
		{"read", "extra-arg"},
		{"measure", "--duration", "0s"},
		{"measure", "--interval", "5ms"},
		{"log", "--format", "xml"},
		{"log", "--count", "-1"},
		{"pd", "--interval", "0s"},
		{"read", "--timeout", "50ms"},
		{"read", "--wait", "-1s"},
		{"measure", "--duration", "banana"},
		{"measure", "-duration", "1s"}, // single-dash long flag
		{"watch"},                      // not a terminal
	} {
		e, out, errb := noMeterEnv(t)
		if got := run(context.Background(), e, append([]string{"--json"}, args...)); got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, got, exitUsage)
		}
		if out.Len() != 0 || !strings.Contains(errb.String(), `"code":"usage"`) {
			t.Errorf("run(%v): stdout %q stderr %q", args, out, errb)
		}
	}
}

func TestUsageHints(t *testing.T) {
	hint := func(args ...string) string {
		t.Helper()
		e, _, errb := noMeterEnv(t)
		if code := run(context.Background(), e, append([]string{"--json"}, args...)); code != exitUsage {
			t.Fatalf("%v: exit %d", args, code)
		}
		var v struct {
			Error cliError `json:"error"`
		}
		if err := json.Unmarshal(errb.Bytes(), &v); err != nil {
			t.Fatalf("%v: stderr %q", args, errb.String())
		}
		return v.Error.Hint
	}
	if h := hint("mesure"); !strings.Contains(h, "did you mean `powerz measure`") {
		t.Errorf("unknown command hint %q", h)
	}
	if h := hint("zzzzzzzz"); strings.Contains(h, "did you mean") || !strings.Contains(h, "measure") {
		t.Errorf("no-suggestion hint %q", h)
	}
	if h := hint("measure", "-duration", "1s"); !strings.Contains(h, "two dashes") || !strings.Contains(h, "--duration") {
		t.Errorf("single-dash hint %q", h)
	}
	if h := hint("measure", "--duration", "banana"); !strings.Contains(h, "durations look like") {
		t.Errorf("bad duration hint %q", h)
	}
}

// --help prints cobra's help on stdout and exits 0 without touching the
// meter; --version prints the version.
func TestHelpAndVersionFlags(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"measure", "--help"}, {"--version"}, {"help", "pd"}} {
		e, out, _ := noMeterEnv(t)
		if code := run(context.Background(), e, args); code != exitOK || out.Len() == 0 {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
}

// Flag help must not contain backquotes: Go's flag package turns
// `quoted` text into the argument name shown by -h.
func TestFlagUsageHasNoBackquotes(t *testing.T) {
	root := newRootCmd(&globals{env: &env{}})
	for _, c := range append(root.Commands(), root) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s --%s usage contains a backquote: %q", c.Name(), f.Name, f.Usage)
			}
		})
	}
}

func loadADC(t testing.TB) km003c.ADC {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "hardware", "km003c", "testdata", "fw1.9.5", "bulk_W1_1_resp.hex"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatal(err)
	}
	a, err := km003c.ParseADCFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSnapshotValues(t *testing.T) {
	a := loadADC(t)
	a.ApplyTail(km003c.TailLayoutFor("1.9.5"))
	s := toSnapshot(a, time.Unix(0, 0), "050002", "1.9.5", nil)
	if s.VBusV < 0.004 || s.VBusV > 0.007 || s.TempC == nil || s.Temp2C != nil {
		t.Errorf("values: %+v", s)
	}
	if s.SampleRateSPS == nil || *s.SampleRateSPS != 50 || s.CC2AvgV == nil || s.Raw.TailLayout != "line-averages" {
		t.Errorf("firmware-dependent fields: %+v", s)
	}
	if s.Inferred.DashboardLabel != nil { // "[ N/A ]" on screen -> null
		t.Errorf("dashboard label %q, want null", *s.Inferred.DashboardLabel)
	}
	if s.Inferred.DashboardClassifier == nil || *s.Inferred.DashboardClassifier != "1.9.5" {
		t.Errorf("classifier %v, want 1.9.5", s.Inferred.DashboardClassifier)
	}
	if s.Time != "1970-01-01T00:00:00.000Z" {
		t.Errorf("time %q, want UTC with milliseconds", s.Time)
	}
	if s.Direction != "none" || s.Inferred.CC.Attached != "none" {
		t.Errorf("idle direction/cc: %s %+v", s.Direction, s.Inferred.CC)
	}
	// Unknown firmware: firmware-dependent fields and labels are null, and
	// the classifier says the labels are unknown rather than [N/A].
	a.ApplyTail(km003c.TailUnknown)
	u := toSnapshot(a, time.Unix(0, 0), "050002", "", new("read block 0x00004420: timeout"))
	if u.FirmwareVersion != nil || u.FirmwareError == nil || u.SampleRateSPS != nil || u.CC2AvgV != nil ||
		u.Inferred.DashboardClassifier != nil || u.Inferred.DashboardLabel2 != nil || u.Raw.TailLayout != "unknown" {
		t.Errorf("unknown-firmware snapshot %+v", u)
	}
}

func TestIntegrator(t *testing.T) {
	var in integrator
	t0 := time.Unix(0, 0)
	for i := range 10 { // 2 W for 10 samples, 100 ms apart
		in.add(t0.Add(time.Duration(i)*100*time.Millisecond), 2, 0.4)
	}
	wh, ah, s := in.at(t0.Add(time.Second))
	if s != 1 || wh != 2.0/3600 || ah != 0.4/3600 {
		t.Errorf("integrated %v s, %v Wh, %v Ah; want 1 s, %v Wh", s, wh, ah, 2.0/3600)
	}
	// at does not consume: asking again gives the same totals.
	if wh2, _, _ := in.at(t0.Add(time.Second)); wh2 != wh {
		t.Errorf("at changed the integrator: %v then %v", wh, wh2)
	}
	// A failed sample in between holds the previous value (no gap dropped).
	var gap integrator
	gap.add(t0, 1, 0.2)
	gap.add(t0.Add(10*time.Second), 1, 0.2)
	if wh, _, _ := gap.at(t0.Add(10 * time.Second)); wh != 10.0/3600 {
		t.Errorf("gap integrated to %v Wh, want %v", wh, 10.0/3600)
	}
}

func TestISODate(t *testing.T) {
	for in, want := range map[string]string{"2025.1.7": "2025-01-07", "2024.9.23": "2024-09-23", "bogus": "bogus"} {
		if got := isoDate(in); got != want {
			t.Errorf("isoDate(%q) = %q, want %q", in, got, want)
		}
	}
}

// feed runs one PD_PACKET payload through a tracker.
func feed(t *testing.T, tr *pdTracker, records ...[]byte) []pdRecord {
	t.Helper()
	p := parsePD(t, pdPayload(records...))
	return tr.packet(&p)
}

func TestPDTrackerNegotiation(t *testing.T) {
	tr := &pdTracker{}
	p := parsePD(t, negotiation())
	recs := tr.packet(&p)
	if len(recs) != 5 || recs[1].SourceCaps == nil || recs[1].SourceCaps[1].MaxVoltageV != 9 || recs[1].SourceCaps[1].MaxPowerW != 27 {
		t.Fatalf("records %+v", recs)
	}
	req := recs[2].Request
	if req == nil || req.PDO == nil || req.PDO.MaxVoltageV != 9 || req.OperatingCurrentA == nil || *req.OperatingCurrentA != 3 {
		t.Fatalf("request %+v", req)
	}
	sum := tr.summary(nil)
	if sum.Contract == nil || sum.Contract.ObjectPosition != 2 || len(sum.SourceCapabilities) != 3 || sum.Note != nil {
		t.Fatalf("summary %+v", sum)
	}
	// A long (undecoded) message record is reported as a message, renders as
	// text without panicking, and does not end the contract.
	recs = feed(t, tr, pdLong(9_300, []byte{1, 2, 3}))
	if len(recs) != 1 || recs[0].Kind != "message" || recs[0].Message != "long_message_undecoded" || recs[0].text() == "" {
		t.Fatalf("long record %+v", recs)
	}
	if tr.summary(nil).Contract == nil {
		t.Fatal("long record cleared the contract")
	}
	// GoodCRC is hidden by default.
	if recs := feed(t, tr, pdMsg(9_400, 0, hdrGoodCRC)); len(recs) != 0 {
		t.Error("GoodCRC not filtered")
	}
	// An empty capture explains itself, and the explanation matches the data.
	if n := (&pdTracker{}).summary(nil); n.Note == nil || *n.Note != noPDNote || n.Records == nil {
		t.Errorf("empty summary %+v", n)
	}
	replug := &pdTracker{}
	feed(t, replug, pdEvent(1, 0x22), pdEvent(2, 0x21))
	if n := replug.summary(nil); n.Note == nil || *n.Note != noPDMessagesNote || n.Events != 2 {
		t.Errorf("replug without PD: %+v", n)
	}
}

// The contract is the Request the source accepted and then confirmed with
// PS_RDY; nothing else becomes a contract.
func TestPDTrackerContractNeedsAccept(t *testing.T) {
	caps := pdMsg(1, 0, hdrSrcCaps|3<<12, pdoFixed5V3A, pdoFixed9V3A, pdoPPS3311V3A)
	cases := []struct {
		name     string
		records  [][]byte
		contract int // expected object position, 0 = none
	}{
		{"accepted", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrAccept), pdMsg(4, 0, hdrPSRdy)}, 2},
		{"rejected then PS_RDY of a swap", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrReject), pdMsg(4, 0, hdrPSRdy)}, 0},
		{"wait", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrWait), pdMsg(4, 0, hdrPSRdy)}, 0},
		// The sink then swaps roles: that Accept and PS_RDY are not for the
		// rejected Request.
		{"rejected, then an accepted swap", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrReject),
			pdMsg(4, 0, hdrAccept), pdMsg(5, 0, hdrPSRdy)}, 0},
		{"PS_RDY without Accept", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(4, 0, hdrPSRdy)}, 0},
		{"Accept on a cable SOP'", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 1, hdrAccept), pdMsg(4, 0, hdrPSRdy)}, 0},
		{"renegotiated to PPS", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrAccept), pdMsg(4, 0, hdrPSRdy),
			pdMsg(5, 0, hdrRequest, rdoPPS3), pdMsg(6, 0, hdrAccept), pdMsg(7, 0, hdrPSRdy)}, 3},
		{"rejected renegotiation keeps the old contract", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrAccept), pdMsg(4, 0, hdrPSRdy),
			pdMsg(5, 0, hdrRequest, rdoPPS3), pdMsg(6, 0, hdrReject)}, 2},
		{"soft reset", [][]byte{caps, pdMsg(2, 0, hdrRequest, rdoFixed2), pdMsg(3, 0, hdrAccept), pdMsg(4, 0, hdrPSRdy), pdMsg(5, 0, hdrSoftReset)}, 0},
	}
	for _, c := range cases {
		tr := &pdTracker{}
		feed(t, tr, c.records...)
		got := tr.summary(nil).Contract
		switch {
		case c.contract == 0 && got != nil:
			t.Errorf("%s: contract %+v, want none", c.name, got)
		case c.contract != 0 && (got == nil || got.ObjectPosition != c.contract):
			t.Errorf("%s: contract %+v, want position %d", c.name, got, c.contract)
		}
	}
}

// Detach and hard reset end the negotiation, including the source's
// capabilities: the next charger starts from scratch.
func TestPDTrackerDetachClearsState(t *testing.T) {
	for _, code := range []byte{0x22 /* detach CC2 */, 0x04 /* hard reset */} {
		tr := &pdTracker{}
		p := parsePD(t, negotiation())
		tr.packet(&p)
		feed(t, tr, pdEvent(10_000, code))
		sum := tr.summary(nil)
		if sum.Contract != nil || sum.LastRequest != nil || len(sum.SourceCapabilities) != 0 || tr.capsText() != "" {
			t.Errorf("event 0x%02X left state behind: %+v", code, sum)
		}
	}
}

// An EPR_Request carries a copy of the PDO it selects; it is decoded against
// that copy even when the EPR capabilities were not captured.
func TestPDTrackerEPRRequestUsesItsPDOCopy(t *testing.T) {
	tr := &pdTracker{}
	recs := feed(t, tr, pdMsg(1, 0, hdrEPRReq, rdoAVS8, pdoEPRAVS))
	r := recs[0].Request
	if r == nil || r.PDO == nil || r.PDO.Type != "epr_avs" || r.ObjectPosition != 8 || !r.EPR {
		t.Fatalf("EPR request %+v", r)
	}
	if r.VoltageV == nil || *r.VoltageV != 28 || r.OperatingCurrentA == nil || *r.OperatingCurrentA != 3 {
		t.Fatalf("EPR AVS request decoded as %+v (want 28 V, 3 A)", r)
	}
}

// Zero slots that pad EPR capabilities are not reported as PDOs, and a
// Request for such a slot has no PDO.
func TestPDTrackerSkipsPaddingSlots(t *testing.T) {
	tr := &pdTracker{}
	tr.srcCaps = nil
	feed(t, tr, pdMsg(1, 0, hdrSrcCaps|3<<12, pdoFixed5V3A, 0, pdoFixed9V3A))
	sum := tr.summary(nil)
	if len(sum.SourceCapabilities) != 2 || sum.SourceCapabilities[1].Index != 3 {
		t.Fatalf("capabilities %+v", sum.SourceCapabilities)
	}
	recs := feed(t, tr, pdMsg(2, 0, hdrRequest, 2<<28|100<<10|100))
	if recs[0].Request == nil || recs[0].Request.PDO != nil {
		t.Fatalf("request for a padding slot %+v", recs[0].Request)
	}
}

// Reserved SOP values and bytes the parser cannot split are reported, not
// dropped, and stay within the schema.
func TestPDTrackerUnknownSOPAndUndecoded(t *testing.T) {
	tr := &pdTracker{}
	payload := pdPayload(pdMsg(1, 5, hdrAccept), []byte{0x81, 0xFF}) // truncated record
	p := parsePD(t, payload)
	if len(p.Trailing) == 0 {
		t.Fatal("test payload has no trailing bytes")
	}
	recs := tr.packet(&p)
	if len(recs) != 2 || recs[0].SOP != "unknown" || recs[1].Kind != "undecoded" || recs[1].RawHex == "" {
		t.Fatalf("records %+v", recs)
	}
	if !strings.Contains(recs[1].text(), "UNDECODED") {
		t.Errorf("text %q", recs[1].text())
	}
	if sum := tr.summary(recs); sum.Undecoded != 1 {
		t.Errorf("undecoded count %d", sum.Undecoded)
	}
	for _, r := range recs {
		checkShape(t, "pd_record", toAny(t, r))
	}
}
