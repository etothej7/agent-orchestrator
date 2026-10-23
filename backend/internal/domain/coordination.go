package domain

import "slices"

// CoordinationConfig links one human-facing coordinator to project managers.
// These are workflow assignments, not filesystem or credential permissions.
type CoordinationConfig struct {
	CoordinatorProjectID ProjectID   `json:"coordinatorProjectId"`
	ManagedProjectIDs    []ProjectID `json:"managedProjectIds"`
}

// Manages reports whether the project has an assigned repo manager role.
func (c CoordinationConfig) Manages(id ProjectID) bool {
	return c.CoordinatorProjectID != "" && slices.Contains(c.ManagedProjectIDs, id)
}
