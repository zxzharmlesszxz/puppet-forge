package store

import "testing"

func FuzzValidateModulePagination(f *testing.F) {
	f.Add(1, 0)
	f.Add(MaxModulePageSize, MaxModulePageOffset)
	f.Add(0, -1)
	f.Add(MaxModulePageSize+1, MaxModulePageOffset+1)
	f.Fuzz(func(t *testing.T, limit, offset int) {
		err := validateModulePagination(limit, offset)
		valid := limit >= 1 && limit <= MaxModulePageSize && offset >= 0 && offset <= MaxModulePageOffset
		if valid && err != nil {
			t.Fatalf("valid pagination (%d, %d) rejected: %v", limit, offset, err)
		}
		if !valid && err == nil {
			t.Fatalf("invalid pagination (%d, %d) accepted", limit, offset)
		}
	})
}
