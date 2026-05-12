//go:build !race

package server

// raceEnabled reports whether the test binary was built with `-race`.
// See `race_on_test.go` for the rationale behind the split budget.
const raceEnabled = false
