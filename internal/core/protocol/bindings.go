package protocol

// Process-wide capabilities are bound before execution for the single active
// workspace. Join all consumers before replacing bindings or closing resources.
var (
	// Both tiers may share an instance. Contexts retain only their tier selection.
	ModelPro   Model
	ModelFlash Model

	// Storage stays bound while the active workspace's process lock is held.
	Storage StorageBackend
	Tools   ToolExecutor

	// UserMessages carries complete text. CLI owns the writable end and closes
	// it on terminal EOF or read failure; redirected startup input is held for
	// interaction without treating its EOF as an interruption of the workflow.
	UserMessages <-chan string
	// ShowUser writes immediately, without waiting for input.
	ShowUser func(string) error

	// A nil observer disables all diagnostics. The implementation owns capture policy.
	Observer Observation
)
