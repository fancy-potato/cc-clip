package userhome

import (
	"os/user"
	"testing"
)

// fakeResolver is a test double for Resolver. Each field controls one
// of the two stdlib calls Dir depends on; unset fields panic to surface
// accidental call paths the test didn't mean to exercise.
type fakeResolver struct {
	lookup func(string) (*user.User, error)
	home   func() (string, error)
	sudo   func() bool
}

func (f fakeResolver) LookupUser(name string) (*user.User, error) {
	if f.lookup == nil {
		panic("fakeResolver.LookupUser called without lookup set")
	}
	return f.lookup(name)
}

func (f fakeResolver) UserHomeDir() (string, error) {
	if f.home == nil {
		panic("fakeResolver.UserHomeDir called without home set")
	}
	return f.home()
}

func (f fakeResolver) IsSudoRoot() bool {
	if f.sudo == nil {
		return false
	}
	return f.sudo()
}

func TestDirUsesSUDOUserHomeOnlyInSudoRootContext(t *testing.T) {
	t.Run("sudo_root_uses_sudo_user_home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("SUDO_USER", "alice")
		t.Setenv("SUDO_UID", "501")
		SetResolverForTest(t, fakeResolver{
			lookup: func(name string) (*user.User, error) {
				if name != "alice" {
					t.Fatalf("LookupUser(%q), want alice", name)
				}
				return &user.User{Username: "alice", Uid: "501", HomeDir: home}, nil
			},
			home: func() (string, error) {
				return "/var/root", nil
			},
			sudo: func() bool { return true },
		})

		got, err := Dir()
		if err != nil {
			t.Fatalf("Dir: %v", err)
		}
		if got != home {
			t.Fatalf("Dir = %q, want %q", got, home)
		}
	})

	t.Run("ordinary_shell_ignores_stale_sudo_user", func(t *testing.T) {
		t.Setenv("SUDO_USER", "alice")
		t.Setenv("SUDO_UID", "501")
		SetResolverForTest(t, fakeResolver{
			lookup: func(name string) (*user.User, error) {
				t.Fatalf("LookupUser(%q) should not be called outside sudo-root context", name)
				return nil, nil
			},
			home: func() (string, error) {
				return "/Users/current", nil
			},
			sudo: func() bool { return false },
		})

		got, err := Dir()
		if err != nil {
			t.Fatalf("Dir: %v", err)
		}
		if got != "/Users/current" {
			t.Fatalf("Dir = %q, want %q", got, "/Users/current")
		}
	})
}

// TestDirRefusesSUDOUserWhenSUDOUIDMismatches pins the cross-check
// against env tampering: a sudoers rule (or hand-set env) that keeps
// SUDO_USER pointing at one account while SUDO_UID belongs to another
// must not let cc-clip read or write that account's home. We fall back
// to UserHomeDir rather than honoring the mismatched mapping.
func TestDirRefusesSUDOUserWhenSUDOUIDMismatches(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")
	t.Setenv("SUDO_UID", "999") // claimed invoker
	SetResolverForTest(t, fakeResolver{
		lookup: func(name string) (*user.User, error) {
			// The looked-up alice's actual Uid disagrees with SUDO_UID.
			return &user.User{Username: "alice", Uid: "501", HomeDir: "/Users/alice"}, nil
		},
		home: func() (string, error) {
			return "/var/root", nil
		},
		sudo: func() bool { return true },
	})

	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if got != "/var/root" {
		t.Fatalf("Dir = %q, want fallback to UserHomeDir (/var/root) when SUDO_UID mismatches", got)
	}
}

// TestDirRefusesSUDOUserPointingAtRoot pins the no-root-fallback rule:
// even when SUDO_USER lookup succeeds and SUDO_UID matches, if the
// resolved Uid is 0 the fallback is meaningless (we'd read/write
// /root or /var/root either way) and could mask a tampered env that
// re-asserted root identity. Fall through to UserHomeDir.
func TestDirRefusesSUDOUserPointingAtRoot(t *testing.T) {
	t.Setenv("SUDO_USER", "root")
	t.Setenv("SUDO_UID", "0")
	SetResolverForTest(t, fakeResolver{
		lookup: func(name string) (*user.User, error) {
			return &user.User{Username: "root", Uid: "0", HomeDir: "/root"}, nil
		},
		home: func() (string, error) {
			return "/var/root", nil
		},
		sudo: func() bool { return true },
	})

	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if got != "/var/root" {
		t.Fatalf("Dir = %q, want UserHomeDir fallback (/var/root) when SUDO_USER resolves to uid 0", got)
	}
}

func TestSudoIdentityMatches(t *testing.T) {
	cases := []struct {
		name      string
		lookupUID string
		sudoUID   string
		want      bool
	}{
		{"both_empty", "", "", false},
		{"sudo_empty", "501", "", false},
		{"lookup_empty", "", "501", false},
		{"mismatch", "501", "999", false},
		{"match_root", "0", "0", false},
		{"match_non_root", "501", "501", true},
		{"match_high_uid", "100000", "100000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sudoIdentityMatches(tc.lookupUID, tc.sudoUID); got != tc.want {
				t.Fatalf("sudoIdentityMatches(%q, %q) = %v, want %v", tc.lookupUID, tc.sudoUID, got, tc.want)
			}
		})
	}
}
