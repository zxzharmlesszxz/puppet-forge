package domain

import "testing"

func TestModuleIdentityValidation(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"teamname", "TeamName", "TEAM123"} {
		if !ValidModuleOwner(value) {
			t.Errorf("ValidModuleOwner(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "team_name", "team-name", "team.name"} {
		if ValidModuleOwner(value) {
			t.Errorf("ValidModuleOwner(%q) = true, want false", value)
		}
	}
	for _, value := range []string{"module", "module_name", "module123"} {
		if !ValidModuleName(value) {
			t.Errorf("ValidModuleName(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "Module", "module-name", "module.name"} {
		if ValidModuleName(value) {
			t.Errorf("ValidModuleName(%q) = true, want false", value)
		}
	}
}
