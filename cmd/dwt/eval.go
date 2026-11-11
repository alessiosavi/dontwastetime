package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/eval"
)

func (a *app) evalCmd(ctx context.Context, goldPath, model, outDir string, out io.Writer) error {
	gold, err := eval.ReadGold(goldPath)
	if err != nil {
		return err
	}
	// Copy the config and adapter so --model affects only this run.
	cfg := *a.cfg
	l := a.llm
	if model != "" {
		cfg.LLM.Model = model
		l = classify.NewLLM(cfg.LLM, a.s, a.now)
	}
	if outDir == "" {
		outDir = filepath.Join(cfg.StateDir, "eval", "runs")
	}
	now := a.now()
	report, err := eval.Run(ctx, a.s, a.c, l, a.tx, &cfg, gold)
	if err != nil {
		return err
	}
	paths, err := report.Write(outDir, cfg.LLM.Model, now)
	if err != nil {
		return err
	}
	if _, err := out.Write(report.Markdown(cfg.LLM.Model)); err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Fprintln(out, path)
	}
	return nil
}
