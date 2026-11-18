//go:build race

package cli

// raceEnabled skips timing checks under the race detector (CI runs them in
// a step without it).
const raceEnabled = true
