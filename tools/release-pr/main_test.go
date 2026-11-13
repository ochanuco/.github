package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type testRepo struct {
	t   *testing.T
	dir string
	g   git
}

// 呼び出し側の checkout の代わり。利用者の git の設定（署名など）を読まず、作者と日時を固定する
func newRepo(t *testing.T) *testRepo {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_AUTHOR_DATE":     "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME":  "test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_COMMITTER_DATE":  "2026-01-01T00:00:00Z",
		"LC_ALL":              "C",
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	r := &testRepo{t: t, dir: dir, g: newGit(dir)}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	out, err := r.g.line(context.Background(), args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *testRepo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		p := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// GitHub の merge commit の形で main に入れる
func (r *testRepo) mergePR(number int, files map[string]string) string {
	r.t.Helper()
	branch := fmt.Sprintf("pr-%d", number)
	r.git("checkout", "-q", "-b", branch)
	r.commit("change for "+branch, files)
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", branch, "-m", fmt.Sprintf("Merge pull request #%d from ochanuco/%s", number, branch))
	return r.git("rev-parse", "HEAD")
}

// workflow の「Update release branch」と同じ形で、production の先端に main の tree の snapshot を積む
func (r *testRepo) promote(production string) {
	r.t.Helper()
	r.git("update-ref", "refs/remotes/origin/main", "main")
	r.git("update-ref", "refs/remotes/origin/production", production)
	sha := r.git("commit-tree", "main^{tree}", "-p", production, "-m", "release: sync production to main")
	if r.git("rev-parse", "main^{tree}") == r.git("rev-parse", production+"^{tree}") {
		sha = production
	}
	r.git("update-ref", "refs/heads/release/production", sha)
}

// production に、main の今の tree の snapshot を入れる（前回のリリースの merge）
func (r *testRepo) release(production string) string {
	r.t.Helper()
	return r.git("commit-tree", "main^{tree}", "-p", production, "-m", "Merge pull request #1 from ochanuco/release/production")
}

type prState struct {
	state, mergeState string
	fail              bool
}

type fakeGitHub struct {
	t      *testing.T
	open   []pullRequest
	states []prState
	// 受けた操作（"list"・"create <title>"・"edit #<n>"・"state #<n>"）
	calls []string
	body  string
}

func (f *fakeGitHub) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			f.t.Errorf("Authorization = %q", got)
		}
		var in map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&in)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/ochanuco/app/pulls":
			q := r.URL.Query()
			if q.Get("state") != "open" || q.Get("base") != "production" || q.Get("head") != "ochanuco:release/production" {
				f.t.Errorf("query = %q", r.URL.RawQuery)
			}
			f.calls = append(f.calls, "list")
			json.NewEncoder(w).Encode(f.open)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/ochanuco/app/pulls":
			if in["head"] != "release/production" || in["base"] != "production" {
				f.t.Errorf("create = %v", in)
			}
			f.calls = append(f.calls, fmt.Sprintf("create %s", in["title"]))
			f.body, _ = in["body"].(string)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(pullRequest{Number: 501, HTMLURL: "https://github.com/ochanuco/app/pull/501"})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/ochanuco/app/pulls/"):
			f.calls = append(f.calls, "edit #"+strings.TrimPrefix(r.URL.Path, "/repos/ochanuco/app/pulls/"))
			f.body, _ = in["body"].(string)
			json.NewEncoder(w).Encode(pullRequest{})
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			vars, _ := in["variables"].(map[string]any)
			if vars["owner"] != "ochanuco" || vars["name"] != "app" {
				f.t.Errorf("variables = %v", vars)
			}
			f.calls = append(f.calls, fmt.Sprintf("state #%v", vars["number"]))
			s := f.states[0]
			if len(f.states) > 1 {
				f.states = f.states[1:]
			}
			if s.fail {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"state":%q,"mergeStateStatus":%q}}}}`, s.state, s.mergeState)
		default:
			f.t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func testDeps(t *testing.T, g git, f *fakeGitHub, stdout *bytes.Buffer, slept *[]time.Duration) deps {
	srv := f.start()
	return deps{
		git:    g,
		gh:     &github{client: srv.Client(), apiURL: srv.URL, graphqlURL: srv.URL + "/graphql", repo: "ochanuco/app", token: "test-token"},
		stdout: stdout,
		now:    func() time.Time { return time.Date(2026, 11, 13, 15, 28, 24, 0, time.UTC) },
		sleep:  func(d time.Duration) { *slept = append(*slept, d) },
	}
}

func head(mainSHA string) string {
	return "## 概要\n\n" +
		"`main` (`" + mainSHA + "`) の内容を `production` に昇格するリリースPRです。\n\n" +
		"`main` に新しい PR が merge されるたびに、このPRは main 先端の snapshot に自動更新されます。merge 時点の diff がそのまま本番に deploy される内容です。\n\n" +
		"このPRは本番昇格の承認ゲートであり、コードの再レビュー用ではありません。通常のコードレビューとCIは `main` に入る通常PRで完了済みです。\n\n"
}

const deployNote = "merge すると apply が動きます。\n"

func TestRunPR(t *testing.T) {
	type scenario struct {
		cfg    config
		gh     *fakeGitHub
		calls  []string
		body   string // 作るか書き換えた本文。空なら PR を触らない
		stdout string
		output string // GITHUB_OUTPUT
		err    bool
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, r *testRepo, cfg config) scenario
	}{
		{"前回のリリースがあり PR がない", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", map[string]string{"a": "1"})
			production := r.release(r.git("rev-parse", "main"))
			r.mergePR(12, map[string]string{"b": "1"})
			r.commit("typo を直す", map[string]string{"a": "2"})
			r.mergePR(13, map[string]string{"c": "1"})
			r.promote(production)
			cfg.mainSHA = r.git("rev-parse", "main")
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t},
				calls:  []string{"list", "create 🚀リリース 2026-11-14 00:28:24"},
				body:   head(cfg.mainSHA) + "## 含まれる変更\n\n- #13\n- typo を直す\n- #12\n\n## Deploy\n\n" + deployNote,
				stdout: "release/production is 1 commit(s) ahead of production.\nhttps://github.com/ochanuco/app/pull/501\n",
				output: "has_changes=true\n"}
		}},
		{"PR が既にある", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", map[string]string{"a": "1"})
			production := r.release(r.git("rev-parse", "main"))
			r.mergePR(12, map[string]string{"b": "1"})
			r.promote(production)
			cfg.mainSHA = r.git("rev-parse", "main")
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t, open: []pullRequest{{Number: 500}}},
				calls:  []string{"list", "edit #500"},
				body:   head(cfg.mainSHA) + "## 含まれる変更\n\n- #12\n\n## Deploy\n\n" + deployNote,
				stdout: "release/production is 1 commit(s) ahead of production.\nUpdated PR #500\n",
				output: "has_changes=true\n"}
		}},
		{"前回のリリースが main にない", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", map[string]string{"a": "1"})
			r.git("checkout", "-q", "--orphan", "old")
			production := r.commit("old production", map[string]string{"old": "1"})
			r.git("checkout", "-q", "main")
			r.mergePR(12, map[string]string{"b": "1"})
			r.promote(production)
			cfg.mainSHA = r.git("rev-parse", "main")
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t},
				calls:  []string{"list", "create 🚀リリース 2026-11-14 00:28:24"},
				body:   head(cfg.mainSHA) + "## 含まれる変更\n\n" + noBase + "\n\n## Deploy\n\n" + deployNote,
				stdout: "release/production is 1 commit(s) ahead of production.\nhttps://github.com/ochanuco/app/pull/501\n",
				output: "has_changes=true\n"}
		}},
		{"main の先端がもう production と同じ tree", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", nil)
			r.mergePR(12, map[string]string{"b": "1"})
			production := r.release(r.git("rev-parse", "main"))
			r.git("update-ref", "refs/remotes/origin/main", "main")
			r.git("update-ref", "refs/remotes/origin/production", production)
			// production より先にあるが、tree は main の先端と同じ
			r.git("update-ref", "refs/heads/release/production", r.git("commit-tree", "main^{tree}", "-p", production, "-m", "release"))
			cfg.mainSHA = r.git("rev-parse", "main")
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t},
				calls:  []string{"list", "create 🚀リリース 2026-11-14 00:28:24"},
				body:   head(cfg.mainSHA) + "## 含まれる変更\n\n" + noCommits + "\n\n## Deploy\n\n" + deployNote,
				stdout: "release/production is 1 commit(s) ahead of production.\nhttps://github.com/ochanuco/app/pull/501\n",
				output: "has_changes=true\n"}
		}},
		{"変更がない", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", nil)
			r.mergePR(12, map[string]string{"b": "1"})
			r.promote(r.release(r.git("rev-parse", "main")))
			cfg.mainSHA = r.git("rev-parse", "main")
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t},
				stdout: "::notice::production is already up to date with main; skipping production release PR creation.\n",
				output: "has_changes=false\n"}
		}},
		{"body-command とチェックリスト", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", map[string]string{"a": "1"})
			production := r.release(r.git("rev-parse", "main"))
			r.mergePR(12, map[string]string{"units.txt": "- web\n- api"})
			r.commit("revert - Merge pull request #9 from ochanuco/x", nil)
			r.promote(production)
			r.git("checkout", "-q", "release/production")
			t.Setenv("GOTOOLCHAIN", "local")
			cfg.mainSHA = r.git("rev-parse", "main")
			// 呼び出し側の checkout で動き、改行で終わらない出力もそのまま入る
			cfg.bodyCommand = `printf '## 反映される単位\n\n'; cat units.txt; printf '\nGOTOOLCHAIN=%s' "${GOTOOLCHAIN-unset}"`
			cfg.checklist = "- [ ] 確かめた\n\n"
			cfg.deployNote = "1 行目\n2 行目"
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t},
				calls: []string{"list", "create 🚀リリース 2026-11-14 00:28:24"},
				body: head(cfg.mainSHA) +
					"## 反映される単位\n\n- web\n- api\nGOTOOLCHAIN=unset\n" +
					"## 含まれる変更\n\n- revert - #9\n- #12\n\n" +
					"## Merge前チェック\n\n- [ ] 確かめた\n\n\n" +
					"## Deploy\n\n1 行目\n2 行目\n",
				stdout: "release/production is 1 commit(s) ahead of production.\nhttps://github.com/ochanuco/app/pull/501\n",
				output: "has_changes=true\n"}
		}},
		{"body-command が失敗する", func(t *testing.T, r *testRepo, cfg config) scenario {
			r.commit("init", map[string]string{"a": "1"})
			production := r.release(r.git("rev-parse", "main"))
			r.mergePR(12, map[string]string{"b": "1"})
			r.promote(production)
			cfg.mainSHA = r.git("rev-parse", "main")
			cfg.bodyCommand = "echo partial; exit 3"
			return scenario{cfg: cfg, gh: &fakeGitHub{t: t}, err: true,
				stdout: "release/production is 1 commit(s) ahead of production.\n",
				output: "has_changes=true\n"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			cfg := config{repoDir: r.dir, deployNote: deployNote, outputPath: filepath.Join(t.TempDir(), "output")}
			s := tt.setup(t, r, cfg)
			var stdout bytes.Buffer
			var slept []time.Duration
			err := runPR(context.Background(), s.cfg, testDeps(t, r.g, s.gh, &stdout, &slept))
			if (err != nil) != s.err {
				t.Fatalf("err = %v", err)
			}
			if !slices.Equal(s.gh.calls, s.calls) {
				t.Errorf("calls = %q, want %q", s.gh.calls, s.calls)
			}
			if s.gh.body != s.body {
				t.Errorf("body:\n%s\nwant:\n%s", s.gh.body, s.body)
			}
			if stdout.String() != s.stdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", stdout.String(), s.stdout)
			}
			output, _ := os.ReadFile(s.cfg.outputPath)
			if string(output) != s.output {
				t.Errorf("GITHUB_OUTPUT = %q, want %q", output, s.output)
			}
		})
	}
}

func TestRunCheck(t *testing.T) {
	const waiting = "mergeability still computing; waiting.\n"
	tests := []struct {
		name   string
		open   []pullRequest
		states []prState
		failed bool
		stdout string
		sleeps int
	}{
		{name: "open な PR がない", stdout: "::notice::no open release PR; nothing to check.\n"},
		{name: "CLEAN", open: []pullRequest{{Number: 500}}, states: []prState{{state: "OPEN", mergeState: "CLEAN"}},
			stdout: "::notice::release PR #500 state=CLEAN; branch is up to date.\n"},
		{name: "BLOCKED も遅れていない", open: []pullRequest{{Number: 500}}, states: []prState{{state: "OPEN", mergeState: "BLOCKED"}},
			stdout: "::notice::release PR #500 state=BLOCKED; branch is up to date.\n"},
		{name: "BEHIND", open: []pullRequest{{Number: 500}}, states: []prState{{state: "OPEN", mergeState: "BEHIND"}}, failed: true,
			stdout: "::error::release PR #500 is BEHIND production. Re-run this workflow to rebuild release/production on the latest production tip.\n"},
		{name: "UNKNOWN の後に決まる", open: []pullRequest{{Number: 500}},
			states: []prState{{state: "OPEN", mergeState: "UNKNOWN"}, {fail: true}, {state: "OPEN", mergeState: "CLEAN"}}, sleeps: 2,
			stdout: waiting + waiting + "::notice::release PR #500 state=CLEAN; branch is up to date.\n"},
		{name: "待つ間に merge された", open: []pullRequest{{Number: 500}},
			states: []prState{{state: "OPEN", mergeState: "UNKNOWN"}, {state: "MERGED", mergeState: "UNKNOWN"}}, sleeps: 1,
			stdout: waiting + "::notice::release PR #500 is already MERGED; nothing to check.\n"},
		{name: "close された", open: []pullRequest{{Number: 500}}, states: []prState{{state: "CLOSED", mergeState: "BEHIND"}},
			stdout: "::notice::release PR #500 is already CLOSED; nothing to check.\n"},
		{name: "決まらない", open: []pullRequest{{Number: 500}}, states: []prState{{state: "OPEN", mergeState: "UNKNOWN"}}, failed: true, sleeps: 6,
			stdout: strings.Repeat(waiting, 6) + "::error::mergeability not confirmed after retries; operator should verify the PR is not behind.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeGitHub{t: t, open: tt.open, states: tt.states}
			var stdout bytes.Buffer
			var slept []time.Duration
			err := runCheck(context.Background(), testDeps(t, nil, f, &stdout, &slept))
			if errors.Is(err, errFailed) != tt.failed || (err != nil && !errors.Is(err, errFailed)) {
				t.Fatalf("err = %v", err)
			}
			if stdout.String() != tt.stdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", stdout.String(), tt.stdout)
			}
			if len(slept) != tt.sleeps || slices.ContainsFunc(slept, func(d time.Duration) bool { return d != 5*time.Second }) {
				t.Errorf("slept = %v, want %d × 5s", slept, tt.sleeps)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{
		"GH_TOKEN": "t", "REPO": "ochanuco/app", "REPO_DIR": "/w", "MAIN_SHA": "abc", "DEPLOY_NOTE": "note",
		"GITHUB_API_URL": "https://ghe.example.com/api/v3/",
	}
	cfg, err := loadConfig("pr", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.graphqlURL != "https://ghe.example.com/api/v3/graphql" || cfg.repoDir != "/w" || cfg.deployNote != "note" {
		t.Errorf("cfg = %+v", cfg)
	}
	// check は PR の本文に使うものを要らない
	if _, err := loadConfig("check", func(k string) string { return map[string]string{"GH_TOKEN": "t", "REPO": "ochanuco/app"}[k] }); err != nil {
		t.Error(err)
	}
	if _, err := loadConfig("pr", func(k string) string { return map[string]string{"GH_TOKEN": "t", "REPO": "ochanuco/app"}[k] }); err == nil ||
		!strings.Contains(err.Error(), "REPO_DIR, MAIN_SHA, DEPLOY_NOTE") {
		t.Errorf("err = %v", err)
	}
	if _, err := loadConfig("deploy", func(string) string { return "x" }); err == nil {
		t.Error("知らないサブコマンドを受けた")
	}
}
