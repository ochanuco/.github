package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// 前回との関係
type relation string

const (
	relationNone     relation = "none" // 前回の記録がない
	relationSame     relation = "same"
	relationRollback relation = "rollback"
	relationForward  relation = "forward"
	relationDiverged relation = "diverged" // 前回が今回の祖先ではない
)

type plan struct {
	sha      string
	prev     previous
	relation relation
	// forward と diverged で、変更の一覧を出す起点（前回と今回の merge-base）
	base           string
	notices        []string
	migrations     []string
	migrationsGate bool
	reasons        []string
	// 承認が要るときの Environment の名前。要らないか、approval-environment が空なら空
	approval string
}

func makePlan(ctx context.Context, cfg config, g git, prev previous) (plan, error) {
	p := plan{sha: cfg.sha, prev: prev}
	var err error
	if p.relation, p.base, err = relate(ctx, g, prev.sha, cfg.sha); err != nil {
		return plan{}, err
	}
	if p.hasDiff() {
		if p.notices, err = matchNotices(ctx, g, cfg.notices, prev.sha, cfg.sha); err != nil {
			return plan{}, err
		}
		if p.migrations, p.migrationsGate, err = classifyMigrations(ctx, g, cfg.migrationDirs, prev.sha, cfg.sha); err != nil {
			return plan{}, err
		}
	}
	p.reasons = approvalReasons(p.relation, len(p.notices) > 0, p.migrationsGate)
	if cfg.approvalEnvironment != "" && len(p.reasons) > 0 {
		p.approval = cfg.approvalEnvironment
	}
	return p, nil
}

func (p plan) hasDiff() bool {
	return p.relation != relationNone && p.relation != relationSame
}

func relate(ctx context.Context, g git, prev, sha string) (relation, string, error) {
	switch {
	case prev == "":
		return relationNone, "", nil
	case prev == sha:
		return relationSame, "", nil
	case g.ok(ctx, "merge-base", "--is-ancestor", sha, prev):
		return relationRollback, "", nil
	}
	base, err := g.line(ctx, "merge-base", prev, sha)
	if err != nil {
		return "", "", err
	}
	if base == prev {
		return relationForward, base, nil
	}
	return relationDiverged, base, nil
}

func matchNotices(ctx context.Context, g git, notices []notice, prev, sha string) ([]string, error) {
	var hits []string
	for _, n := range notices {
		files, err := g.lines(ctx, "diff", "--name-only", prev, sha, "--", n.prefix)
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			hits = append(hits, fmt.Sprintf("- %s（`%s` の %d ファイル）", n.message, n.prefix, len(files)))
		}
	}
	return hits, nil
}

var safeStatement = regexp.MustCompile(`(?i)^(create\s+table|create\s+(unique\s+)?index|alter\s+table\s+\S+\s+add\s)`)

// migration を文ごとに分類する。追加された .sql が追加系の文だけなら通し、
// 追加系でない文があるか、既存の .sql をコメント以外で書き換えた・消したときは gate を立てる
func classifyMigrations(ctx context.Context, g git, dirs []string, prev, sha string) ([]string, bool, error) {
	var out []string
	blocked := false
	for _, dir := range dirs {
		entries, err := g.lines(ctx, "diff", "--name-status", "--no-renames", prev, sha, "--", dir)
		if err != nil {
			return nil, false, err
		}
		for _, entry := range entries {
			status, file, ok := strings.Cut(entry, "\t")
			if !ok {
				return nil, false, fmt.Errorf("git diff --name-status の行が読めない: %q", entry)
			}
			if !strings.HasSuffix(file, ".sql") {
				continue
			}
			if status == "M" {
				before, err := statementsAt(ctx, g, prev, file)
				if err != nil {
					return nil, false, err
				}
				after, err := statementsAt(ctx, g, sha, file)
				if err != nil {
					return nil, false, err
				}
				if slices.Equal(before, after) {
					out = append(out, fmt.Sprintf("- `%s`: コメントだけの変更", file))
					continue
				}
			}
			if status != "A" {
				blocked = true
				out = append(out, fmt.Sprintf("- ⚠️ `%s`: 既存の migration の変更（%s）。適用済みの migration は書き換えない", file, status))
				continue
			}
			statements, err := statementsAt(ctx, g, sha, file)
			if err != nil {
				return nil, false, err
			}
			var unsafe []string
			for _, s := range statements {
				if !safeStatement.MatchString(s) {
					unsafe = append(unsafe, s)
				}
			}
			if len(unsafe) == 0 {
				out = append(out, fmt.Sprintf("- `%s`: 追加系だけ（%d 文）", file, len(statements)))
				continue
			}
			blocked = true
			out = append(out, fmt.Sprintf("- ⚠️ `%s`: 追加系でない文が %d 個", file, len(unsafe)))
			for _, s := range unsafe[:min(len(unsafe), 5)] {
				out = append(out, fmt.Sprintf("  - `%s`", truncate(s, 120)))
			}
		}
	}
	return out, blocked, nil
}

func statementsAt(ctx context.Context, g git, commit, file string) ([]string, error) {
	text, err := g(ctx, "show", commit+":"+file)
	if err != nil {
		return nil, err
	}
	return splitStatements(text), nil
}

var lineComment = regexp.MustCompile(`--[^\n]*`)

// `--` から行末までを除き、`;` で分けて、空白を 1 つの空白に詰める。文字列リテラルの中も区別しない
func splitStatements(text string) []string {
	var out []string
	for _, s := range strings.Split(lineComment.ReplaceAllString(text, ""), ";") {
		if fields := strings.FieldsFunc(s, isSpace); len(fields) > 0 {
			out = append(out, strings.Join(fields, " "))
		}
	}
	return out
}

// Python の str.isspace と同じ範囲（unicode.IsSpace に加えて U+001C〜U+001F）
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func approvalReasons(rel relation, notices, migrationsGate bool) []string {
	var reasons []string
	switch rel {
	case relationNone:
		reasons = append(reasons, "前回のデプロイの記録がない")
	case relationRollback:
		reasons = append(reasons, "ロールバック")
	case relationDiverged:
		reasons = append(reasons, "前回のコミットが今回の祖先ではない")
	}
	if notices {
		reasons = append(reasons, "注意に当たる変更がある")
	}
	if migrationsGate {
		reasons = append(reasons, "追加系でない migration がある")
	}
	return reasons
}
