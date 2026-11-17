package km003c

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/google/gousb"
)

// USBTransport talks to interface 0 (vendor class) over bulk endpoints
// 0x01 (OUT) and 0x81 (IN) using libusb.
type USBTransport struct {
	ctx  *gousb.Context
	dev  *gousb.Device
	cfg  *gousb.Config
	intf *gousb.Interface
	out  *gousb.OutEndpoint
	in   *gousb.InEndpoint
	desc USBDescription
}

// USBDescription identifies a connected meter by its USB descriptors.
type USBDescription struct {
	Manufacturer string `json:"manufacturer"`
	Product      string `json:"product"`
	Serial       string `json:"serial"`
	Bus          int    `json:"bus"`
	Address      int    `json:"address"`
}

// isLibusb reports whether err is (or wraps, possibly only textually) the
// given libusb error. gousb wraps some errors with %v, which hides them from
// errors.Is, so the "[code N]" suffix of the message is checked as well.
func isLibusb(err error, code gousb.Error) bool {
	return errors.Is(err, code) || strings.Contains(err.Error(), fmt.Sprintf("[code %d]", int(code)))
}

// mapUSBError converts libusb errors into this package's sentinel errors.
func mapUSBError(op string, err error) error {
	switch {
	case isLibusb(err, gousb.ErrorBusy):
		return fmt.Errorf("%w (%s: %w)", ErrBusy, op, err)
	case isLibusb(err, gousb.ErrorAccess):
		return fmt.Errorf("%w (%s: %w)", ErrPermission, op, err)
	case isLibusb(err, gousb.ErrorNoDevice), isLibusb(err, gousb.ErrorNotFound):
		return fmt.Errorf("%w (%s: %w)", ErrNotFound, op, err)
	case isLibusb(err, gousb.ErrorTimeout), errors.Is(err, gousb.TransferTimedOut):
		return fmt.Errorf("%w (%s: %w)", ErrTimeout, op, err)
	}
	return fmt.Errorf("km003c: %s: %w", op, err)
}

// newContext initialises libusb. gousb panics when libusb_init fails (for
// example in a container without USB access); turn that into an error.
func newContext() (c *gousb.Context, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w (libusb init: %v)", ErrNotFound, r)
		}
	}()
	return gousb.NewContext(), nil
}

func isMeter(d *gousb.DeviceDesc) bool {
	return d.Vendor == VendorID && d.Product == ProductID
}

func describe(d *gousb.Device) USBDescription {
	var u USBDescription
	u.Manufacturer, _ = d.Manufacturer()
	u.Product, _ = d.Product()
	u.Serial, _ = d.SerialNumber()
	u.Bus, u.Address = d.Desc.Bus, d.Desc.Address
	return u
}

// ListDevices returns every connected KM003C. Devices are opened only long
// enough to read their string descriptors; no interface is claimed and no
// request is sent.
func ListDevices() ([]USBDescription, error) {
	c, err := newContext()
	if err != nil {
		return []USBDescription{}, err
	}
	defer func() { _ = c.Close() }()
	devs, err := c.OpenDevices(isMeter)
	defer func() {
		for _, d := range devs {
			_ = d.Close()
		}
	}()
	out := make([]USBDescription, 0, len(devs))
	for _, d := range devs {
		out = append(out, describe(d))
	}
	if err != nil && len(devs) == 0 {
		return out, mapUSBError("enumerate", err)
	}
	return out, nil
}

// OpenUSB opens the first KM003C (or the one whose USB serial matches, if
// serial is non-empty) and claims interface 0. On Linux the in-kernel
// "powerz" hwmon driver owns interface 0 and is detached temporarily.
func OpenUSB(serial string) (*USBTransport, error) {
	c, err := newContext()
	if err != nil {
		return nil, err
	}
	t := &USBTransport{ctx: c}
	devs, err := c.OpenDevices(isMeter)
	if err != nil && len(devs) == 0 {
		_ = c.Close()
		return nil, mapUSBError("open", err)
	}
	var seen []string
	for _, d := range devs {
		if t.dev == nil {
			s, _ := d.SerialNumber()
			seen = append(seen, s)
			if serial == "" || s == serial {
				t.dev = d
				continue
			}
		}
		_ = d.Close()
	}
	if t.dev == nil {
		_ = c.Close()
		if serial != "" {
			return nil, fmt.Errorf("%w: no meter with serial %q (connected: %v)", ErrNotFound, serial, seen)
		}
		return nil, fmt.Errorf("%w: no POWER-Z KM003C (USB %04x:%04x) connected", ErrNotFound, VendorID, ProductID)
	}
	if err := t.claim(); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

func (t *USBTransport) claim() error {
	d := t.dev
	t.desc = describe(d)
	cfgNum, err := d.ActiveConfigNum()
	if err != nil {
		return mapUSBError("active config", err)
	}
	// Config() only sets the configuration if it differs from the active
	// one, so the device is not reset.
	if t.cfg, err = d.Config(cfgNum); err != nil {
		return mapUSBError("open config", err)
	}
	if runtime.GOOS == "linux" {
		// The in-kernel "powerz" hwmon driver owns interface 0. Enable
		// libusb's auto-detach only now: set before Config(), gousb would
		// detach the CDC and HID drivers of every interface and never
		// reattach them. libusb detaches interface 0 on claim and reattaches
		// it on release.
		_ = d.SetAutoDetach(true)
	}
	if t.intf, err = t.cfg.Interface(0, 0); err != nil {
		// Permissions were already checked when the device was opened, so
		// ACCESS here (macOS: exclusive access) means another process holds
		// the interface.
		if isLibusb(err, gousb.ErrorAccess) || isLibusb(err, gousb.ErrorBusy) {
			return fmt.Errorf("%w (claim interface 0: %w)", ErrBusy, err)
		}
		return mapUSBError("claim interface 0", err)
	}
	if t.out, err = t.intf.OutEndpoint(0x01); err != nil {
		return mapUSBError("endpoint 0x01", err)
	}
	if t.in, err = t.intf.InEndpoint(0x81); err != nil {
		return mapUSBError("endpoint 0x81", err)
	}
	return nil
}

// Description returns the USB string descriptors read at open.
func (t *USBTransport) Description() USBDescription { return t.desc }

// Kind implements Transport.
func (t *USBTransport) Kind() TransportKind { return Bulk }

// Write implements Transport.
func (t *USBTransport) Write(ctx context.Context, p []byte) error {
	// Defence in depth: Device already checks every frame.
	if err := CheckRequest(p, Bulk); err != nil {
		return err
	}
	n, err := t.out.WriteContext(ctx, p)
	if err != nil {
		return mapUSBError("bulk write", err)
	}
	if n != len(p) {
		return fmt.Errorf("km003c: bulk write: short write %d/%d", n, len(p))
	}
	return nil
}

// Read implements Transport. A transfer that is cut off by the deadline after
// some data arrived is returned as (n, ErrIncomplete).
func (t *USBTransport) Read(ctx context.Context, p []byte) (int, error) {
	n, err := t.in.ReadContext(ctx, p)
	switch {
	case err == nil:
		return n, nil
	case n > 0:
		return n, ErrIncomplete
	case errors.Is(ctx.Err(), context.Canceled):
		return 0, context.Canceled
	case errors.Is(err, gousb.TransferCancelled), errors.Is(err, gousb.TransferTimedOut),
		errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return 0, ErrTimeout
	}
	return 0, mapUSBError("bulk read", err)
}

// Close releases the interface and closes the device.
func (t *USBTransport) Close() error {
	var errs []error
	if t.intf != nil {
		t.intf.Close()
		t.intf = nil
	}
	if t.cfg != nil {
		errs = append(errs, t.cfg.Close())
		t.cfg = nil
	}
	if t.dev != nil {
		errs = append(errs, t.dev.Close())
		t.dev = nil
	}
	if t.ctx != nil {
		errs = append(errs, t.ctx.Close())
		t.ctx = nil
	}
	return errors.Join(errs...)
}
