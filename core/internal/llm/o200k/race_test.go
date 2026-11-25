//go:build race

package o200k

func init() { raceDetector = true }
