package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

func render(ctx context.Context, cfg config, g git, p plan) (string, error) {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	link := func(sha string) (string, error) {
		short, err := g.line(ctx, "rev-parse", "--short", sha)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[`%s`](%s/%s/commit/%s)", short, cfg.serverURL, cfg.repo, sha), nil
	}

	current, err := link(p.sha)
	if err != nil {
		return "", err
	}
	line("## %s にデプロイする内容", cfg.environment)
	line("")
	line("- 今回: %s", current)
	if p.prev.sha != "" {
		prev, err := link(p.prev.sha)
		if err != nil {
			return "", err
		}
		line("- 前回: %s（[run](%s)）", prev, p.prev.runURL)
	} else {
		line("- 前回: 記録なし")
	}
	if cfg.approvalEnvironment != "" {
		if p.approval != "" {
			line("- 承認: 要る（%s）。Environment `%s` で止まる", strings.Join(p.reasons, "、"), p.approval)
		} else {
			line("- 承認: 要らない。このまま出る")
		}
	}
	line("")

	switch p.relation {
	case relationNone:
		line("前回のデプロイが見つからないので、差分は出さない。")
	case relationSame:
		line("差分なし。前回と同じコミットを出す。")
	case relationRollback:
		line("### ロールバック")
		line("")
		line("次の変更が %s から外れる。", cfg.environment)
		line("")
		if err := writeChanges(ctx, cfg, g, &b, link, p.sha, p.prev.sha); err != nil {
			return "", err
		}
	default:
		if p.relation == relationDiverged {
			base, err := link(p.base)
			if err != nil {
				return "", err
			}
			line("前回のコミットが今回の祖先ではない。共通の祖先 %s からの変更を出す。", base)
			line("")
		}
		line("### 含まれる変更")
		line("")
		if err := writeChanges(ctx, cfg, g, &b, link, p.base, p.sha); err != nil {
			return "", err
		}
	}

	if len(p.migrations) > 0 {
		line("")
		line("### migration")
		line("")
		for _, m := range p.migrations {
			line("%s", m)
		}
	}

	if len(p.notices) > 0 {
		line("")
		line("### 注意")
		line("")
		for _, n := range p.notices {
			line("%s", n)
		}
	}

	if p.hasDiff() {
		shortstat, err := g.line(ctx, "diff", "--shortstat", p.prev.sha, p.sha)
		if err != nil {
			return "", err
		}
		stat, err := g(ctx, "diff", "--stat=120", p.prev.sha, p.sha)
		if err != nil {
			return "", err
		}
		line("")
		line("<details><summary>変更ファイル（%s）</summary>", strings.TrimLeft(shortstat, " "))
		line("")
		line("```")
		b.WriteString(stat)
		line("```")
		line("")
		line("</details>")
	}
	return b.String(), nil
}

var mergePR = regexp.MustCompile(`^Merge pull request #([0-9]+) `)

// from..to の first-parent の各コミットを 1 行に書く。PR の merge commit は PR 番号とタイトル（本文の空でない最初の行）にする
func writeChanges(ctx context.Context, cfg config, g git, b *strings.Builder, link func(string) (string, error), from, to string) error {
	commits, err := g.lines(ctx, "rev-list", "--first-parent", from+".."+to)
	if err != nil {
		return err
	}
	for _, c := range commits {
		subject, err := g.line(ctx, "log", "-1", "--format=%s", c)
		if err != nil {
			return err
		}
		if m := mergePR.FindStringSubmatch(subject); m != nil {
			body, err := g(ctx, "log", "-1", "--format=%b", c)
			if err != nil {
				return err
			}
			title := ""
			for _, l := range strings.Split(body, "\n") {
				if strings.TrimSpace(l) != "" {
					title = l
					break
				}
			}
			fmt.Fprintf(b, "- [#%s](%s/%s/pull/%s) %s\n", m[1], cfg.serverURL, cfg.repo, m[1], title)
			continue
		}
		l, err := link(c)
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "- %s %s\n", l, subject)
	}
	return nil
}
