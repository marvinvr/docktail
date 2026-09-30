package version

import "testing"

func TestParse(t *testing.T) {
	valid := map[string]string{
		"1.8.3":          "1.8.3",
		"v1.8.3":         "1.8.3",
		"2.0.0-cloud.16": "2.0.0-cloud.16",
		"1.2.3+build.5":  "1.2.3",
	}
	for in, want := range valid {
		v, ok := Parse(in)
		if !ok || v.String() != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, v.String(), ok, want)
		}
	}
	for _, in := range []string{"", "dev", "latest", "a.7.3", "1.8", "1.8.3.4", "1.8.x", "1.8.3-", "1.8.3-a..b"} {
		if _, ok := Parse(in); ok {
			t.Errorf("Parse(%q) accepted an invalid version", in)
		}
	}
}

func TestCompare(t *testing.T) {
	// Each version is lower than the next.
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
		"1.7.9", "1.8.0-debug.4", "1.8.0", "1.8.3", "2.0.0-cloud.2", "2.0.0-cloud.16", "2.0.0",
	}
	for i := 0; i+1 < len(ordered); i++ {
		a, _ := Parse(ordered[i])
		b, _ := Parse(ordered[i+1])
		if Compare(a, b) != -1 || Compare(b, a) != 1 {
			t.Errorf("expected %s < %s", ordered[i], ordered[i+1])
		}
	}
	a, _ := Parse("v1.8.3")
	b, _ := Parse("1.8.3+meta")
	if Compare(a, b) != 0 {
		t.Errorf("expected v1.8.3 == 1.8.3+meta")
	}
}
