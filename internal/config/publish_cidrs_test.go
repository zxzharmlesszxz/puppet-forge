package config

import (
	"strings"
	"testing"
)

func TestPublishAllowedCIDRsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, env, flag string
		invalid         bool
	}{
		{name: "default"},
		{name: "whitespace only", env: " \t\r\n"},
		{name: "IPv4 and IPv6", env: "192.0.2.7/24, 2001:db8::/32"},
		{name: "whitespace separators", env: "192.0.2.7/32\r\n2001:db8::7/128"},
		{name: "flag overrides env", env: "invalid", flag: "198.51.100.0/24"},
		{name: "invalid", env: "invalid", invalid: true},
		{name: "bare IP", env: "192.0.2.1", invalid: true},
		{name: "wildcard", env: "*", invalid: true},
		{name: "separators only", env: ", ,", invalid: true},
		{name: "mapped IPv4", env: "::ffff:192.0.2.0/120", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
			t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
			t.Setenv("ARTIFACT_PROJECT", "local-dev")
			t.Setenv("PUBLISH_ALLOWED_CIDRS", tc.env)
			var args []string
			want := tc.env
			if tc.flag != "" {
				args = []string{"--publish-allowed-cidrs", tc.flag}
				want = tc.flag
			}
			cfg, err := LoadArgs(args)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "PUBLISH_ALLOWED_CIDRS") {
					t.Fatalf("LoadArgs() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PublishAllowedCIDRs != want {
				t.Fatalf("PublishAllowedCIDRs = %q, want %q", cfg.PublishAllowedCIDRs, want)
			}
			prefixes, err := ParsePublishAllowedCIDRs(cfg.PublishAllowedCIDRs)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "IPv4 and IPv6" && (len(prefixes) != 2 || prefixes[0].String() != "192.0.2.0/24") {
				t.Fatalf("parsed prefixes = %v", prefixes)
			}
		})
	}
}
