// deploy-plan は deploy-plan.yml の summary の step。前回のデプロイから何が出るかを Job Summary に書き、承認の要否を出力する。
// 判定の条件は README の「デプロイ」の節にある
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

type notice struct {
	prefix, message string
}

type config struct {
	apiURL       string
	serverURL    string
	repo         string
	workflowFile string
	token        string
	// 呼び出し側の checkout。ツールは別の checkout から動くので、git はここで実行する
	repoDir             string
	sha                 string
	environment         string
	notices             []notice
	migrationDirs       []string
	approvalEnvironment string
	outputPath          string
	summaryPath         string
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	g := newGit(cfg.repoDir)
	if err := run(context.Background(), cfg, &http.Client{Timeout: 30 * time.Second}, g, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func loadConfig(getenv func(string) string) (config, error) {
	var missing []string
	need := func(key string) string {
		v := getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	cfg := config{
		apiURL:              getenv("GITHUB_API_URL"),
		serverURL:           getenv("GITHUB_SERVER_URL"),
		token:               need("GITHUB_TOKEN"),
		repo:                need("REPO"),
		repoDir:             need("REPO_DIR"),
		sha:                 need("SHA"),
		environment:         need("ENVIRONMENT"),
		approvalEnvironment: getenv("APPROVAL_ENVIRONMENT"),
		outputPath:          getenv("GITHUB_OUTPUT"),
		summaryPath:         getenv("GITHUB_STEP_SUMMARY"),
	}
	workflowRef := need("WORKFLOW_REF")
	if len(missing) > 0 {
		return config{}, fmt.Errorf("環境変数がない: %s", strings.Join(missing, ", "))
	}
	if cfg.apiURL == "" {
		cfg.apiURL = "https://api.github.com"
	}
	if cfg.serverURL == "" {
		cfg.serverURL = "https://github.com"
	}
	// WORKFLOW_REF は呼び出し側の owner/repo/.github/workflows/<file>@<ref>
	if i := strings.LastIndex(workflowRef, "@"); i >= 0 {
		workflowRef = workflowRef[:i]
	}
	cfg.workflowFile = path.Base(workflowRef)

	for _, line := range strings.Split(getenv("NOTICES"), "\n") {
		if line == "" {
			continue
		}
		prefix, message, ok := strings.Cut(line, "=")
		if !ok {
			message = line
		}
		cfg.notices = append(cfg.notices, notice{prefix: prefix, message: message})
	}
	for _, dir := range strings.Split(getenv("MIGRATIONS"), "\n") {
		if dir != "" {
			cfg.migrationDirs = append(cfg.migrationDirs, dir)
		}
	}
	return cfg, nil
}

func run(ctx context.Context, cfg config, httpClient *http.Client, g git, stdout io.Writer) error {
	prev, err := findPrevious(ctx, cfg, httpClient, g, stdout)
	if err != nil {
		return err
	}
	p, err := makePlan(ctx, cfg, g, prev)
	if err != nil {
		return err
	}

	if cfg.outputPath != "" {
		if err := appendFile(cfg.outputPath, "approval-environment="+p.approval+"\n"); err != nil {
			return err
		}
	}

	summary, err := render(ctx, cfg, g, p)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(stdout, summary); err != nil {
		return err
	}
	if cfg.summaryPath != "" {
		return appendFile(cfg.summaryPath, summary)
	}
	return nil
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
