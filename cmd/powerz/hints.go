package main

import (
	"fmt"
	"math"
)

// ccHint interprets CC line voltages as seen by an inline meter. These are
// inferences from the USB Type-C voltage thresholds (vRd ranges with a 5.1 kΩ
// sink pull-down), not data reported by the device.
func ccHint(cc1, cc2 float64) string {
	adv := func(v float64) string {
		switch {
		case v >= 0.25 && v <= 0.61:
			return "Default USB current"
		case v >= 0.70 && v <= 1.16:
			return "1.5 A"
		case v >= 1.31 && v <= 2.04:
			return "3.0 A"
		}
		return ""
	}
	a1, a2 := adv(cc1), adv(cc2)
	switch {
	case a1 != "" && a2 == "":
		return fmt.Sprintf("Type-C attached on CC1, source advertises %s", a1)
	case a2 != "" && a1 == "":
		return fmt.Sprintf("Type-C attached on CC2, source advertises %s", a2)
	case a1 != "" && a2 != "":
		return fmt.Sprintf("both CC lines terminated (CC1 %s, CC2 %s)", a1, a2)
	case cc1 < 0.2 && cc2 < 0.2:
		return "no Type-C termination detected"
	case cc1 > 2.2 || cc2 > 2.2:
		return "CC pulled up, no sink termination (open or legacy cable)"
	}
	return "CC levels outside the standard advertisement ranges"
}

// dataLineHint compares D+/D- with well-known charger signalling levels.
// Purely an inference from voltages; the meter does not report it.
func dataLineHint(dp, dm float64) string {
	near := func(v, target float64) bool { return math.Abs(v-target) <= 0.25 }
	switch {
	case dp < 0.15 && dm < 0.15:
		return "idle (no data-line signalling)"
	case near(dp, 2.7) && near(dm, 2.7):
		return "resembles Apple 2.4 A divider"
	case near(dp, 2.7) && near(dm, 2.0):
		return "resembles Apple 2.1 A divider"
	case near(dp, 2.0) && near(dm, 2.7):
		return "resembles Apple 1 A divider"
	case near(dp, 2.0) && near(dm, 2.0):
		return "resembles Apple 0.5 A divider"
	case near(dp, 0.6) && dm < 0.15:
		return "resembles QC 2.0/3.0 handshake or 5 V request"
	case near(dp, 3.3) && near(dm, 0.6):
		return "resembles QC 9 V request"
	case near(dp, 0.6) && near(dm, 0.6):
		return "resembles BC1.2 DCP (shorted D+/D-) or QC 12 V request"
	case near(dp, 0.6) && near(dm, 3.3):
		return "resembles QC 3.0 continuous mode"
	case near(dp, 3.3) && near(dm, 3.3):
		return "resembles QC 20 V request"
	}
	return "no well-known pattern"
}
