package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type previous struct {
	sha, runURL string
}

// 前回デプロイした SHA を、呼び出し側の workflow の直近の成功 run から読む。
// run 名（`... @ <ref>`）の ref がコミットならそれ、main（または ref がない）ならその run の head_sha。
// run を読めないときは警告を出して「記録なし」として続ける
func findPrevious(ctx context.Context, cfg config, httpClient *http.Client, g git, stdout io.Writer) (previous, error) {
	run, err := latestSuccessfulRun(ctx, cfg, httpClient)
	if err != nil {
		fmt.Fprintln(stdout, "::warning::前回の deploy run を読めなかった（呼び出し側のジョブに actions: read が要る）")
		return previous{}, nil
	}
	if run == nil {
		return previous{}, nil
	}
	prev := previous{sha: run.HeadSHA, runURL: run.HTMLURL}
	if i := strings.LastIndex(run.DisplayTitle, "@ "); i >= 0 {
		ref := run.DisplayTitle[i+len("@ "):]
		if ref != "main" {
			if resolved, err := g.line(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
				prev.sha = resolved
			}
		}
	}
	return prev, nil
}

type workflowRun struct {
	DisplayTitle string `json:"display_title"`
	HeadSHA      string `json:"head_sha"`
	HTMLURL      string `json:"html_url"`
}

func latestSuccessfulRun(ctx context.Context, cfg config, httpClient *http.Client) (*workflowRun, error) {
	u := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?status=success&per_page=1",
		strings.TrimSuffix(cfg.apiURL, "/"), cfg.repo, url.PathEscape(cfg.workflowFile))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	var body struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.WorkflowRuns) == 0 {
		return nil, nil
	}
	return &body.WorkflowRuns[0], nil
}
