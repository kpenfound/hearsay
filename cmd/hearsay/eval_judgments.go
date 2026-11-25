package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/kpenfound/hearsay/internal/eval"
)

func runJudgmentEval(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs, _, _ := newFlagSet("eval judgments", stderr)
	corpusPath := fs.String("corpus", "", "source-backed, human-adjudicated corpus JSON")
	predictionsPath := fs.String("predictions", "", "recorded Jev and baseline predictions JSON")
	split := fs.String("split", "held_out", "comparison split: tuning, held_out, or all")
	fs.Bool("json", false, "JSON output (always used for judgment evaluation)")
	words, err := parseWords(fs, args)
	if err != nil {
		return err
	}
	if len(words) > 0 {
		return fmt.Errorf("unexpected argument %q", words[0])
	}
	if *corpusPath == "" || *predictionsPath == "" {
		return fmt.Errorf("eval judgments requires --corpus and --predictions")
	}
	if *split != "held_out" && *split != "tuning" && *split != "all" {
		return fmt.Errorf("--split must be tuning, held_out or all")
	}
	var c eval.Corpus
	var ps eval.Predictions
	if err := readEvaluationFile(*corpusPath, &c); err != nil {
		return err
	}
	if err := readEvaluationFile(*predictionsPath, &ps); err != nil {
		return err
	}
	selection := *split
	if selection == "all" {
		selection = ""
	}
	comparisons, err := eval.CompareJudgments(c, ps, selection)
	if err != nil {
		return err
	}
	report := eval.Report{Judgments: comparisons, Pipelines: eval.SummarizePipelines(nil)}
	return report.WriteJSON(stdout)
}

func readEvaluationFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open evaluation input: %w", err)
	}
	defer func() { _ = f.Close() }()
	return eval.DecodeEvaluation(f, v)
}
