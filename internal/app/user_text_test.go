package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/provision"
)

// The stored last_error is an internal error chain; users must only ever see
// the translated stage text.
func TestOnboardingErrorHidesInternals(t *testing.T) {
	raw := "create upstream user: vm2api POST /users: 409 username_exists: username already exists"
	for _, testcase := range []struct {
		reason, code string
	}{
		{"", "onboarding_failed"},
		{"provisioning_failed:proxy", "provisioning_failed"},
		{"selfcheck_failed", "selfcheck_failed"},
		{"selfcheck_exhausted:fingerprint_unique", "selfcheck_exhausted"},
		{"account_duplicate", "account_duplicate"},
	} {
		dto := toOnboardingDTO(provision.Onboarding{Status: "failed", Error: &provision.OnboardingError{Code: "onboarding_failed", Message: raw, Reason: testcase.reason}})
		if dto.Error == nil || dto.Error.Code != testcase.code || dto.Error.Message == "" {
			t.Fatalf("reason %q: %+v", testcase.reason, dto.Error)
		}
		for _, leak := range []string{"vm2api", "POST", "/users", "409", "fingerprint_unique", "username"} {
			if strings.Contains(dto.Error.Message, leak) {
				t.Fatalf("reason %q leaks %q: %s", testcase.reason, leak, dto.Error.Message)
			}
		}
	}
}

func TestSelfcheckMessageForUsersDropsSentinelAndCodes(t *testing.T) {
	failure := fmt.Errorf("%w: %s（%s）", provision.ErrSelfcheckFailed, "槽位未绑定独立出口", "proxy_bound")
	mapped := api.AsUserError(mapProvisionErr(failure))
	if mapped == nil || strings.Contains(mapped.Message, "slot self-check failed") || strings.Contains(mapped.Message, "proxy_bound") || !strings.Contains(mapped.Message, "槽位未绑定独立出口") {
		t.Fatalf("mapped = %+v", mapped)
	}
}
