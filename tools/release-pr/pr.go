package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	noCommits = "(差分コミットなし — tree 差分のみ)"
	noBase    = "(前回昇格点を main 上で特定できなかったため一覧は省略。diff を確認してください)"
)

// GitHub の merge commit の件名を PR 番号だけにする。direct push の件名はそのまま残す
var mergeSubject = regexp.MustCompile(`- Merge pull request (#[0-9]+) from .*`)

var jst = time.FixedZone("JST", 9*60*60)

func runPR(ctx context.Context, cfg config, d deps) error {
	ahead, err := d.git.line(ctx, "rev-list", "--count", "origin/"+productionBranch+".."+releaseBranch)
	if err != nil {
		return err
	}
	if ahead == "0" {
		fmt.Fprintln(d.stdout, "::notice::production is already up to date with main; skipping production release PR creation.")
		return writeOutput(cfg, "has_changes=false\n")
	}
	if err := writeOutput(cfg, "has_changes=true\n"); err != nil {
		return err
	}
	fmt.Fprintf(d.stdout, "release/production is %s commit(s) ahead of production.\n", ahead)

	base, err := previousPromotion(ctx, d.git)
	if err != nil {
		return err
	}
	changes, err := changeList(ctx, d.git, base)
	if err != nil {
		return err
	}
	inserted := ""
	if cfg.bodyCommand != "" {
		if inserted, err = runBodyCommand(ctx, cfg); err != nil {
			return err
		}
	}
	body := renderBody(cfg, inserted, changes)

	existing, err := d.gh.openReleasePR(ctx)
	if err != nil {
		return err
	}
	// title は初回作成時の日時のまま維持する（いつから昇格待ちかを表す）。body と diff が常に main 先端を示す
	if existing != nil {
		if err := d.gh.editBody(ctx, existing.Number, body); err != nil {
			return err
		}
		fmt.Fprintf(d.stdout, "Updated PR #%d\n", existing.Number)
		return nil
	}
	pr, err := d.gh.createReleasePR(ctx, "🚀リリース "+d.now().In(jst).Format("2006-01-02 15:04:05"), body)
	if err != nil {
		return err
	}
	fmt.Fprintln(d.stdout, pr.HTMLURL)
	return nil
}

func writeOutput(cfg config, s string) error {
	if cfg.outputPath == "" {
		return nil
	}
	return appendFile(cfg.outputPath, s)
}

// 前回昇格した main の位置。production の各コミットは main のどこかの tree の snapshot なので、
// main を first-parent で遡り、production と tree が一致した最初のコミットがそれ。見つからなければ空
func previousPromotion(ctx context.Context, g git) (string, error) {
	productionTree, err := g.line(ctx, "rev-parse", "origin/"+productionBranch+"^{tree}")
	if err != nil {
		return "", err
	}
	commits, err := g.lines(ctx, "log", "--first-parent", "--format=%H %T", "origin/main")
	if err != nil {
		return "", err
	}
	for _, c := range commits {
		sha, tree, _ := strings.Cut(c, " ")
		if tree == productionTree {
			return sha, nil
		}
	}
	return "", nil
}

// main は merge commit だけなので、first-parent の件名から PR 番号を並べられる
func changeList(ctx context.Context, g git, base string) (string, error) {
	if base == "" {
		return noBase, nil
	}
	subjects, err := g.lines(ctx, "log", "--first-parent", "--format=- %s", base+"..origin/main")
	if err != nil {
		return "", err
	}
	if len(subjects) == 0 {
		return noCommits, nil
	}
	for i, s := range subjects {
		subjects[i] = mergeSubject.ReplaceAllString(s, "- $1")
	}
	return strings.Join(subjects, "\n"), nil
}

// body-command は呼び出し側の checkout で bash -c に渡す。標準出力をそのまま本文に入れる
func runBodyCommand(ctx context.Context, cfg config) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", cfg.bodyCommand)
	cmd.Dir = cfg.repoDir
	cmd.Env = bodyCommandEnv(os.Environ())
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("body-command: %w", err)
	}
	return out.String(), nil
}

// setup-go が job に GOTOOLCHAIN=local を足す。body-command の中の go が呼び出し側の go.mod の版を取りに行けるように、
// ツールを動かす前の環境（GOTOOLCHAIN なし）に戻して渡す
func bodyCommandEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GOTOOLCHAIN=") {
			out = append(out, kv)
		}
	}
	return out
}

func renderBody(cfg config, inserted, changes string) string {
	var b strings.Builder
	b.WriteString("## 概要\n\n")
	fmt.Fprintf(&b, "`main` (`%s`) の内容を `production` に昇格するリリースPRです。\n\n", cfg.mainSHA)
	b.WriteString("`main` に新しい PR が merge されるたびに、このPRは main 先端の snapshot に自動更新されます。merge 時点の diff がそのまま本番に deploy される内容です。\n\n")
	b.WriteString("このPRは本番昇格の承認ゲートであり、コードの再レビュー用ではありません。通常のコードレビューとCIは `main` に入る通常PRで完了済みです。\n\n")
	if cfg.bodyCommand != "" {
		b.WriteString(inserted)
		b.WriteString("\n")
	}
	b.WriteString("## 含まれる変更\n\n")
	b.WriteString(changes + "\n\n")
	// with の | ブロックは末尾に改行が付くので、1 つ落としてから出す
	if cfg.checklist != "" {
		b.WriteString("## Merge前チェック\n\n")
		b.WriteString(strings.TrimSuffix(cfg.checklist, "\n") + "\n\n")
	}
	b.WriteString("## Deploy\n\n")
	b.WriteString(strings.TrimSuffix(cfg.deployNote, "\n") + "\n")
	return b.String()
}
