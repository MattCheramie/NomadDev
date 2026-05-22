package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var envVarLiteral = regexp.MustCompile(`^NOMADDEV_[A-Z0-9_]+$`)

// scanEnvLiterals returns every exact NOMADDEV_* string literal that appears
// in the non-test source of package config, excluding registry.go itself
// (which lists all of them by construction). This is the set of env vars the
// package actually reads.
func scanEnvLiterals(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "registry.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && envVarLiteral.MatchString(s) {
				found[s] = true
			}
			return true
		})
	}
	return found
}

// TestRegistryParity fails if config.go (and its companion source) reads a
// NOMADDEV_* var with no Registry entry, or if Registry lists a var no source
// file reads. This is the load-bearing guard against the registry drifting
// out of sync with Load() as new knobs are added.
func TestRegistryParity(t *testing.T) {
	referenced := scanEnvLiterals(t)
	if len(referenced) == 0 {
		t.Fatal("scanned zero NOMADDEV_* literals — the AST scan is broken")
	}

	inRegistry := map[string]bool{}
	for _, s := range Registry {
		if inRegistry[s.EnvVar] {
			t.Errorf("duplicate Registry entry: %s", s.EnvVar)
		}
		inRegistry[s.EnvVar] = true
	}

	for envVar := range referenced {
		if !inRegistry[envVar] {
			t.Errorf("%s is read by package config but missing from Registry", envVar)
		}
	}
	for envVar := range inRegistry {
		if !referenced[envVar] {
			t.Errorf("%s is in Registry but read by no source file", envVar)
		}
	}
}

// TestRegistryDefaultsValid asserts every default round-trips through the
// setting's own validator — including enum-default membership.
func TestRegistryDefaultsValid(t *testing.T) {
	for _, s := range Registry {
		if err := s.Validate(s.Default); err != nil {
			t.Errorf("%s: default %q fails validation: %v", s.EnvVar, s.Default, err)
		}
	}
}

// TestRegistryShape catches structural mistakes in hand-authored entries.
func TestRegistryShape(t *testing.T) {
	for _, s := range Registry {
		if s.Category == "" {
			t.Errorf("%s: empty Category", s.EnvVar)
		}
		if s.Description == "" {
			t.Errorf("%s: empty Description", s.EnvVar)
		}
		if (s.Type == TypeEnum) != (len(s.Enum) > 0) {
			t.Errorf("%s: enum type and Enum list must agree", s.EnvVar)
		}
		got, ok := Lookup(s.EnvVar)
		if !ok || got.EnvVar != s.EnvVar {
			t.Errorf("%s: Lookup failed", s.EnvVar)
		}
	}
	if len(Categories()) == 0 {
		t.Error("Categories returned nothing")
	}
}
