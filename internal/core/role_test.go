package core

import (
	"errors"
	"testing"
)

func TestRoleAllows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		have, need Role
		want       bool
	}{
		{have: RoleViewer, need: RoleViewer, want: true},
		{have: RoleViewer, need: RoleReporter},
		{have: RoleViewer, need: RoleDeployer},
		{have: RoleViewer, need: RoleAdmin},
		{have: RoleReporter, need: RoleViewer, want: true},
		{have: RoleReporter, need: RoleReporter, want: true},
		{have: RoleReporter, need: RoleDeployer},
		{have: RoleReporter, need: RoleAdmin},
		{have: RoleDeployer, need: RoleViewer, want: true},
		{have: RoleDeployer, need: RoleReporter, want: true},
		{have: RoleDeployer, need: RoleDeployer, want: true},
		{have: RoleDeployer, need: RoleAdmin},
		{have: RoleAdmin, need: RoleViewer, want: true},
		{have: RoleAdmin, need: RoleReporter, want: true},
		{have: RoleAdmin, need: RoleDeployer, want: true},
		{have: RoleAdmin, need: RoleAdmin, want: true},
		{have: "", need: RoleViewer},
		{have: "root", need: RoleViewer},
		{have: RoleAdmin, need: ""},
		{have: RoleAdmin, need: "root"},
		{have: "Admin", need: RoleViewer},
	}
	for _, tt := range tests {
		t.Run(string(tt.have)+">="+string(tt.need), func(t *testing.T) {
			t.Parallel()
			if got := tt.have.Allows(tt.need); got != tt.want {
				t.Errorf("%q.Allows(%q) = %v, want %v", tt.have, tt.need, got, tt.want)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{in: "viewer", want: RoleViewer},
		{in: "reporter", want: RoleReporter},
		{in: "deployer", want: RoleDeployer},
		{in: "admin", want: RoleAdmin},
		{in: "", wantErr: true},
		{in: "Admin", wantErr: true},
		{in: " admin", wantErr: true},
		{in: "owner", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRole(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrUnknownRole) {
					t.Errorf("ParseRole(%q) error = %v, want ErrUnknownRole", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ParseRole(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestRolesAreOrderedAndUnique(t *testing.T) {
	t.Parallel()

	roles := Roles()
	for i, r := range roles {
		if !r.Valid() {
			t.Errorf("%q is not valid", r)
		}
		for _, lower := range roles[:i] {
			if lower == r {
				t.Errorf("duplicate role %q", r)
			}
			if lower.Allows(r) {
				t.Errorf("%q allows the higher role %q", lower, r)
			}
			if !r.Allows(lower) {
				t.Errorf("%q does not allow the lower role %q", r, lower)
			}
		}
	}
}
