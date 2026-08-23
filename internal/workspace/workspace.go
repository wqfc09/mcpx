package workspace

const (
	StatusOK      = "ok"
	StatusMissing = "missing"
	StatusInvalid = "invalid"
)

// Workspace is one durable Workspace registration resolved from global config.
type Workspace struct {
	ID          string
	Name        string
	Path        string
	Description string
	Status      string
}
