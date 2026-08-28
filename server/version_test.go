package server

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The linker stamps these four. Assigning to them here is half the test: it only
// compiles while a package-level var of that exact name exists, which is what
// -X needs and what was missing.
func TestBuildVersionUsesLinkerVariables(t *testing.T) {
	oldV, oldS, oldB, oldD := Version, GitSha, GitBranch, BuildDate
	defer func() { Version, GitSha, GitBranch, BuildDate = oldV, oldS, oldB, oldD }()

	Version, GitSha, GitBranch, BuildDate = "1.2.3", "deadbee", "main", "1999-01-01 00:00:00 UTC"
	got := buildVersion()
	want := VersionInfo{Version: "1.2.3", GitSha: "deadbee", GitBranch: "main", BuildDate: "1999-01-01 00:00:00 UTC"}
	if got != want {
		t.Errorf("buildVersion() = %+v, want %+v", got, want)
	}

	// A plain `go build` stamps nothing, and the build date is the one value
	// worth inventing at that point.
	BuildDate = ""
	if d := buildVersion().BuildDate; d == "" {
		t.Error("BuildDate is empty with no stamp; expected a fallback")
	}
}

// build_all.sh passes -X for four symbols. The Go linker does not fail on a
// symbol it cannot find, so a typo or a rename here goes unnoticed until someone
// asks a release binary for its version and it answers "dev" — which is exactly
// what every release did until this was fixed.
func TestBuildScriptStampsSymbolsThatExist(t *testing.T) {
	b, err := os.ReadFile("../build_all.sh")
	if err != nil {
		t.Skipf("build_all.sh not readable: %v", err)
	}

	// Names proven to exist by the assignment in the test above.
	declared := map[string]bool{"Version": true, "GitSha": true, "GitBranch": true, "BuildDate": true}

	stamped := map[string]bool{}
	for _, m := range regexp.MustCompile(`-X '([^'=]+)=`).FindAllStringSubmatch(string(b), -1) {
		sym := m[1]
		pkg, name, ok := strings.Cut(sym, ".")
		if !ok {
			t.Errorf("-X target %q is not package.Name", sym)
			continue
		}
		if pkg != "jacred/server" {
			t.Errorf("-X targets %q; this test only knows jacred/server", pkg)
			continue
		}
		if !declared[name] {
			t.Errorf("build_all.sh stamps jacred/server.%s, which is not a package variable — "+
				"the linker will ignore it silently", name)
		}
		stamped[name] = true
	}

	if len(stamped) == 0 {
		t.Fatal("build_all.sh stamps nothing; releases would report the defaults")
	}
	var missing []string
	for name := range declared {
		if !stamped[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("build_all.sh does not stamp %v", missing)
	}
}
