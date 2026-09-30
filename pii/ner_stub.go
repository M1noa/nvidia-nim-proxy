//go:build !ner

package pii

// default build: no model, no native lib. NerDetector is always nil.
func loadNer(modelPath string, timeoutMs int) Detector {
	return nil
}

func NerDetector(modelPath, ortLib string, timeoutMs int, minScore float64) Detector {
	return nil
}

func NerAvailable() bool { return false }
