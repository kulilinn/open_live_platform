package livebot

import "fmt"

type LegacyPoolEntry struct {
	UserID      int64  `json:"user_id,string"`
	State       string `json:"state"`
	AllocatedAt int64  `json:"allocated_at,omitempty"`
}

func (entry LegacyPoolEntry) Validate() error {
	if entry.UserID <= 0 || entry.State == "idle" && entry.AllocatedAt != 0 ||
		entry.State == "inuse" && entry.AllocatedAt <= 0 || entry.State != "idle" && entry.State != "inuse" {
		return fmt.Errorf("invalid legacy room bot pool member")
	}
	return nil
}
