package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// git は dir で git を実行し、標準出力を返す
type git func(ctx context.Context, args ...string) (string, error)

func newGit(dir string) git {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
}

// 出力の末尾の改行を除いたもの（シェルの $(...) と同じ）
func (g git) line(ctx context.Context, args ...string) (string, error) {
	out, err := g(ctx, args...)
	return strings.TrimRight(out, "\n"), err
}

func (g git) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := g.line(ctx, args...)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}
