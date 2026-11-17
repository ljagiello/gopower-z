package km003c

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/gousb"
)

// gousb wraps some libusb errors with %v, which hides them from errors.Is;
// the "[code N]" text fallback must still classify them.
func TestMapUSBError(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{gousb.ErrorBusy, ErrBusy},
		{fmt.Errorf("claim: %w", gousb.ErrorBusy), ErrBusy},
		{errors.New("claim: " + gousb.ErrorBusy.Error()), ErrBusy}, // text only
		{gousb.ErrorAccess, ErrPermission},
		{errors.New("open: " + gousb.ErrorAccess.Error()), ErrPermission},
		{gousb.ErrorNoDevice, ErrNotFound},
		{gousb.ErrorNotFound, ErrNotFound},
		{errors.New("x: " + gousb.ErrorNoDevice.Error()), ErrNotFound},
		{gousb.ErrorTimeout, ErrTimeout},
		{gousb.TransferTimedOut, ErrTimeout},
	}
	for _, c := range cases {
		if got := mapUSBError("op", c.err); !errors.Is(got, c.want) {
			t.Errorf("mapUSBError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	other := mapUSBError("op", errors.New("pipe error"))
	for _, s := range []error{ErrBusy, ErrPermission, ErrNotFound, ErrTimeout} {
		if errors.Is(other, s) {
			t.Errorf("unrelated error mapped to %v", s)
		}
	}
	// "[code 6]" must not match code 16 and vice versa.
	if isLibusb(errors.New("libusb: x [code -16]"), gousb.ErrorBusy) {
		t.Error("[code -16] matched ErrorBusy (-6)")
	}
}

func TestReadResult(t *testing.T) {
	boom := errors.New("pipe error")
	cases := []struct {
		name   string
		n      int
		err    error
		ctxErr error
		wantN  int
		want   error
	}{
		{"data", 52, nil, nil, 52, nil},
		{"cut off", 64, gousb.TransferCancelled, context.DeadlineExceeded, 64, ErrIncomplete},
		{"cancelled", 0, gousb.TransferCancelled, context.Canceled, 0, context.Canceled},
		{"deadline", 0, gousb.TransferCancelled, context.DeadlineExceeded, 0, ErrTimeout},
		{"libusb timeout", 0, gousb.TransferTimedOut, nil, 0, ErrTimeout},
		{"ctx deadline error", 0, context.DeadlineExceeded, nil, 0, ErrTimeout},
		{"device gone", 0, gousb.ErrorNoDevice, nil, 0, ErrNotFound},
		{"other", 0, boom, nil, 0, boom},
	}
	for _, c := range cases {
		n, err := readResult(c.n, c.err, c.ctxErr)
		if n != c.wantN || !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("%s: readResult = (%d, %v), want (%d, %v)", c.name, n, err, c.wantN, c.want)
		}
	}
}
