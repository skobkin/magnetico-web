package advisory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"strings"
	"testing"
)

// TestEveryKeyIsPositiveAndUnique reads the declarations of this package
// itself, so a key added later is covered without editing the test.
func TestEveryKeyIsPositiveAndUnique(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "advisory.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	maxKey := new(big.Int).SetInt64(1<<63 - 1)
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		vs, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				t.Errorf("%s is not an integer literal", vs.Names[i].Name)
				continue
			}
			val, _ := new(big.Int).SetString(strings.ReplaceAll(lit.Value, "_", ""), 0)
			if val.Sign() <= 0 || val.Cmp(maxKey) > 0 {
				t.Errorf("%s = %s is not a positive int64 (ownsLockSQL relies on it)", vs.Names[i].Name, val)
			}
			if other, dup := seen[val.String()]; dup {
				t.Errorf("%s and %s share the key %s", other, vs.Names[i].Name, val)
			}
			seen[val.String()] = vs.Names[i].Name
			n++
		}
		return true
	})
	if n != 9 {
		t.Errorf("found %d keys, expected 9: update this count when adding or removing one", n)
	}
}
