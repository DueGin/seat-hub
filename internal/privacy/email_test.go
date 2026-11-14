package privacy

import "testing"

func TestMaskEmail(t *testing.T) {
	for _, testcase := range []struct{ in, want string }{
		{"fdsiome@protonmail.com", "fds***@***.com"},
		{"FDSiome@ProtonMail.COM", "FDS***@***.com"},
		{"jsonuiw@proton.me", "jso***@***.me"},
		{"rental001@example.com", "ren***@***.com"},
		{"a.b.c@mail.example.co.uk", "a.***@***.uk"},
		// Short mailboxes never show more than half.
		{"abcd@x.io", "ab***@***.io"},
		{"abc@x.io", "a***@***.io"},
		{"ab@x.io", "a***@***.io"},
		{"a@x.io", "***@***.io"},
		// Already masked by vm2api or stored in the old format: tighten it.
		{"f***@protonmail.com", "f***@***.com"},
		{"ren***001@example.com", "ren***@***.com"},
		{"cla***000005@example.com", "cla***@***.com"},
		{"us***@gmail.com", "us***@***.com"},
		{"fds***@***.com", "fds***@***.com"},
		{"***@***.com", "***@***.com"},
		// Odd input.
		{"", ""},
		{"   ", ""},
		{"no-at-sign", "***"},
		{"user@localhost", "us***@***"},
		{"user@domain.", "us***@***"},
		{"中文邮箱名@例子.公司", "中文***@***.公司"},
	} {
		if got := MaskEmail(testcase.in); got != testcase.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", testcase.in, got, testcase.want)
		}
		if again := MaskEmail(MaskEmail(testcase.in)); again != MaskEmail(testcase.in) {
			t.Errorf("MaskEmail is not stable for %q: %q", testcase.in, again)
		}
	}
}
