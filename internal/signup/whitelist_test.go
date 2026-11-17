package signup

import (
	"reflect"
	"strings"
	"testing"
)

func TestWhitelist(t *testing.T) {
	for _, tt := range []struct {
		name, email string
		entries     []string
		want        bool
	}{
		{"empty", "any@example.com", nil, true},
		{"exact domain", "a@qq.com", []string{"qq.com"}, true},
		{"domain subdomain", "a@mail.qq.com", []string{"qq.com"}, false},
		{"wildcard", "a@school.edu.cn", []string{"*.edu.cn"}, true},
		{"nested wildcard", "a@mail.school.edu.cn", []string{"*.edu.cn"}, true},
		{"wildcard apex", "a@edu.cn", []string{"*.edu.cn"}, false},
		{"wildcard suffix", "a@notedu.cn", []string{"*.edu.cn"}, false},
		{"exact address", "someone@gmail.com", []string{"someone@gmail.com"}, true},
		{"other address", "other@gmail.com", []string{"someone@gmail.com"}, false},
		{"case and trim", " Someone@GMAIL.COM ", []string{" SOMEONE@gmail.com "}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allowed(tt.email, tt.entries); got != tt.want {
				t.Fatalf("Allowed=%v want %v", got, tt.want)
			}
		})
	}
	entries, err := NormalizeWhitelist([]string{" QQ.com ", "", "qq.COM", "*.edu.cn", " Someone@Gmail.com "})
	if err != nil || !reflect.DeepEqual(entries, []string{"qq.com", "*.edu.cn", "someone@gmail.com"}) {
		t.Fatalf("normalize=%v err=%v", entries, err)
	}
	if !reflect.DeepEqual(Domains(entries), []string{"qq.com", "*.edu.cn"}) {
		t.Fatal("address leaked")
	}
	for _, bad := range []string{"localhost", "*.cn", "a.*.com", "*qq.com", "-bad.com", "bad-.com", "a..com", "a_b.com", ".qq.com", "qq.com.", "https://qq.com", "Alice <alice@gmail.com>", "a@gmail.com,b@gmail.com", "a@x.com\nBcc:b@x.com", strings.Repeat("a", 64) + ".com"} {
		if _, err := NormalizeWhitelist([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	tooMany := []string{}
	for i := 0; i < 201; i++ {
		tooMany = append(tooMany, strings.Repeat("a", i/60+1)+string(rune('a'+i%26))+string(rune('a'+i/26))+".com")
	}
	if _, err := NormalizeWhitelist(tooMany); err == nil {
		t.Fatal("accepted over 200 entries")
	}
}
