package km003c

import "time"

// Test hooks for the external km003c_test package.

// SetMemSpacing changes the minimum gap between MemoryReads.
func SetMemSpacing(d *Device, v time.Duration) { d.memSpacing = v }

// Backdate moves the time of the last request into the past, as if the link
// had been idle for age.
func Backdate(d *Device, age time.Duration) { d.lastIO = time.Now().Add(-age) }
