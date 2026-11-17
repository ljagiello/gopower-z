package km003c

// DashboardLabels reproduces the two D+/D− protocol labels the meter shows on
// its dashboard. The meter computes them from VBUS, D+ and D− alone, so the
// same strings can be derived from an ADC snapshot. Ported from firmware
// 2.0.7 (FUN_00022098 and FUN_00021bd4); see docs/firmware.html.
//
// They describe what the voltages on the data lines look like. Protocols
// detected by sniffing (AFC/FCP/SCP/UFCS/VOOC codes) and USB-PD details are
// not part of this function.
func DashboardLabels(a ADC) (line1, line2 string) {
	v := int(a.VBusUV / 1000)
	dp := int(a.DP) / 10
	dm := int(a.DM) / 10
	return dashboardLabel1(v, dp, dm), dashboardLabel2(v, dp, dm)
}

const labelNA = "[ N/A ]"

func in(x, lo, hi int) bool { return x >= lo && x <= hi }

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func dashboardLabel1(v, dp, dm int) string {
	switch {
	case v < 3300:
		return labelNA
	case dm > 2800 && in(dp, 501, 699):
		return "QC3.0 | QC3+"
	case v < 5500:
		switch {
		case dp < 110 && dm < 100:
			return "DCP"
		case dp > 2500 && in(dm, 2501, 2899):
			return "Apple 2.4A"
		case dm < 100 && in(dp, 501, 699):
			return "QC2.0 5V"
		case abs(dp-dm) < 100:
			return "DCP"
		}
		return labelNA
	case in(v, 8501, 9499):
		switch {
		case dp > 2800 && in(dm, 451, 1099):
			return "QC2.0 9V"
		case in(dp, 1101, 1299) && in(dm, 1101, 1299):
			return "Samsung 2.0A"
		case dm < 100 && in(dp, 501, 699):
			return "FCP | AFC"
		case in(dp, 501, 699) && in(dm, 501, 699):
			return "QC2.0 12V Error"
		case dp > 2800 && dm > 2800:
			return "QC2.0 20V Error"
		}
		return labelNA
	case in(v, 11001, 12999):
		switch {
		case in(dp, 501, 699) && in(dm, 501, 699):
			return "QC2.0 12V"
		case in(dp, 501, 699) && dm < 100:
			return "FCP | AFC"
		}
		return labelNA
	case in(v, 18501, 20999) && dp > 2800 && dm > 2800:
		return "QC2.0 20V"
	}
	return labelNA
}

func dashboardLabel2(v, dp, dm int) string {
	switch {
	case v < 3300:
		return labelNA
	case v > 8500 && v < 10000 && in(dp, 1100, 1699) && in(dm, 1100, 1699):
		return "SFCP 9V"
	case in(v, 10000, 12499) && in(dp, 2100, 2699) && in(dm, 2100, 2699):
		return "SFCP 12V"
	case v > 12499 && dp < 700:
		return "FCP | SCP"
	case dm < 300 && in(dp, 501, 699):
		return "FCP | SCP | AFC"
	}
	return labelNA
}
