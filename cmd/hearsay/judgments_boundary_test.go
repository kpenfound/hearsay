package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/llm"
)

func TestJevIsRequiredOnceAConfigurationIsLoaded(t *testing.T) {
	provider := func(llm.JudgmentConfig, string) llm.JudgmentProvider { return llm.NewFakeJudgmentProvider() }
	for _, tt := range []struct {
		name, path, model, key, want string
	}{
		{name: "no configuration has nothing to judge", key: ""},
		{name: "a configuration with no model", path: "/etc/hearsay", key: "secret", want: "llm.judgments.model"},
		{name: "a configuration with no key", path: "/etc/hearsay", model: "recorded-v1", want: llm.DefaultJudgmentAPIKeyEnv},
		{name: "a configuration with both", path: "/etc/hearsay", model: "recorded-v1", key: "secret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(llm.DefaultJudgmentAPIKeyEnv, tt.key)
			cfg := config.Default()
			cfg.Repo.Path = tt.path
			cfg.Repo.Judgments = llm.JudgmentConfig{Model: tt.model}
			j, err := judgmentCapability(&cfg, provider)
			switch {
			case tt.want == "" && (err != nil || j == nil):
				t.Fatalf("judgmentCapability() = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("judgmentCapability() = %v, want an error naming %s", err, tt.want)
			}
		})
	}
}

// Package dependencies include l1's passive journal types. Guard the active
// capability at the symbol boundary: these paths may not construct, receive or
// consult judgments, nor branch on Jev configuration. Search retains embed.
func TestReadAndHumanPolicyPathsHaveNoJudgmentDependency(t *testing.T) {
	for _, dir := range []string{"bundle", "l2", "l3", "principal", "deletion", "service/api", "service/connectors"} {
		t.Run(dir, func(t *testing.T) {
			root := filepath.Join("../../internal", dir)
			err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
				if err != nil {
					return err
				}
				for _, imp := range f.Imports {
					if strings.Contains(imp.Path.Value, "/llm/jev") {
						t.Errorf("judgment transport dependency: %s", path)
					}
				}
				ast.Inspect(f, func(n ast.Node) bool {
					if s, ok := n.(*ast.SelectorExpr); ok {
						switch s.Sel.Name {
						case "Judgments", "NewJudgments", "Judge", "Unconfigured":
							t.Errorf("judgment dependency in %s: %s", path, s.Sel.Name)
						}
					}
					return true
				})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
