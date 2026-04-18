package tunnel

import "testing"

func TestMatchesTunnelProcess(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "myhost",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	// managedPrefix is the option sequence cc-clip's sshTunnelArgs emits. It
	// is what we allow cleanup to recognize as "ours"; anything short of this
	// must not be matched, or a PID that the OS recycled to an unrelated
	// `ssh -N -R …` invocation could be killed as if it were our tunnel.
	const managedPrefix = "-F /tmp/cc-clip-ssh-config-abc.conf -N -v " +
		"-o BatchMode=yes -o ExitOnForwardFailure=yes " +
		"-o ServerAliveInterval=15 -o ServerAliveCountMax=3 " +
		"-o ControlMaster=no -o ControlPath=none"

	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			// Full managed invocation — this is what the manager launches.
			name:    "managed invocation",
			cmdline: "/usr/bin/ssh " + managedPrefix + " -R 19001:127.0.0.1:18339 myhost",
			want:    true,
		},
		{
			name:    "managed invocation windows ssh exe",
			cmdline: `"C:\Windows\System32\OpenSSH\ssh.exe" ` + managedPrefix + " -R 19001:127.0.0.1:18339 myhost",
			want:    true,
		},
		{
			// Flattened ProxyCommand with the -F anchor still matches: the
			// anchor is load-bearing, not the absence of `-oProxyCommand=…`.
			name:    "flattened proxycommand with -F anchor still matches",
			cmdline: `/usr/bin/ssh -F /tmp/cc-clip-ssh-config-abc.conf -o ProxyCommand=ssh -W %h:%p bastion -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost`,
			want:    true,
		},
		{
			// Same managed prefix but wrong destination host — the tail
			// anchor check rejects it.
			name:    "managed wrong host does not match",
			cmdline: "/usr/bin/ssh " + managedPrefix + " -R 19001:127.0.0.1:18339 otherhost",
			want:    false,
		},
		{
			name:    "managed wrong forward port does not match",
			cmdline: "/usr/bin/ssh " + managedPrefix + " -R 19002:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			// Bare simple-form invocation a user might start by hand. The
			// prior matcher accepted this; the strict matcher must not, or
			// PID recycling could wrap an unrelated ssh in our cleanup.
			name:    "plain user-started ssh does not match",
			cmdline: "ssh -N -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			name:    "plain user-started windows ssh does not match",
			cmdline: `C:\Windows\System32\OpenSSH\ssh.exe -N -R 19001:127.0.0.1:18339 myhost`,
			want:    false,
		},
		{
			name:    "combined NR without -F anchor does not match",
			cmdline: "ssh -NR 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			// Near-miss: has -F pointing at a non-cc-clip config file.
			// Only the `cc-clip-ssh-config-` marker is a valid anchor.
			name:    "-F with non-cc-clip path does not match",
			cmdline: "/usr/bin/ssh -F /tmp/my-ssh.conf -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			// Decoy: the marker appears as a substring of the basename but
			// the file does NOT have the `cc-clip-ssh-config-*.conf` shape
			// `os.CreateTemp` produces. The tightened matcher rejects it so
			// an attacker-chosen path cannot masquerade as ours.
			name:    "-F with marker substring in basename does not match",
			cmdline: "/usr/bin/ssh -F /tmp/my-cc-clip-ssh-config-notes.txt -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			// Decoy: the marker is a directory name, not the filename. A
			// substring-only matcher would accept this path; the tightened
			// matcher requires the basename itself to match.
			name:    "-F with marker in directory name does not match",
			cmdline: "/tmp/cc-clip-ssh-config-scratch/evil.conf",
			want:    false,
		},
		{
			// Windows-native backslash path — filepath.Base on Unix does not
			// know about `\`, so the matcher has to check both separators.
			// This test pins the cross-platform basename handling.
			name:    "managed windows backslash path matches",
			cmdline: `C:\Windows\System32\OpenSSH\ssh.exe -F C:\Users\u\AppData\Local\Temp\cc-clip-ssh-config-abc.conf -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost`,
			want:    true,
		},
		{
			name:    "apostrophe in path does not break tokenization",
			cmdline: `/usr/bin/ssh -F /Users/O'Connor/.cache/cc-clip-ssh-config-abc.conf -N -v -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost`,
			want:    true,
		},
		{
			name:    "managed prefix missing one required -o does not match",
			cmdline: "/usr/bin/ssh -F /tmp/cc-clip-ssh-config-abc.conf -N -v -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			name:    "managed prefix missing -v does not match",
			cmdline: "/usr/bin/ssh -F /tmp/cc-clip-ssh-config-abc.conf -N -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ControlMaster=no -o ControlPath=none -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			name:    "not ssh",
			cmdline: "curl -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			name:    "ssh substring should not match",
			cmdline: "cssh " + managedPrefix + " -R 19001:127.0.0.1:18339 myhost",
			want:    false,
		},
		{
			name:    "empty",
			cmdline: "",
			want:    false,
		},
		{
			name:    "host as substring of trailing token does not match",
			cmdline: "/usr/bin/ssh " + managedPrefix + " -R 19001:127.0.0.1:18339 myhost-gateway",
			want:    false,
		},
		{
			name:    "raw forward substring in remote command should not match",
			cmdline: `ssh otherhost "echo 19001:127.0.0.1:18339"`,
			want:    false,
		},
		{
			// NUL-separated argv, as read from /proc/<pid>/cmdline on Linux.
			// The -F path intentionally contains a space to pin the behavior
			// that argv tokens with embedded whitespace survive the
			// tokenizer when the input uses NUL boundaries.
			name: "nul-separated argv with space in -F path matches",
			cmdline: "/usr/bin/ssh\x00-F\x00/home/user/with space/cc-clip-ssh-config-abc.conf\x00-N\x00-v\x00" +
				"-o\x00BatchMode=yes\x00-o\x00ExitOnForwardFailure=yes\x00" +
				"-o\x00ServerAliveInterval=15\x00-o\x00ServerAliveCountMax=3\x00" +
				"-o\x00ControlMaster=no\x00-o\x00ControlPath=none\x00" +
				"-R\x0019001:127.0.0.1:18339\x00myhost",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesTunnelProcess(tt.cmdline, cfg)
			if got != tt.want {
				t.Errorf("matchesTunnelProcess(%q, cfg) = %v, want %v", tt.cmdline, got, tt.want)
			}
		})
	}
}
