package cli

// PlanCmd is the umbrella command for plan-workflow operations
// beyond validation. Today's surface: `bones plan finalize`. Validate
// stays at the top level (`bones validate-plan`) for now — it predates
// this group and changing the verb shape is a separate concern.
type PlanCmd struct {
	Finalize PlanFinalizeCmd `cmd:"" help:"Materialize hub artifacts to the host tree (ADR 0044)"`
}
