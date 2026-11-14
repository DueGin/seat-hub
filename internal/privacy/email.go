// Package privacy masks personal data before it leaves the server.
package privacy

import "strings"

// MaskEmail shows at most the first three characters of the mailbox and only
// the top-level suffix of the domain: fdsiome@protonmail.com -> fds***@***.com.
//
// It accepts an address that is already masked (vm2api sends e.g.
// "f***@protonmail.com", and older rows hold that form) and tightens it: only
// characters that were visible stay visible. Short mailboxes keep fewer
// characters, so at least half of a mailbox is always hidden. Applying it twice
// gives the same result.
func MaskEmail(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	at := strings.LastIndexByte(value, '@')
	if at < 0 {
		return "***"
	}
	local, domain := value[:at], value[at+1:]

	// Characters known to belong to the mailbox: everything before the first
	// mask character when the input is already masked.
	visible := []rune(local)
	masked := false
	if cut := strings.IndexByte(local, '*'); cut >= 0 {
		visible, masked = []rune(local[:cut]), true
	}
	keep := 3
	if !masked {
		// A raw mailbox: never reveal more than half of it.
		keep = min(3, len(visible)/2)
	}
	keep = min(keep, len(visible))
	head := string(visible[:keep])

	suffix := ""
	domain = strings.TrimSuffix(domain, ".")
	if dot := strings.LastIndexByte(domain, '.'); dot >= 0 && dot < len(domain)-1 {
		tld := domain[dot+1:]
		if !strings.Contains(tld, "*") {
			suffix = "." + strings.ToLower(tld)
		}
	}
	return head + "***@***" + suffix
}
