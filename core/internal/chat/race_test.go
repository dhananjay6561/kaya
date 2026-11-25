//go:build race

package chat

func init() { raceDetector = true }
