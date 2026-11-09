package buildinfo

import "testing"

func TestVersion(t *testing.T) {
	previous := currentVersion.Load()
	t.Cleanup(func() { currentVersion.Store(previous) })
	currentVersion.Store(nil)
	if got := Version(); got != "dev" {
		t.Fatalf("default version = %q", got)
	}
	for _, tc := range []struct{ input, want string }{
		{"v0.9.1", "v0.9.1"},
		{" v0.9.1-2-gabcdef ", "v0.9.1-2-gabcdef"},
		{"", "dev"},
		{" \n", "dev"},
	} {
		SetVersion(tc.input)
		if got := Version(); got != tc.want {
			t.Errorf("version %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}
