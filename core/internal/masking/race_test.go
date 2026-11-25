//go:build race

package masking

func init() { raceDetector = true }
