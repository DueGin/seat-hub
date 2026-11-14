package update

import (
	"regexp"
	"strconv"
	"strings"
)

// tagPattern accepts release tags like v1.2.3 and v1.2.3-rc.1. It is also the
// only shape a version may have before it is used in a URL or a file name.
var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})(-[0-9A-Za-z.-]{1,40})?$`)

// ValidTag reports whether tag is a release version this package will act on.
func ValidTag(tag string) bool { return tagPattern.MatchString(tag) }

// Compare orders two release tags: -1 if a < b, 0 if equal, 1 if a > b.
// A pre-release sorts before its release (v1.2.0-rc.1 < v1.2.0). Tags that are
// not valid sort before every valid one, so "dev" is never newer than a release.
func Compare(a, b string) int {
	pa, okA := parseTag(a)
	pb, okB := parseTag(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for index := range 3 {
		if pa.core[index] != pb.core[index] {
			if pa.core[index] < pb.core[index] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	}
	return comparePre(pa.pre, pb.pre)
}

// Newer reports whether candidate is a strictly newer release than current.
func Newer(candidate, current string) bool { return Compare(candidate, current) > 0 }

type parsedTag struct {
	core [3]int
	pre  string
}

func parseTag(tag string) (parsedTag, bool) {
	match := tagPattern.FindStringSubmatch(tag)
	if match == nil {
		return parsedTag{}, false
	}
	var out parsedTag
	for index := range 3 {
		out.core[index], _ = strconv.Atoi(match[index+1])
	}
	out.pre = strings.TrimPrefix(match[4], "-")
	return out, true
}

// comparePre follows semver: dot-separated identifiers, numeric ones compare
// numerically and sort before alphanumeric ones, a longer list wins a tie.
func comparePre(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for index := 0; index < len(left) && index < len(right); index++ {
		x, y := left[index], right[index]
		nx, errX := strconv.Atoi(x)
		ny, errY := strconv.Atoi(y)
		switch {
		case errX == nil && errY == nil:
			if nx != ny {
				if nx < ny {
					return -1
				}
				return 1
			}
		case errX == nil:
			return -1
		case errY == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	}
	return 0
}
