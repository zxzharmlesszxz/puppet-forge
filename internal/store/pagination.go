package store

import "fmt"

const (
	MaxModulePageSize   = 100
	MaxModulePageOffset = 100_000
)

func validateModulePagination(limit, offset int) error {
	if limit < 1 || limit > MaxModulePageSize {
		return fmt.Errorf("module page limit must be between 1 and %d", MaxModulePageSize)
	}
	if offset < 0 || offset > MaxModulePageOffset {
		return fmt.Errorf("module page offset must be between 0 and %d", MaxModulePageOffset)
	}
	return nil
}
