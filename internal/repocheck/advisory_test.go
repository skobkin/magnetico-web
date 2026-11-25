package repocheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// advisoryPackage is the one place where PostgreSQL advisory-lock keys are
// declared (Task 09A: web-schema-init and the ingest consumer once shared a key).
const advisoryPackage = "internal/advisory"

// keyFloor separates lock keys from every other integer literal in the code
// base; the project's keys are all 4.0e18 or 7.8e18.
var keyFloor = new(big.Int).SetInt64(1_000_000_000_000_000)

type sourceFile struct {
	rel  string
	file *ast.File
}

// productionGoFiles parses every non-test Go file of the module.
func productionGoFiles(t *testing.T) []sourceFile {
	t.Helper()
	root := testkit.ModuleRoot(t)
	var out []sourceFile
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".superpowers", ".claude", "third_party", "dist", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, sourceFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func inAdvisoryPackage(rel string) bool {
	return strings.HasPrefix(rel, advisoryPackage+"/")
}

// TestAdvisoryLockKeysAreDeclaredOnceAndDistinct inventories every lock-key
// literal in production code. Each key must be declared exactly once, in
// internal/advisory, and no two names may share a value. A key shared on
// purpose (the trackers write lock, taken by web-schema-init, the operator
// CLI and the seed) is one declaration used from several places.
func TestAdvisoryLockKeysAreDeclaredOnceAndDistinct(t *testing.T) {
	byValue := map[string][]string{}
	var outside []string
	for _, sf := range productionGoFiles(t) {
		ast.Inspect(sf.file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				val, ok := new(big.Int).SetString(strings.ReplaceAll(lit.Value, "_", ""), 0)
				if !ok || val.Cmp(keyFloor) < 0 {
					continue
				}
				name := sf.rel + ":" + vs.Names[i].Name
				byValue[val.String()] = append(byValue[val.String()], name)
				if !inAdvisoryPackage(sf.rel) {
					outside = append(outside, name)
				}
			}
			return true
		})
	}
	for val, names := range byValue {
		if len(names) > 1 {
			sort.Strings(names)
			t.Errorf("advisory-lock key %s is declared more than once: %s", val, strings.Join(names, ", "))
		}
	}
	for _, name := range outside {
		t.Errorf("%s declares a lock-key sized constant outside %s; declare keys there only", name, advisoryPackage)
	}
	if len(byValue) == 0 {
		t.Error("no advisory-lock key found: the inventory is broken")
	}
}

// TestAdvisoryLocksUseOnlyTheRegistry fails when a package takes an advisory
// lock without importing the registry in any of its files. Together with the
// ban on key-sized literals outside internal/advisory (above) an unregistered
// key cannot enter: a package may alias a registry constant in one file and use
// it in another, but cannot invent a number.
func TestAdvisoryLocksUseOnlyTheRegistry(t *testing.T) {
	importing := map[string]bool{}
	taking := map[string][]string{}
	for _, sf := range productionGoFiles(t) {
		if inAdvisoryPackage(sf.rel) {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(sf.rel))
		for _, imp := range sf.file.Imports {
			if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/"+advisoryPackage) {
				importing[dir] = true
			}
		}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && (strings.Contains(lit.Value, "pg_advisory") || strings.Contains(lit.Value, "pg_try_advisory")) {
				taking[dir] = append(taking[dir], sf.rel)
			}
			return true
		})
	}
	for dir, files := range taking {
		if !importing[dir] {
			t.Errorf("package %s takes an advisory lock (%s) without importing %s", dir, files[0], advisoryPackage)
		}
	}
	if len(taking) == 0 {
		t.Error("no advisory lock use found: the scan is broken")
	}
}
