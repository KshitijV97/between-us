//go:build race

package telegram

// raceEnabled skips the large-stream memory test: race instrumentation
// multiplies heap use and runtime, so its numbers are meaningless there.
const raceEnabled = true
