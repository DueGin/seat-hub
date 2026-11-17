// Package signup enforces the email policy and one-time codes for self-service registration.
package signup

import (
	"fmt"
	"strings"

	"github.com/DueGin/seat-hub/internal/mail"
)

func Email(value string) bool { return mail.Address(value) }

func hostname(value string) bool {
	if len(value) > 253 || !strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func NormalizeWhitelist(entries []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" || seen[entry] {
			continue
		}
		if strings.Contains(entry, "@") {
			if !Email(entry) {
				return nil, fmt.Errorf("白名单邮箱格式不正确：%s", entry)
			}
		} else if !hostname(strings.TrimPrefix(entry, "*.")) {
			return nil, fmt.Errorf("白名单域名格式不正确：%s", entry)
		}
		seen[entry] = true
		out = append(out, entry)
		if len(out) > 200 {
			return nil, fmt.Errorf("注册邮箱白名单最多 200 项")
		}
	}
	return out, nil
}

func Allowed(email string, entries []string) bool {
	if len(entries) == 0 {
		return true
	}
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if at < 1 {
		return false
	}
	domain := email[at+1:]
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if strings.Contains(entry, "@") {
			if entry == email {
				return true
			}
		} else if strings.HasPrefix(entry, "*.") {
			if strings.HasSuffix(domain, entry[1:]) {
				return true
			}
		} else if domain == entry {
			return true
		}
	}
	return false
}

func Domains(entries []string) []string {
	domains := []string{}
	for _, entry := range entries {
		if !strings.Contains(entry, "@") {
			domains = append(domains, entry)
		}
	}
	return domains
}
