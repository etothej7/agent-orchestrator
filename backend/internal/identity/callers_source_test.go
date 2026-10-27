package identity

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WithCaller and WithClaim write the verified identity (and the unauthenticated claim) onto a
// request context. Only the daemon's own identity middleware may call them: any other production
// code that did could hand a request an identity nothing verified.
func TestOnlyTheIdentityMiddlewareWritesTheCallerAndTheClaimToAContext(t *testing.T) {
	root := filepath.Join("..", "..") // backend/
	allowed := map[string]bool{
		filepath.Join("internal", "httpd", "caller_identity.go"): true,
		filepath.Join("internal", "identity", "identity.go"):     true, // the definitions
	}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[rel] {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // reading this repository's own sources
		if err != nil {
			return err
		}
		text := string(src)
		if strings.Contains(text, "identity.WithCaller(") || strings.Contains(text, "identity.WithClaim(") {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("production code outside the identity middleware writes a caller or claim to a context: %v", offenders)
	}
}
