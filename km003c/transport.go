package km003c

import (
	"context"
	"errors"
)

// Transport moves raw frames to and from the meter. Write sends one request;
// Read returns one reply transfer (bulk: one transfer terminated by a short
// packet; HID: one 64-byte report). Read must return ErrTimeout when nothing
// arrives before the context deadline, context.Canceled when the context was
// cancelled, and (n, ErrIncomplete) when a transfer was cut off after some
// data arrived. Transports are not safe for direct use: Device enforces the
// request whitelist and the drain discipline the meter requires.
type Transport interface {
	Kind() TransportKind
	Write(ctx context.Context, p []byte) error
	Read(ctx context.Context, p []byte) (int, error)
	Close() error
}

// Errors callers can test with errors.Is.
var (
	// ErrTimeout is returned when the meter did not answer in time.
	ErrTimeout = errors.New("km003c: read timeout")
	// ErrIncomplete is returned by Transport.Read together with n > 0 when
	// a transfer was cut off by the deadline: the data is valid but more of
	// the reply may still be queued.
	ErrIncomplete = errors.New("km003c: transfer cut off by deadline")
	// ErrNotFound means no KM003C (or none with the requested serial) is
	// connected.
	ErrNotFound = errors.New("km003c: meter not found")
	// ErrBusy means another program holds the meter's USB interface.
	ErrBusy = errors.New("km003c: meter is in use by another program")
	// ErrPermission means the OS denied access to the USB device.
	ErrPermission = errors.New("km003c: permission denied opening the meter")
	// ErrRejected means the meter answered a request with REJECT.
	ErrRejected = errors.New("km003c: request rejected by the meter")
)
