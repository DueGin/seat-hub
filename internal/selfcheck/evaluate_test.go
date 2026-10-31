package selfcheck

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func healthyInput() Input {
	return Input{
		VM: vm2api.VM{
			ID: "vm-target", ProxyConfigured: true, Timezone: "Asia/Tokyo",
			Proxy: &vm2api.ProxyView{
				ID: "proxy-target", Scheme: "socks5", Enabled: true, Status: "ok",
				GeoIP: "203.0.113.10", GeoTimezone: "Asia/Tokyo",
			},
			Fingerprint: vm2api.Fingerprint{
				DeviceID: "device-target", GuestMachineID: "machine-target", Hostname: "host-target", Timezone: "Asia/Tokyo",
			},
			AccountUUID: "account-target", Email: "target@example.com",
		},
		Probe: vm2api.ExitProbe{IP: "203.0.113.10", Timezone: "Asia/Tokyo"},
		Peers: []vm2api.VM{{
			ID: "vm-peer", Proxy: &vm2api.ProxyView{ID: "proxy-peer", GeoIP: "203.0.113.20"},
			Fingerprint: vm2api.Fingerprint{DeviceID: "device-peer", GuestMachineID: "machine-peer", Hostname: "host-peer"},
			AccountUUID: "account-peer", Email: "peer@example.com",
		}},
		PeerExitIPs: map[string]string{}, CheckAccount: true,
		Rules: Rules{RequireDedicatedProxy: true, RequireTimezoneMatch: true},
		Now:   time.Date(2026, time.October, 31, 12, 0, 0, 0, time.FixedZone("local", 8*60*60)),
	}
}

func findCheck(t *testing.T, result Result, code Code) Check {
	t.Helper()
	for _, check := range result.Checks {
		if check.Code == code {
			return check
		}
	}
	t.Fatalf("check %q missing in %+v", code, result)
	return Check{}
}

func TestEvaluateHealthyAndPure(t *testing.T) {
	in := healthyInput()
	result := Evaluate(in)
	if !result.Passed || result.Remedy() != RemedyNone || len(result.Failed()) != 0 {
		t.Fatalf("healthy result = %+v", result)
	}
	if result.VMID != in.VM.ID || result.ExitIP != in.Probe.IP || result.ProxyID != in.VM.Proxy.ID || result.CheckedAt != in.Now {
		t.Fatalf("metadata = %+v", result)
	}
	codes := []Code{ProxyBound, ProxyHealthy, ProxyExclusive, ExitProbe, ExitIPUnique, ExitGeoTimezone, FingerprintPresent, FingerprintUnique, AccountUnique}
	if len(result.Checks) != len(codes) {
		t.Fatalf("check count = %d, want %d", len(result.Checks), len(codes))
	}
	for index, code := range codes {
		check := result.Checks[index]
		if check.Code != code || !check.OK || check.Skipped || check.Message == "" || check.Remedy != RemedyNone {
			t.Errorf("check %q = %+v", code, check)
		}
	}
	if !reflect.DeepEqual(in, healthyInput()) {
		t.Fatal("Evaluate changed its input")
	}
	if !reflect.DeepEqual(result, Evaluate(in)) {
		t.Fatal("Evaluate is not deterministic")
	}
}

func TestEvaluateRules(t *testing.T) {
	tests := []struct {
		name    string
		code    Code
		change  func(*Input)
		ok      bool
		skipped bool
		remedy  Remedy
		detail  []string
		message string
	}{
		{name: "bound dedicated", code: ProxyBound, ok: true},
		{name: "bound missing", code: ProxyBound, change: func(in *Input) { in.VM.Proxy = nil }, remedy: RemedyReproxy, detail: []string{"vm-target"}},
		{name: "bound unconfigured", code: ProxyBound, change: func(in *Input) { in.VM.ProxyConfigured = false }, remedy: RemedyReproxy, detail: []string{"proxy-target"}},
		{name: "bound shared local", code: ProxyBound, change: func(in *Input) { in.VM.Proxy.Scheme = "local" }, remedy: RemedyReproxy, message: "共享出口不满足一 IP 一槽"},
		{name: "bound non socks5", code: ProxyBound, change: func(in *Input) { in.VM.Proxy.Scheme = "http" }, remedy: RemedyReproxy},
		{name: "bound local allowed", code: ProxyBound, change: func(in *Input) { in.VM.Proxy.Scheme = "local"; in.Rules.RequireDedicatedProxy = false }, ok: true},
		{name: "bound still required", code: ProxyBound, change: func(in *Input) { in.VM.Proxy = nil; in.Rules.RequireDedicatedProxy = false }, remedy: RemedyReproxy},
		{name: "healthy proxy", code: ProxyHealthy, ok: true},
		{name: "healthy unknown status", code: ProxyHealthy, change: func(in *Input) { in.VM.Proxy.Status = "unknown" }, ok: true},
		{name: "healthy missing", code: ProxyHealthy, change: func(in *Input) { in.VM.Proxy = nil }, remedy: RemedyReproxy},
		{name: "healthy disabled", code: ProxyHealthy, change: func(in *Input) { in.VM.Proxy.Enabled = false }, remedy: RemedyReproxy, detail: []string{"proxy-target", "enabled=false"}},
		{name: "healthy dead", code: ProxyHealthy, change: func(in *Input) { in.VM.Proxy.Status = "dead" }, remedy: RemedyReproxy},
		{name: "healthy fail", code: ProxyHealthy, change: func(in *Input) { in.VM.Proxy.Status = "fail" }, remedy: RemedyReproxy},
		{name: "healthy blocked", code: ProxyHealthy, change: func(in *Input) { in.VM.Proxy.BlockedReason = "blocked upstream" }, remedy: RemedyReproxy, detail: []string{"blocked upstream"}},
		{name: "exclusive proxy", code: ProxyExclusive, ok: true},
		{name: "exclusive collision", code: ProxyExclusive, change: func(in *Input) { in.Peers[0].Proxy.ID = in.VM.Proxy.ID }, remedy: RemedyReproxy, detail: []string{"proxy-target", "vm-peer"}},
		{name: "exclusive all collisions", code: ProxyExclusive, change: func(in *Input) {
			in.Peers[0].Proxy.ID = in.VM.Proxy.ID
			in.Peers = append(in.Peers, vm2api.VM{ID: "vm-other", Proxy: &vm2api.ProxyView{ID: in.VM.Proxy.ID}})
		}, remedy: RemedyReproxy, detail: []string{"vm-peer", "vm-other"}},
		{name: "exclusive missing target proxy", code: ProxyExclusive, change: func(in *Input) { in.VM.Proxy = nil }, ok: true},
		{name: "exclusive empty ids ignored", code: ProxyExclusive, change: func(in *Input) { in.VM.Proxy.ID = ""; in.Peers[0].Proxy.ID = "" }, ok: true},
		{name: "exclusive missing peer proxy", code: ProxyExclusive, change: func(in *Input) { in.Peers[0].Proxy = nil }, ok: true},
		{name: "probe success", code: ExitProbe, ok: true},
		{name: "probe error with ip", code: ExitProbe, change: func(in *Input) { in.ProbeErr = errors.New("probe unavailable") }, remedy: RemedyReproxy, detail: []string{"probe unavailable", "vm-target", "203.0.113.10"}},
		{name: "probe no ip", code: ExitProbe, change: func(in *Input) { in.Probe.IP = "" }, remedy: RemedyReproxy},
		{name: "ip unique", code: ExitIPUnique, ok: true},
		{name: "ip cached collision", code: ExitIPUnique, change: func(in *Input) { in.Peers[0].Proxy.GeoIP = in.Probe.IP }, remedy: RemedyReproxy, detail: []string{"203.0.113.10", "vm-peer"}},
		{name: "ip observed collision", code: ExitIPUnique, change: func(in *Input) { in.PeerExitIPs["vm-peer"] = in.Probe.IP }, remedy: RemedyReproxy},
		{name: "ip observed takes precedence", code: ExitIPUnique, change: func(in *Input) {
			in.Peers[0].Proxy.GeoIP = in.Probe.IP
			in.PeerExitIPs["vm-peer"] = "203.0.113.30"
		}, ok: true},
		{name: "ip target geo fallback", code: ExitIPUnique, change: func(in *Input) { in.Probe.IP = ""; in.Peers[0].Proxy.GeoIP = in.VM.Proxy.GeoIP }, remedy: RemedyReproxy},
		{name: "ip probe overrides geo", code: ExitIPUnique, change: func(in *Input) { in.VM.Proxy.GeoIP = in.Peers[0].Proxy.GeoIP }, ok: true},
		{name: "ip empty history falls back", code: ExitIPUnique, change: func(in *Input) { in.PeerExitIPs["vm-peer"] = ""; in.Peers[0].Proxy.GeoIP = in.Probe.IP }, remedy: RemedyReproxy},
		{name: "ip history without peer proxy", code: ExitIPUnique, change: func(in *Input) { in.Peers[0].Proxy = nil; in.PeerExitIPs["vm-peer"] = in.Probe.IP }, remedy: RemedyReproxy},
		{name: "ip unavailable", code: ExitIPUnique, change: func(in *Input) { in.Probe.IP = ""; in.VM.Proxy.GeoIP = "" }, skipped: true},
		{name: "ip missing proxy", code: ExitIPUnique, change: func(in *Input) { in.Probe.IP = ""; in.VM.Proxy = nil }, skipped: true},
		{name: "timezone match", code: ExitGeoTimezone, ok: true},
		{name: "timezone disabled", code: ExitGeoTimezone, change: func(in *Input) { in.Rules.RequireTimezoneMatch = false; in.VM.Timezone = "Europe/Paris" }, skipped: true},
		{name: "timezone unknown exit", code: ExitGeoTimezone, change: func(in *Input) { in.Probe.Timezone = ""; in.VM.Proxy.GeoTimezone = "" }, skipped: true},
		{name: "timezone no proxy", code: ExitGeoTimezone, change: func(in *Input) { in.Probe.Timezone = ""; in.VM.Proxy = nil }, skipped: true},
		{name: "timezone missing slot", code: ExitGeoTimezone, change: func(in *Input) { in.VM.Timezone = ""; in.VM.Fingerprint.Timezone = "" }, remedy: RemedyReproxy},
		{name: "timezone mismatch", code: ExitGeoTimezone, change: func(in *Input) { in.VM.Timezone = "Europe/Paris" }, remedy: RemedyReproxy, detail: []string{"vm-target", "Europe/Paris", "Asia/Tokyo"}, message: "重新分配代理"},
		{name: "timezone fingerprint fallback", code: ExitGeoTimezone, change: func(in *Input) { in.VM.Timezone = "" }, ok: true},
		{name: "timezone fingerprint mismatch", code: ExitGeoTimezone, change: func(in *Input) { in.VM.Timezone = ""; in.VM.Fingerprint.Timezone = "Europe/Paris" }, remedy: RemedyReproxy},
		{name: "timezone proxy fallback", code: ExitGeoTimezone, change: func(in *Input) { in.Probe.Timezone = "" }, ok: true},
		{name: "timezone probe preferred", code: ExitGeoTimezone, change: func(in *Input) { in.VM.Proxy.GeoTimezone = "Europe/Paris" }, ok: true},
		{name: "timezone slot preferred", code: ExitGeoTimezone, change: func(in *Input) { in.VM.Fingerprint.Timezone = "Europe/Paris" }, ok: true},
		{name: "fingerprint complete", code: FingerprintPresent, ok: true},
		{name: "fingerprint no device", code: FingerprintPresent, change: func(in *Input) { in.VM.Fingerprint.DeviceID = "" }, remedy: RemedyReset, detail: []string{"device_id"}},
		{name: "fingerprint no machine", code: FingerprintPresent, change: func(in *Input) { in.VM.Fingerprint.GuestMachineID = "" }, remedy: RemedyReset, detail: []string{"guest_machine_id"}},
		{name: "fingerprint no hostname", code: FingerprintPresent, change: func(in *Input) { in.VM.Fingerprint.Hostname = "" }, remedy: RemedyReset, detail: []string{"hostname"}},
		{name: "fingerprint collection failed", code: FingerprintPresent, change: func(in *Input) {
			in.VM.Fingerprint = vm2api.Fingerprint{}
			in.CollectErr = errors.New("collect unavailable")
		}, remedy: RemedyReset, message: "无法采集槽位指纹", detail: []string{"collect unavailable", "device_id", "guest_machine_id", "hostname"}},
		{name: "fingerprint retained after collection error", code: FingerprintPresent, change: func(in *Input) { in.CollectErr = errors.New("collect unavailable") }, ok: true},
		{name: "fingerprint unique", code: FingerprintUnique, ok: true},
		{name: "fingerprint device collision", code: FingerprintUnique, change: func(in *Input) { in.Peers[0].Fingerprint.DeviceID = in.VM.Fingerprint.DeviceID }, remedy: RemedyReset, detail: []string{"device_id=device-target", "vm-peer"}},
		{name: "fingerprint machine collision", code: FingerprintUnique, change: func(in *Input) { in.Peers[0].Fingerprint.GuestMachineID = in.VM.Fingerprint.GuestMachineID }, remedy: RemedyReset, detail: []string{"guest_machine_id=machine-target", "vm-peer"}},
		{name: "fingerprint hostname collision", code: FingerprintUnique, change: func(in *Input) { in.Peers[0].Fingerprint.Hostname = in.VM.Fingerprint.Hostname }, remedy: RemedyReset, detail: []string{"hostname=host-target", "vm-peer"}},
		{name: "fingerprint multiple conflicts", code: FingerprintUnique, change: func(in *Input) {
			in.Peers[0].Fingerprint.DeviceID = in.VM.Fingerprint.DeviceID
			in.Peers = append(in.Peers, vm2api.VM{ID: "vm-other", Fingerprint: vm2api.Fingerprint{Hostname: in.VM.Fingerprint.Hostname}})
		}, remedy: RemedyReset, detail: []string{"device_id", "vm-peer", "hostname", "vm-other"}},
		{name: "fingerprint empty values ignored", code: FingerprintUnique, change: func(in *Input) {
			in.VM.Fingerprint = vm2api.Fingerprint{}
			in.Peers[0].Fingerprint = vm2api.Fingerprint{}
		}, ok: true},
		{name: "fingerprint different keys ignored", code: FingerprintUnique, change: func(in *Input) { in.Peers[0].Fingerprint.Hostname = in.VM.Fingerprint.DeviceID }, ok: true},
		{name: "account unique", code: AccountUnique, ok: true},
		{name: "account disabled", code: AccountUnique, change: func(in *Input) { in.CheckAccount = false; in.Peers[0].AccountUUID = in.VM.AccountUUID }, skipped: true},
		{name: "account not signed in", code: AccountUnique, change: func(in *Input) { in.VM.AccountUUID = ""; in.VM.Email = "" }, skipped: true},
		{name: "account uuid collision", code: AccountUnique, change: func(in *Input) { in.Peers[0].AccountUUID = in.VM.AccountUUID }, remedy: RemedyManual, detail: []string{"account-target", "vm-peer"}, message: "该账号已在其它槽位托管"},
		{name: "account email collision", code: AccountUnique, change: func(in *Input) {
			in.VM.AccountUUID = ""
			in.VM.Email = "User@Example.COM"
			in.Peers[0].AccountUUID = ""
			in.Peers[0].Email = "user@example.com"
		}, remedy: RemedyManual, detail: []string{"user@example.com", "vm-peer"}},
		{name: "account uuid preferred", code: AccountUnique, change: func(in *Input) { in.Peers[0].Email = in.VM.Email }, ok: true},
		{name: "account peer uuid preferred", code: AccountUnique, change: func(in *Input) { in.VM.AccountUUID = ""; in.Peers[0].Email = in.VM.Email }, ok: true},
		{name: "account peer unsigned", code: AccountUnique, change: func(in *Input) { in.Peers[0].AccountUUID = ""; in.Peers[0].Email = "" }, ok: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := healthyInput()
			if test.change != nil {
				test.change(&in)
			}
			result := Evaluate(in)
			check := findCheck(t, result, test.code)
			if check.OK != test.ok || check.Skipped != test.skipped || check.Remedy != test.remedy {
				t.Fatalf("check = %+v; want ok=%t, skipped=%t, remedy=%s", check, test.ok, test.skipped, test.remedy)
			}
			if check.Message == "" || !strings.Contains(check.Message, test.message) {
				t.Errorf("message = %q, want nonempty and containing %q", check.Message, test.message)
			}
			for _, detail := range test.detail {
				if !strings.Contains(check.Detail, detail) {
					t.Errorf("detail = %q, want containing %q", check.Detail, detail)
				}
			}
			wantPassed := true
			for _, item := range result.Checks {
				if !item.OK && !item.Skipped {
					wantPassed = false
				}
				for _, secret := range []string{"vm-target", "vm-peer", "proxy-target", "203.0.113.10", "target@example.com"} {
					if strings.Contains(item.Message, secret) {
						t.Errorf("message leaks administrator detail: %q", item.Message)
					}
				}
			}
			if result.Passed != wantPassed {
				t.Errorf("passed = %t, want %t", result.Passed, wantPassed)
			}
		})
	}
}

func TestEvaluateSkippedRulesDoNotFail(t *testing.T) {
	in := healthyInput()
	in.CheckAccount = false
	in.Rules.RequireTimezoneMatch = false
	result := Evaluate(in)
	if !result.Passed || len(result.Failed()) != 0 || result.Remedy() != RemedyNone {
		t.Fatalf("skipped rules affect pass: %+v", result)
	}
}

func TestResultRemedy(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		want   Remedy
	}{
		{name: "empty", want: RemedyNone},
		{name: "passed ignored", checks: []Check{{OK: true, Remedy: RemedyManual}}, want: RemedyNone},
		{name: "skipped ignored", checks: []Check{{Skipped: true, Remedy: RemedyManual}}, want: RemedyNone},
		{name: "reset", checks: []Check{{Remedy: RemedyReset}}, want: RemedyReset},
		{name: "reproxy over reset", checks: []Check{{Remedy: RemedyReproxy}, {Remedy: RemedyReset}}, want: RemedyReproxy},
		{name: "manual over reproxy", checks: []Check{{Remedy: RemedyReset}, {Remedy: RemedyManual}, {Remedy: RemedyReproxy}}, want: RemedyManual},
		{name: "skipped manual ignored", checks: []Check{{Skipped: true, Remedy: RemedyManual}, {Remedy: RemedyReproxy}}, want: RemedyReproxy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Checks: test.checks}
			if got := result.Remedy(); got != test.want {
				t.Errorf("Remedy() = %q, want %q", got, test.want)
			}
			if test.want != RemedyNone && !result.Needs(test.want) {
				t.Errorf("Needs(%q) = false", test.want)
			}
		})
	}
	result := Result{Checks: []Check{{Code: ProxyBound, Remedy: RemedyReproxy}, {Code: AccountUnique, Skipped: true, Remedy: RemedyManual}, {OK: true, Remedy: RemedyReset}}}
	if got := result.Failed(); len(got) != 1 || got[0].Code != ProxyBound {
		t.Errorf("Failed() = %+v", got)
	}
	if result.Needs(RemedyManual) || result.Needs(RemedyReset) {
		t.Fatal("Needs includes passed or skipped checks")
	}
}
