package app

import "testing"

func TestBrowserSolveLimitRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FlareSolverrGo.MaxConcurrentSolves = 1
	var parsed Config
	parseYAMLIntoConfig(MarshalYAML(cfg), &parsed)
	if parsed.FlareSolverrGo.MaxConcurrentSolves != 1 {
		t.Fatal("browser solve limit was lost while saving settings")
	}
}
