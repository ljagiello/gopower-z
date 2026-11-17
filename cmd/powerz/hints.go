package main

import "fmt"

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
