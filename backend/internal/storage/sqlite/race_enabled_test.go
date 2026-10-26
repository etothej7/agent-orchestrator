//go:build race

package sqlite

// raceDetector reports that the race detector slows every operation several-fold.
const raceDetector = true
