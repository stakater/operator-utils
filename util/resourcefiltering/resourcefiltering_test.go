package resourcefiltering

import "testing"

func TestAllowPass(t *testing.T) {
	a := &Allow{Literal: []string{"team-a", "team-b"}}
	if !a.Pass("team-a") {
		t.Error("Allow.Pass(team-a) = false, want true")
	}
	if a.Pass("team-c") {
		t.Error("Allow.Pass(team-c) = true, want false")
	}
}

func TestDenyPass(t *testing.T) {
	d := &Deny{Literal: []string{"team-x", "team-y"}}
	if d.Pass("team-x") {
		t.Error("Deny.Pass(team-x) = true, want false")
	}
	if !d.Pass("team-a") {
		t.Error("Deny.Pass(team-a) = false, want true")
	}
}

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		name   string
		ad     *AllowDeny
		input  string
		expect bool
	}{
		{
			name:   "nil AllowDeny returns false",
			ad:     nil,
			input:  "anything",
			expect: false,
		},
		{
			name:   "both nil: everything passes",
			ad:     &AllowDeny{},
			input:  "anything",
			expect: true,
		},
		// Allow-only
		{
			name:   "allow only: name in allow returns true",
			ad:     &AllowDeny{Allow: &Allow{Literal: []string{"team-a", "team-b"}}},
			input:  "team-a",
			expect: true,
		},
		{
			name:   "allow only: name not in allow returns false",
			ad:     &AllowDeny{Allow: &Allow{Literal: []string{"team-a", "team-b"}}},
			input:  "team-c",
			expect: false,
		},
		// Deny-only
		{
			name:   "deny only: name in deny returns false",
			ad:     &AllowDeny{Deny: &Deny{Literal: []string{"team-x", "team-y"}}},
			input:  "team-x",
			expect: false,
		},
		{
			name:   "deny only: name not in deny returns true",
			ad:     &AllowDeny{Deny: &Deny{Literal: []string{"team-x", "team-y"}}},
			input:  "team-a",
			expect: true,
		},
		// Allow takes precedence over Deny
		{
			name:   "allow and deny: name in both, allow wins",
			ad:     &AllowDeny{Allow: &Allow{Literal: []string{"team-a"}}, Deny: &Deny{Literal: []string{"team-a"}}},
			input:  "team-a",
			expect: true,
		},
		{
			name:   "allow and deny: name only in deny, allow evaluated (deny ignored)",
			ad:     &AllowDeny{Allow: &Allow{Literal: []string{"team-a"}}, Deny: &Deny{Literal: []string{"team-b"}}},
			input:  "team-b",
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAllowed(tt.ad, tt.input)
			if got != tt.expect {
				t.Errorf("IsAllowed() = %v, want %v", got, tt.expect)
			}
		})
	}
}
