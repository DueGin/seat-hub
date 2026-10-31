package selfcheck

import (
	"fmt"
	"strings"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func Evaluate(in Input) Result {
	exitIP := in.Probe.IP
	proxyID := ""
	if in.VM.Proxy != nil {
		proxyID = in.VM.Proxy.ID
		if exitIP == "" {
			exitIP = in.VM.Proxy.GeoIP
		}
	}
	result := Result{
		VMID: in.VM.ID, ExitIP: exitIP, ProxyID: proxyID, CheckedAt: in.Now,
		Checks: []Check{
			checkProxyBound(in), checkProxyHealthy(in), checkProxyExclusive(in),
			checkExitProbe(in), checkExitIPUnique(in, exitIP), checkExitTimezone(in),
			checkFingerprintPresent(in), checkFingerprintUnique(in), checkAccountUnique(in),
		},
		Passed: true,
	}
	result.Passed = len(result.Failed()) == 0
	return result
}

func checkProxyBound(in Input) Check {
	check := Check{Code: ProxyBound, OK: true, Message: "槽位已绑定独立出口代理"}
	proxy := in.VM.Proxy
	if proxy == nil || !in.VM.ProxyConfigured {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "槽位尚未绑定出口代理"
	} else if in.Rules.RequireDedicatedProxy && proxy.Scheme != "socks5" {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "共享出口不满足一 IP 一槽，需要重新分配独立代理"
	} else if !in.Rules.RequireDedicatedProxy {
		check.Message = "槽位已绑定出口代理，未要求独立出口"
	}
	if !check.OK {
		check.Detail = fmt.Sprintf("vm_id=%s, proxy_configured=%t", in.VM.ID, in.VM.ProxyConfigured)
		if proxy != nil {
			check.Detail += fmt.Sprintf(", proxy_id=%s, scheme=%s", proxy.ID, proxy.Scheme)
		}
	}
	return check
}

func checkProxyHealthy(in Input) Check {
	check := Check{Code: ProxyHealthy, OK: true, Message: "出口代理状态正常"}
	proxy := in.VM.Proxy
	if proxy == nil {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "槽位未绑定可用的出口代理"
		check.Detail = fmt.Sprintf("vm_id=%s, proxy=nil", in.VM.ID)
	} else if !proxy.Enabled || proxy.Status == "dead" || proxy.Status == "fail" || proxy.BlockedReason != "" {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "出口代理不可用，需要重新分配"
		check.Detail = fmt.Sprintf("proxy_id=%s, enabled=%t, status=%s, blocked_reason=%s",
			proxy.ID, proxy.Enabled, proxy.Status, proxy.BlockedReason)
	}
	return check
}

func checkProxyExclusive(in Input) Check {
	check := Check{Code: ProxyExclusive, OK: true, Message: "出口代理未被其它槽位共用"}
	if in.VM.Proxy == nil || in.VM.Proxy.ID == "" {
		return check
	}
	var conflicts []string
	for _, peer := range in.Peers {
		if peer.Proxy != nil && peer.Proxy.ID == in.VM.Proxy.ID {
			conflicts = append(conflicts, peer.ID)
		}
	}
	if len(conflicts) > 0 {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "出口代理已被其它槽位共用，需要重新分配"
		check.Detail = fmt.Sprintf("proxy_id=%s, peer_vm_ids=%s", in.VM.Proxy.ID, strings.Join(conflicts, ","))
	}
	return check
}

func checkExitProbe(in Input) Check {
	check := Check{Code: ExitProbe, OK: true, Message: "出口连通性探测通过"}
	if in.ProbeErr != nil || in.Probe.IP == "" {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "无法探测槽位出口，需要重新分配代理"
		check.Detail = fmt.Sprintf("vm_id=%s, exit_ip=%s", in.VM.ID, in.Probe.IP)
		if in.ProbeErr != nil {
			check.Detail += fmt.Sprintf(", error=%v", in.ProbeErr)
		}
	}
	return check
}

func checkExitIPUnique(in Input, exitIP string) Check {
	check := Check{Code: ExitIPUnique, OK: true, Message: "出口 IP 未被其它槽位使用"}
	if exitIP == "" {
		check.OK, check.Skipped, check.Message = false, true, "未获取到出口 IP，跳过出口唯一性检查"
		return check
	}
	var conflicts []string
	for _, peer := range in.Peers {
		peerIP := in.PeerExitIPs[peer.ID]
		if peerIP == "" && peer.Proxy != nil {
			peerIP = peer.Proxy.GeoIP
		}
		if peerIP == exitIP {
			conflicts = append(conflicts, peer.ID)
		}
	}
	if len(conflicts) > 0 {
		check.OK, check.Remedy, check.Message = false, RemedyReproxy, "出口 IP 已被其它槽位使用，需要重新分配代理"
		check.Detail = fmt.Sprintf("exit_ip=%s, peer_vm_ids=%s", exitIP, strings.Join(conflicts, ","))
	}
	return check
}

func checkExitTimezone(in Input) Check {
	check := Check{Code: ExitGeoTimezone, OK: true, Message: "槽位时区与出口地理时区一致"}
	if !in.Rules.RequireTimezoneMatch {
		check.OK, check.Skipped, check.Message = false, true, "未要求时区匹配，跳过检查"
		return check
	}
	exitTimezone := in.Probe.Timezone
	if exitTimezone == "" && in.VM.Proxy != nil {
		exitTimezone = in.VM.Proxy.GeoTimezone
	}
	if exitTimezone == "" {
		check.OK, check.Skipped, check.Message = false, true, "未获取到出口地理时区，跳过检查"
		return check
	}
	slotTimezone := in.VM.Timezone
	if slotTimezone == "" {
		slotTimezone = in.VM.Fingerprint.Timezone
	}
	if slotTimezone == "" || slotTimezone != exitTimezone {
		check.OK, check.Remedy = false, RemedyReproxy
		check.Message = "槽位时区与出口不一致，需要重新分配代理以同步时区"
		if slotTimezone == "" {
			check.Message = "槽位时区缺失，需要重新分配代理以同步时区"
		}
		check.Detail = fmt.Sprintf("vm_id=%s, slot_timezone=%s, exit_timezone=%s", in.VM.ID, slotTimezone, exitTimezone)
	}
	return check
}

func fingerprintKeys(fingerprint vm2api.Fingerprint) []struct{ name, value string } {
	return []struct{ name, value string }{
		{"device_id", fingerprint.DeviceID},
		{"guest_machine_id", fingerprint.GuestMachineID},
		{"hostname", fingerprint.Hostname},
	}
}

func checkFingerprintPresent(in Input) Check {
	check := Check{Code: FingerprintPresent, OK: true, Message: "槽位设备指纹完整"}
	var missing []string
	for _, key := range fingerprintKeys(in.VM.Fingerprint) {
		if key.value == "" {
			missing = append(missing, key.name)
		}
	}
	if len(missing) > 0 {
		check.OK, check.Remedy, check.Message = false, RemedyReset, "槽位设备指纹不完整，需要重建指纹"
		check.Detail = fmt.Sprintf("vm_id=%s, missing=%s", in.VM.ID, strings.Join(missing, ","))
		if in.CollectErr != nil {
			check.Message = "无法采集槽位指纹，需要重建指纹"
			check.Detail += fmt.Sprintf(", error=%v", in.CollectErr)
		}
	}
	return check
}

func checkFingerprintUnique(in Input) Check {
	check := Check{Code: FingerprintUnique, OK: true, Message: "槽位设备指纹未与其它槽位重复"}
	var conflicts []string
	keys := fingerprintKeys(in.VM.Fingerprint)
	for _, peer := range in.Peers {
		peerKeys := fingerprintKeys(peer.Fingerprint)
		for index, key := range keys {
			if key.value != "" && key.value == peerKeys[index].value {
				conflicts = append(conflicts, fmt.Sprintf("%s=%s, peer_vm_id=%s", key.name, key.value, peer.ID))
			}
		}
	}
	if len(conflicts) > 0 {
		check.OK, check.Remedy, check.Message = false, RemedyReset, "槽位设备指纹与其它槽位重复，需要重建指纹"
		check.Detail = strings.Join(conflicts, "; ")
	}
	return check
}

func accountKey(vm vm2api.VM) string {
	if vm.AccountUUID != "" {
		return vm.AccountUUID
	}
	return strings.ToLower(vm.Email)
}

func checkAccountUnique(in Input) Check {
	check := Check{Code: AccountUnique, OK: true, Message: "该账号未在其它槽位托管"}
	if !in.CheckAccount {
		check.OK, check.Skipped, check.Message = false, true, "未启用账号唯一性检查"
		return check
	}
	key := accountKey(in.VM)
	if key == "" {
		check.OK, check.Skipped, check.Message = false, true, "槽位尚未登录账号，跳过账号唯一性检查"
		return check
	}
	var conflicts []string
	for _, peer := range in.Peers {
		if key == accountKey(peer) {
			conflicts = append(conflicts, peer.ID)
		}
	}
	if len(conflicts) > 0 {
		check.OK, check.Remedy, check.Message = false, RemedyManual, "该账号已在其它槽位托管，需要人工处理"
		check.Detail = fmt.Sprintf("account_key=%s, peer_vm_ids=%s", key, strings.Join(conflicts, ","))
	}
	return check
}
