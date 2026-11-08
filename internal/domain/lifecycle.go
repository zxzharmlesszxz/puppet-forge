package domain

import "time"

type ResourceState string

const (
	StateAvailable  ResourceState = "available"
	StatePending    ResourceState = "pending"
	StateDeprecated ResourceState = "deprecated"
	StateBlocked    ResourceState = "blocked"
	StateDeleted    ResourceState = "deleted"
)

type ResourceStatus struct {
	State       ResourceState `json:"state"`
	Previous    ResourceState `json:"previous_state,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Replacement string        `json:"replacement,omitempty"`
	SHA256      string        `json:"sha256,omitempty"`
	ChangedAt   time.Time     `json:"changed_at"`
}

func (s ResourceState) Readable() bool {
	return s == "" || s == StateAvailable || s == StateDeprecated
}
