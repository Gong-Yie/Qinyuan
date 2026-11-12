package model

import "testing"

func TestNormalizeApprovalMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", ApprovalModeInherit, true},
		{"inherit", ApprovalModeInherit, true},
		{" AUTO ", ApprovalModeAuto, true},
		{"manual", ApprovalModeManual, true},
		{"off", ApprovalModeOff, true},
		{"bypass", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeApprovalMode(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("NormalizeApprovalMode(%q) = (%q,%v)，期望 (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestEffectiveApprovalMode(t *testing.T) {
	cases := []struct {
		run, def, want string
	}{
		{"", "auto", "auto"},
		{"", "", "manual"},
		{"off", "auto", "off"},
		{"manual", "auto", "manual"},
		{"bogus", "auto", "auto"},
		{"", "bogus", "manual"},
	}
	for _, c := range cases {
		if got := EffectiveApprovalMode(c.run, c.def); got != c.want {
			t.Fatalf("EffectiveApprovalMode(%q,%q)=%q，期望 %q", c.run, c.def, got, c.want)
		}
	}
}
