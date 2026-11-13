package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type github struct {
	client     *http.Client
	apiURL     string
	graphqlURL string
	repo       string
	token      string
}

type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

func (gh *github) do(ctx context.Context, method, u string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+gh.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := gh.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %s: %s", method, u, resp.Status, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (gh *github) rest(path string) string {
	return strings.TrimSuffix(gh.apiURL, "/") + "/repos/" + gh.repo + path
}

// release/production から production への open な PR。なければ nil
func (gh *github) openReleasePR(ctx context.Context) (*pullRequest, error) {
	owner, _, _ := strings.Cut(gh.repo, "/")
	q := url.Values{"state": {"open"}, "base": {productionBranch}, "head": {owner + ":" + releaseBranch}}
	var prs []pullRequest
	if err := gh.do(ctx, http.MethodGet, gh.rest("/pulls?"+q.Encode()), nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

func (gh *github) createReleasePR(ctx context.Context, title, body string) (pullRequest, error) {
	var pr pullRequest
	err := gh.do(ctx, http.MethodPost, gh.rest("/pulls"), map[string]string{
		"title": title, "head": releaseBranch, "base": productionBranch, "body": body,
	}, &pr)
	return pr, err
}

func (gh *github) editBody(ctx context.Context, number int, body string) (pullRequest, error) {
	var pr pullRequest
	err := gh.do(ctx, http.MethodPatch, gh.rest(fmt.Sprintf("/pulls/%d", number)), map[string]string{"body": body}, &pr)
	return pr, err
}

// state（OPEN・MERGED・CLOSED）と mergeStateStatus（BEHIND・UNKNOWN など）。
// どちらも GraphQL の enum で、REST の mergeable_state とは値が違う
func (gh *github) mergeState(ctx context.Context, number int) (state, mergeState string, err error) {
	owner, name, _ := strings.Cut(gh.repo, "/")
	in := map[string]any{
		"query": `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { state mergeStateStatus } }
}`,
		"variables": map[string]any{"owner": owner, "name": name, "number": number},
	}
	var out struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					State            string `json:"state"`
					MergeStateStatus string `json:"mergeStateStatus"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := gh.do(ctx, http.MethodPost, gh.graphqlURL, in, &out); err != nil {
		return "", "", err
	}
	if len(out.Errors) > 0 {
		return "", "", fmt.Errorf("graphql: %s", out.Errors[0].Message)
	}
	pr := out.Data.Repository.PullRequest
	if pr == nil {
		return "", "", fmt.Errorf("PR #%d がない", number)
	}
	return pr.State, pr.MergeStateStatus, nil
}
