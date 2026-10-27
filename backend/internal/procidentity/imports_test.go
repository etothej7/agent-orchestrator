package procidentity

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The process lookup (the socket owner and the process table) is a source of facts for exactly two
// consumers: the identity bridge (which derives a session from the daemon's own records and the
// kernel) and the anonymous-call observations (a log and metric sink only). Nothing else in the
// daemon, in particular no route handler, predicate or authority decision, may import it, so a
// pid or a process name can never become an input to who may do what.
func TestOnlyTheBridgeAndTheObservationsImportTheProcessLookup(t *testing.T) {
	allowed := map[string]bool{"identity": true, "identityobs": true, "daemon": true, "procidentity": true}
	root := ".."
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range file.Imports {
			if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/internal/procidentity") {
				dir := filepath.Base(filepath.Dir(path))
				if !allowed[dir] {
					offenders = append(offenders, path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("these packages import the process lookup but are not allowed to: %v", offenders)
	}
}
