package main

// ccInfo is powerz's interpretation of the CC line voltages as seen by an
// inline meter: which CC line carries the Type-C connection and what current
// the source advertises. It is inferred from the USB Type-C vRd thresholds
// (5.1 kΩ sink pull-down), not reported by the meter.
type ccInfo struct {
	// Attached is cc1, cc2, both, none (no termination) or open (pulled up,
	// no sink), or unknown.
	Attached string `json:"attached"`
	// Advertisement is default_usb, 1.5A, 3.0A, none or unknown.
	Advertisement string `json:"advertisement"`
	// AdvertisedCurrentA is 1.5 or 3.0 for the Type-C current levels, null
	// for default USB current (500/900 mA, set by the USB version) or none.
	AdvertisedCurrentA *float64 `json:"advertised_current_a"`
	Text               string   `json:"text"`
}

func ccAdvertisement(v float64) string {
	switch {
	case v >= 0.25 && v <= 0.61:
		return "default_usb"
	case v >= 0.70 && v <= 1.16:
		return "1.5A"
	case v >= 1.31 && v <= 2.04:
		return "3.0A"
	}
	return ""
}

func advertisedAmps(adv string) *float64 {
	switch adv {
	case "1.5A":
		return new(1.5)
	case "3.0A":
		return new(3.0)
	}
	return nil
}

func advText(adv string) string {
	if adv == "default_usb" {
		return "Default USB current"
	}
	return adv
}

func ccInfer(cc1, cc2 float64) ccInfo {
	a1, a2 := ccAdvertisement(cc1), ccAdvertisement(cc2)
	switch {
	case a1 != "" && a2 == "":
		return ccInfo{"cc1", a1, advertisedAmps(a1), "Type-C attached on CC1, source advertises " + advText(a1)}
	case a2 != "" && a1 == "":
		return ccInfo{"cc2", a2, advertisedAmps(a2), "Type-C attached on CC2, source advertises " + advText(a2)}
	case a1 != "" && a2 != "":
		adv := "unknown"
		if a1 == a2 {
			adv = a1
		}
		return ccInfo{"both", adv, advertisedAmps(adv),
			"both CC lines terminated (CC1 " + advText(a1) + ", CC2 " + advText(a2) + ")"}
	case cc1 < 0.2 && cc2 < 0.2:
		return ccInfo{"none", "none", nil, "no Type-C termination detected"}
	case cc1 > 2.2 || cc2 > 2.2:
		return ccInfo{"open", "unknown", nil, "CC pulled up, no sink termination (open or legacy cable)"}
	}
	return ccInfo{"unknown", "unknown", nil, "CC levels outside the standard advertisement ranges"}
}

// ccHint is the human-readable form used by the text outputs.
func ccHint(cc1, cc2 float64) string { return ccInfer(cc1, cc2).Text }
