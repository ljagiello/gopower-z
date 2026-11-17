package km003c

// DashboardLabels reproduces the two D+/D− protocol labels the meter shows on
// its dashboard, using the algorithm of the meter's own firmware generation:
//
//   - firmware <= 2.0.0 (TailLineAverages): 1.9.5 FUN_0001fb44 (identical in
//     1.8.8-1.9.9), inputs VBUS, IBUS (mA) and the D+/D− 50-sample averages
//     reported in the ADC tail, so the result is exact;
//   - firmware >= 2.0.1 or unknown: 2.0.7 FUN_00022098 / FUN_00021bd4
//     (identical in 2.0.1-2.0.7), inputs VBUS and D+/D−. That firmware uses
//     D+/D− averages it no longer reports over USB, so the instantaneous
//     D+/D− values are used instead: labels match except while the data-line
//     voltages are changing.
//
// See docs/firmware.html §10. When the meter has identified a proprietary
// session (VOOC/SVOOC/VFCP/UFCS) it shows that instead; that state is not
// exposed over USB, so these functions return the voltage-based labels only.
func DashboardLabels(a ADC) (line1, line2 string) {
	v := int(a.VBusUV / 1000)
	if a.Layout == TailLineAverages && a.HasLineAvg {
		// IBUS mA as the firmware's u16: negative currents compare as large.
		i := int(uint16(int16(a.IBusUA / 1000)))
		dp, dm := int(a.DPAvgMV), int(a.DMAvgMV)
		return label1v195(v, i, dp, dm), label2v195(v, i, dp, dm)
	}
	dp := int(a.DP) / 10
	dm := int(a.DM) / 10
	return label1v207(v, dp, dm), label2v207(v, dp, dm)
}

const labelNA = "[ N/A ]"

func in(x, lo, hi int) bool { return x >= lo && x <= hi }

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// label1v195 is line 1 of the 1.9.5 dashboard label (FUN_0001fb44).
func label1v195(v, i, dp, dm int) string {
	switch {
	case v < 3300:
		return labelNA
	case dp > 2800:
		switch {
		case dm > 2800:
			switch {
			case v >= 19001:
				return "QC2.0 20V"
			case v > 7000 && i > 1000:
				return "VFCP | UFCS"
			case v < 5500:
				return "DCP 1.5A"
			}
			return labelNA
		case in(dm, 451, 1099):
			if v > 8500 {
				return "QC2.0 9V"
			}
			return "VOOC"
		case v > 8500:
			return "SVOOC"
		}
		return labelNA
	case in(dp, 2501, 2800):
		if v < 5500 && in(dm, 2501, 2899) {
			return "Apple 2.4A"
		}
		return labelNA
	case in(dp, 1101, 1300):
		if v < 5500 && in(dm, 1101, 1299) {
			return "Samsung 2.0A"
		}
		return labelNA
	case in(dp, 501, 1000):
		switch {
		case dm > 2800:
			return "QC3.0 | QC3+"
		case in(dm, 501, 699):
			if in(v, 10501, 12499) {
				return "QC2.0 12V"
			}
			if in(v, 4501, 5499) {
				return "DCP 1.5A"
			}
			return labelNA
		case dm < 200:
			switch {
			case i >= 2001:
				return "QC2.0 | SCP"
			case in(v, 8501, 12499):
				return "FCP | AFC"
			case in(v, 4501, 5499):
				return "QC2.0 5V"
			}
			return labelNA
		}
	}
	return labelNA
}

// label2v195 is line 2 of the 1.9.5 dashboard label.
func label2v195(v, i, dp, dm int) string {
	switch {
	case v < 3300:
		return labelNA
	case in(v, 8501, 9999) && in(dp, 1100, 1699) && in(dm, 1100, 1699):
		return "SFCP 9V"
	case in(v, 10000, 12499) && in(dp, 2100, 2699) && in(dm, 2100, 2699):
		return "SFCP 12V"
	case v >= 12500 && dp < 700:
		return "FCP | SCP"
	case i <= 2000:
		alt := labelNA
		if abs(dp-dm) < 100 {
			if in(v, 4501, 5499) {
				alt = "DCP 1.5A"
			} else if v < 19500 && dp > 3000 {
				alt = "QC2.0 20V Err"
			}
		}
		if in(dp, 501, 699) && dm < 300 {
			return "FCP | SCP | AFC"
		}
		return alt
	case dp > 2900:
		if v >= 7001 {
			return "SVOOC"
		}
		if v < 5500 {
			return "VOOC"
		}
	}
	return labelNA
}

// label1v207 is line 1 of the 2.0.7 dashboard label (FUN_00022098).
func label1v207(v, dp, dm int) string {
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

// label2v207 is line 2 of the 2.0.7 dashboard label (FUN_00021bd4).
func label2v207(v, dp, dm int) string {
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
