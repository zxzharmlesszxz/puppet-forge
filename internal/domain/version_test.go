package domain

import "testing"

func TestValidModuleVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"1.2.3", "1.2.3-rc.1", "1.2.3+build.4", "1.2.3-rc.1+build.4"} {
		if !ValidModuleVersion(version) {
			t.Errorf("ValidModuleVersion(%q) = false", version)
		}
	}
	for _, version := range []string{"", "1", "1.2", "v1.2.3", "1.a.0", "01.2.3", "1.2.3-01", " 1.2.3"} {
		if ValidModuleVersion(version) {
			t.Errorf("ValidModuleVersion(%q) = true", version)
		}
	}
}

func TestCompareVersionsOrdersInvalidValuesDeterministically(t *testing.T) {
	t.Parallel()

	if got := CompareVersions("1.a.0", "1.0.0"); got >= 0 {
		t.Fatalf("CompareVersions(invalid, valid) = %d, want invalid lower", got)
	}
	if got := CompareVersions("1.0.0", "1.a.0"); got <= 0 {
		t.Fatalf("CompareVersions(valid, invalid) = %d, want valid higher", got)
	}
	if got := CompareVersions("dirty-b", "dirty-a"); got <= 0 {
		t.Fatalf("CompareVersions(dirty-b, dirty-a) = %d, want lexical ordering", got)
	}
}
