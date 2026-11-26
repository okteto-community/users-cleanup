// Package cleanup decides which Okteto users are inactive, which of them can be
// deleted safely, and performs the deletion.
package cleanup

import (
	"context"
	"time"
)

// User is a user as returned by the Okteto public API.
type User struct {
	ID               string     `json:"id"`
	ExternalID       string     `json:"externalId"`
	Name             string     `json:"name"`
	Email            string     `json:"email"`
	Role             string     `json:"role"`
	DefaultNamespace string     `json:"defaultNamespace"`
	CreationDate     *time.Time `json:"creationDate,omitempty"`
	LastSeen         *time.Time `json:"lastSeen,omitempty"`
}

// RoleAdmin is the role the Okteto API reports for admin users.
const RoleAdmin = "Admin"

// Resource is something a user left in a namespace.
type Resource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Namespace is a namespace owned by a user. Deleting the user deletes it.
type Namespace struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Personal bool   `json:"personal,omitempty"`
	// OtherMembers is the number of users, other than the owner, with access to the namespace.
	OtherMembers int        `json:"otherMembers,omitempty"`
	Resources    []Resource `json:"resources,omitempty"`
	// ResourceCount is the real number of resources: Resources is capped to keep reports small.
	ResourceCount int `json:"resourceCount,omitempty"`
}

// Status is the outcome of classifying a user.
type Status string

const (
	// StatusActive users were seen within the inactivity window. They are never touched.
	StatusActive Status = "active"
	// StatusDeletable users are inactive and nothing prevents their deletion.
	StatusDeletable Status = "deletable"
	// StatusBlocked users are inactive, but something prevents their deletion.
	StatusBlocked Status = "blocked"
)

// Reason explains why an inactive user is blocked.
type Reason string

const (
	// ReasonAdmin: admins are never deleted.
	ReasonAdmin Reason = "admin"
	// ReasonResources: a namespace of the user has resources.
	ReasonResources Reason = "resources"
	// ReasonShared: a namespace of the user is shared with other users, who would lose it.
	ReasonShared Reason = "shared"
	// ReasonNoActivityData: Okteto reports neither a last seen date nor a creation date.
	ReasonNoActivityData Reason = "no-activity-data"
	// ReasonCheckFailed: the namespaces of the user could not be inspected.
	ReasonCheckFailed Reason = "check-failed"
	// ReasonActiveAgain: the user came back between the scan and the deletion.
	ReasonActiveAgain Reason = "active-again"
)

// UserReport is a user plus everything the cleanup knows about them.
type UserReport struct {
	User
	// InactiveDays is the number of days since the last activity (or since the user
	// was created, when they were never seen). It is -1 when unknown.
	InactiveDays int         `json:"inactiveDays"`
	NeverSeen    bool        `json:"neverSeen,omitempty"`
	Status       Status      `json:"status"`
	Reasons      []Reason    `json:"reasons,omitempty"`
	Namespaces   []Namespace `json:"namespaces,omitempty"`
	// Error is set when the user was blocked or failed because of an error.
	Error string `json:"error,omitempty"`
}

// HasReason reports whether the user is blocked for the given reason.
func (u UserReport) HasReason(r Reason) bool {
	for _, x := range u.Reasons {
		if x == r {
			return true
		}
	}
	return false
}

func (u *UserReport) addReason(r Reason) {
	if !u.HasReason(r) {
		u.Reasons = append(u.Reasons, r)
	}
	u.Status = StatusBlocked
}

// Totals summarises a scan.
type Totals struct {
	Users     int `json:"users"`
	Active    int `json:"active"`
	Inactive  int `json:"inactive"`
	Deletable int `json:"deletable"`
	Blocked   int `json:"blocked"`
}

// Snapshot is the result of a scan: what a cleanup would do right now.
type Snapshot struct {
	GeneratedAt  time.Time    `json:"generatedAt"`
	InactiveDays int          `json:"inactiveDays"`
	Cutoff       time.Time    `json:"cutoff"`
	Users        []UserReport `json:"users"`
	Totals       Totals       `json:"totals"`
}

// Trigger says what started a run.
type Trigger string

const (
	TriggerManual    Trigger = "manual"
	TriggerScheduled Trigger = "scheduled"
)

// RunTotals summarises a run.
type RunTotals struct {
	Users    int `json:"users"`
	Active   int `json:"active"`
	Inactive int `json:"inactive"`
	Deleted  int `json:"deleted"`
	Blocked  int `json:"blocked"`
	Failed   int `json:"failed"`
}

// Run is the report of a cleanup execution.
type Run struct {
	ID           string    `json:"id"`
	Trigger      Trigger   `json:"trigger"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	InactiveDays int       `json:"inactiveDays"`
	Cutoff       time.Time `json:"cutoff"`
	// Deleted users no longer exist in Okteto.
	Deleted []UserReport `json:"deleted"`
	// Failed users should have been deleted, but the Okteto API returned an error.
	Failed []UserReport `json:"failed"`
	// Blocked users are inactive but were skipped. Reasons and Namespaces say why.
	Blocked []UserReport `json:"blocked"`
	Totals  RunTotals    `json:"totals"`
	// Error is set when the run could not complete.
	Error string `json:"error,omitempty"`
}

// UsersAPI is the part of the Okteto API the cleanup needs.
type UsersAPI interface {
	ListUsers(ctx context.Context) ([]User, error)
	DeleteUser(ctx context.Context, id string) error
}

// Inventory answers what a user owns in the cluster.
type Inventory interface {
	// OwnedNamespaces returns every Okteto namespace grouped by the ID of its owner.
	OwnedNamespaces(ctx context.Context) (map[string][]Namespace, error)
	// NamespacesOf returns the namespaces owned by one user.
	NamespacesOf(ctx context.Context, userID string) ([]Namespace, error)
	// Resources returns the resources in a namespace.
	Resources(ctx context.Context, namespace string) ([]Resource, error)
}

// Progress describes how far a scan or run is.
type Progress struct {
	Phase string `json:"phase"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}
