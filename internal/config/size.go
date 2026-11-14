package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// ParseSize parses a byte size: a plain number of bytes ("4096") or a whole
// number with a binary unit ("512KiB", "64MiB", "1GiB", "64 MiB"). Units are
// case-insensitive. Zero, negative and fractional sizes are rejected, and so
// are SI units like "MB", which are ambiguous.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	num, unit := t[:i], strings.TrimSpace(t[i:])
	bad := fmt.Errorf("invalid size %q: use bytes or a whole number of KiB, MiB or GiB, e.g. 64MiB", s)
	if num == "" {
		return 0, bad
	}
	mult := int64(0)
	if unit == "" {
		mult = 1
	}
	for _, u := range sizeUnits {
		if strings.EqualFold(unit, u.suffix) {
			mult = u.mult
		}
	}
	if mult == 0 {
		return 0, bad
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	if n == 0 {
		return 0, fmt.Errorf("invalid size %q: must be greater than 0", s)
	}
	return n * mult, nil
}

// FormatSize formats n in the largest binary unit that holds it exactly
// ("64MiB", "1536KiB"), or as plain bytes. ParseSize(FormatSize(n)) == n.
func FormatSize(n int64) string {
	if n > 0 {
		for _, u := range sizeUnits[:3] {
			if n%u.mult == 0 {
				return strconv.FormatInt(n/u.mult, 10) + u.suffix
			}
		}
	}
	return strconv.FormatInt(n, 10)
}
