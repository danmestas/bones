package main

// toExitCode maps handler errors to process exit codes. Fleshed out in Task 2.
func toExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}
