package update

import "testing"

func TestCompare(t *testing.T) {
	for _, testcase := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.10.0", "v1.9.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.2.0-rc.1", "v1.2.0", -1},
		{"v1.2.0-rc.2", "v1.2.0-rc.1", 1},
		{"v1.2.0-rc.10", "v1.2.0-rc.9", 1},
		{"v1.2.0-alpha", "v1.2.0-alpha.1", -1},
		{"v1.2.0-1", "v1.2.0-alpha", -1},
		{"dev", "v0.0.1", -1},
		{"v0.0.1", "dev", 1},
		{"dev", "nope", 0},
		{"1.2.3", "v0.0.1", -1},
		{"v01.2.3", "v0.0.1", -1},
	} {
		if got := Compare(testcase.a, testcase.b); got != testcase.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", testcase.a, testcase.b, got, testcase.want)
		}
	}
}

func TestValidTagRejectsPathTricks(t *testing.T) {
	for _, tag := range []string{"v1.2.3/../../x", "v1.2.3 ", "v1.2", "latest", "v1.2.3-", "v1.2.3-a/b", ""} {
		if ValidTag(tag) {
			t.Errorf("ValidTag(%q) = true", tag)
		}
	}
	for _, tag := range []string{"v0.1.0", "v1.2.3-rc.1", "v10.20.30"} {
		if !ValidTag(tag) {
			t.Errorf("ValidTag(%q) = false", tag)
		}
	}
}
