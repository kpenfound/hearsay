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

func TestJudgmentConstructionIsInertWhenDisabled(t *testing.T) {
	cfg := config.Default()
	built := 0
	j, err := judgmentCapability(&cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { built++; return llm.NewFakeJudgmentProvider() })
	if err != nil || built != 0 || j.Status().Enabled {
		t.Fatalf("disabled construction: built=%d err=%v", built, err)
	}
	t.Setenv(llm.DefaultJudgmentAPIKeyEnv, "")
	cfg.Repo.Boost = llm.JudgmentConfig{Enabled: true, Model: "recorded-v1"}
	j, err = judgmentCapability(&cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { built++; return llm.NewFakeJudgmentProvider() })
	if err != nil || built != 0 || j.Status().Healthy || j.Status().LastFallback != llm.JudgmentMissingCredential {
		t.Fatalf("missing credential construction: built=%d err=%v status=%+v", built, err, j.Status())
	}
}

// Package dependencies include l1's passive journal types. Guard the active
// capability at the symbol boundary: these paths may not construct, receive or
// consult judgments, nor branch on boost configuration. Search retains embed.
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
						case "Judgments", "NewJudgments", "Judge", "Boost":
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
