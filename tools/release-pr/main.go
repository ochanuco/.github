// release-pr は release-pr.yml の step。release/production から production へのリリース PR を作る・更新する（pr）、
// その PR が production より遅れていないかを確かめる（check）
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	productionBranch = "production"
	releaseBranch    = "release/production"
)

// ::error:: を出し終えて、step を失敗させるだけのとき
var errFailed = errors.New("failed")

type config struct {
	apiURL     string
	graphqlURL string
	repo       string
	token      string
	// 呼び出し側の checkout。ツールは別の checkout から動くので、git と body-command はここで実行する
	repoDir     string
	mainSHA     string
	checklist   string
	deployNote  string
	bodyCommand string
	outputPath  string
}

// テストで差し替えるもの
type deps struct {
	git    git
	gh     *github
	stdout io.Writer
	now    func() time.Time
	sleep  func(time.Duration)
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("使い方: release-pr pr|check")
	}
	cfg, err := loadConfig(os.Args[1], os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	d := deps{
		git:    newGit(cfg.repoDir),
		gh:     &github{client: &http.Client{Timeout: 30 * time.Second}, apiURL: cfg.apiURL, graphqlURL: cfg.graphqlURL, repo: cfg.repo, token: cfg.token},
		stdout: os.Stdout,
		now:    time.Now,
		sleep:  time.Sleep,
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "pr":
		err = runPR(ctx, cfg, d)
	case "check":
		err = runCheck(ctx, d)
	}
	if errors.Is(err, errFailed) {
		os.Exit(1)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func loadConfig(cmd string, getenv func(string) string) (config, error) {
	var missing []string
	need := func(key string) string {
		v := getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	cfg := config{
		apiURL:     getenv("GITHUB_API_URL"),
		graphqlURL: getenv("GITHUB_GRAPHQL_URL"),
		token:      need("GH_TOKEN"),
		repo:       need("REPO"),
	}
	switch cmd {
	case "pr":
		cfg.repoDir = need("REPO_DIR")
		cfg.mainSHA = need("MAIN_SHA")
		cfg.deployNote = need("DEPLOY_NOTE")
		cfg.checklist = getenv("CHECKLIST")
		cfg.bodyCommand = getenv("BODY_COMMAND")
		cfg.outputPath = getenv("GITHUB_OUTPUT")
	case "check":
	default:
		return config{}, fmt.Errorf("知らないサブコマンド: %s", cmd)
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("環境変数がない: %s", strings.Join(missing, ", "))
	}
	if !strings.Contains(cfg.repo, "/") {
		return config{}, fmt.Errorf("REPO は owner/name の形にする: %s", cfg.repo)
	}
	if cfg.apiURL == "" {
		cfg.apiURL = "https://api.github.com"
	}
	if cfg.graphqlURL == "" {
		cfg.graphqlURL = strings.TrimSuffix(cfg.apiURL, "/") + "/graphql"
	}
	return cfg, nil
}

func appendFile(name, s string) error {
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
