package version

import (
	"strconv"
	"strings"
)

// Semver is a parsed semantic version: MAJOR.MINOR.PATCH with an optional
// pre-release. Build metadata is accepted and ignored, as it carries no
// precedence.
type Semver struct {
	Major, Minor, Patch uint64
	Pre                 []string
}

// Parse reads a semantic version, tolerating a leading "v". It reports false
// for anything else, including development builds ("dev", "latest").
func Parse(s string) (Semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Semver{}, false
	}
	var nums [3]uint64
	for i, p := range parts {
		if !isNumeric(p) {
			return Semver{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Semver{}, false
		}
		nums[i] = n
	}
	v := Semver{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if hasPre {
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if id == "" {
				return Semver{}, false
			}
		}
	}
	return v, true
}

// Stable reports whether v is a final release rather than a pre-release.
func (v Semver) Stable() bool { return len(v.Pre) == 0 }

// String renders v without a leading "v".
func (v Semver) String() string {
	s := strconv.FormatUint(v.Major, 10) + "." + strconv.FormatUint(v.Minor, 10) + "." + strconv.FormatUint(v.Patch, 10)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Compare orders a and b by semver precedence: -1, 0 or +1.
func Compare(a, b Semver) int {
	if c := cmpUint(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpUint(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpUint(a.Patch, b.Patch); c != 0 {
		return c
	}
	// A release outranks any of its pre-releases.
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := cmpIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpUint(uint64(len(a.Pre)), uint64(len(b.Pre)))
}

// cmpIdent compares pre-release identifiers: numeric ones numerically and
// below alphanumeric ones, which compare as ASCII.
func cmpIdent(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		ai, _ := strconv.ParseUint(a, 10, 64)
		bi, _ := strconv.ParseUint(b, 10, 64)
		return cmpUint(ai, bi)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
