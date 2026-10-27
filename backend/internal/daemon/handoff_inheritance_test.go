package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The human-capability key arrives on the daemon's stdin and the daemon closes that pipe after one read.
// Nothing it launches (chat hosts, pty hosts, agent sessions, helpers) may be handed that descriptor, so
// no production code may give a child process os.Stdin or extra file descriptors: exec gives a nil Stdin
// /dev/null, and ExtraFiles/ForkExec are the only ways around that. This walks the backend source and
// fails on either.
func TestNoProductionCodeHandsAChildProcessTheDaemonsStdinOrExtraDescriptors(t *testing.T) {
	root := filepath.Join("..", "..") // backend/
	var offences []string
	parsed := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if entry.Name() == "node_modules" || entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed++
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				switch n.Sel.Name {
				case "ExtraFiles", "ForkExec":
					offences = append(offences, path+": uses "+n.Sel.Name)
				case "StartProcess", "ProcAttr":
					// The CLI launches the daemon; the daemon is not its child, so it is not a way into one.
					if !strings.Contains(filepath.ToSlash(path), "/internal/cli/") {
						offences = append(offences, path+": uses "+n.Sel.Name)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "ExtraFiles" {
					offences = append(offences, path+": sets ExtraFiles")
				}
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Stdin" && isOsStdin(n.Value) {
					offences = append(offences, path+": gives a child os.Stdin")
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "Stdin" && i < len(n.Rhs) && isOsStdin(n.Rhs[i]) {
						offences = append(offences, path+": assigns os.Stdin to a Stdin field")
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed < 300 {
		t.Fatalf("the walk parsed only %d source files; it is not looking at the backend", parsed)
	}
	if len(offences) > 0 {
		t.Fatalf("a child process could inherit the daemon's handoff pipe:\n%s", strings.Join(offences, "\n"))
	}
	if _, statErr := os.Stat(filepath.Join(root, "internal", "daemon")); statErr != nil {
		t.Fatalf("the walk did not start at backend/: %v", statErr)
	}
}

func isOsStdin(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Stdin" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "os"
}
