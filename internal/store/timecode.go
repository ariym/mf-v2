package store

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseTimecode parses a media timestamp, never a date or wall-clock time.
func ParseTimecode(text string) (int64, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("invalid media timecode %q; use HH:MM:SS.ffffff", text)
	}
	h, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || h < 0 || len(parts[0]) < 2 || h > 2_000_000_000 {
		return 0, fmt.Errorf("invalid timecode hours")
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || len(parts[1]) != 2 || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid timecode minutes")
	}
	seconds := strings.Split(parts[2], ".")
	if len(seconds) > 2 || len(seconds[0]) != 2 {
		return 0, fmt.Errorf("invalid timecode seconds")
	}
	s, err := strconv.Atoi(seconds[0])
	if err != nil || s < 0 || s > 59 {
		return 0, fmt.Errorf("invalid timecode seconds")
	}
	var fraction int64
	if len(seconds) == 2 {
		if len(seconds[1]) < 1 || len(seconds[1]) > 6 {
			return 0, fmt.Errorf("timecodes support up to six fractional digits")
		}
		for _, c := range seconds[1] {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("invalid timecode fraction")
			}
		}
		fraction, err = strconv.ParseInt(seconds[1]+strings.Repeat("0", 6-len(seconds[1])), 10, 64)
		if err != nil {
			return 0, err
		}
	}
	// Reject signed components, whitespace, and alternative numeric spellings.
	for _, p := range []string{parts[0], parts[1], seconds[0]} {
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("invalid media timecode")
			}
		}
	}
	return ((h*60+int64(m))*60+int64(s))*1_000_000 + fraction, nil
}
func Timecode(us int64) string {
	return fmt.Sprintf("%02d:%02d:%02d.%06d", us/3_600_000_000, (us/60_000_000)%60, (us/1_000_000)%60, us%1_000_000)
}
func TimecodeSeconds(seconds float64) (string, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > 7.2e12 {
		return "", fmt.Errorf("invalid media timestamp")
	}
	return Timecode(int64(math.Round(seconds * 1e6))), nil
}
