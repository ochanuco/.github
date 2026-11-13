package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const deleteFile = "\x00delete"

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
		if content == deleteFile {
			if err := os.Remove(p); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
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

// GitHub の merge commit の形（件名に PR 番号、本文の 1 行目に PR のタイトル）で main に入れる
func (r *testRepo) mergePR(number int, title string, files map[string]string) string {
	r.t.Helper()
	branch := fmt.Sprintf("pr-%d", number)
	r.git("checkout", "-q", "-b", branch)
	r.commit("change for "+branch, files)
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", branch, "-m", fmt.Sprintf("Merge pull request #%d from ochanuco/%s", number, branch), "-m", title)
	return r.git("rev-parse", "HEAD")
}

func (r *testRepo) link(sha string) string {
	return fmt.Sprintf("[`%s`](https://github.com/ochanuco/app/commit/%s)", r.git("rev-parse", "--short", sha), sha)
}

func (r *testRepo) details(prev, sha string) string {
	return "\n<details><summary>変更ファイル（" + strings.TrimLeft(r.git("diff", "--shortstat", prev, sha), " ") + "）</summary>\n\n```\n" +
		r.git("diff", "--stat=120", prev, sha) + "\n```\n\n</details>\n"
}

type fakeAPI struct {
	t      *testing.T
	status int
	body   string
}

func (f fakeAPI) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/ochanuco/app/actions/workflows/deploy.yml/runs" {
			f.t.Errorf("path = %q", r.URL.Path)
		}
		if q := r.URL.Query(); q.Get("status") != "success" || q.Get("per_page") != "1" {
			f.t.Errorf("query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			f.t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(f.status)
		fmt.Fprint(w, f.body)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func runsBody(t *testing.T, runs ...workflowRun) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"total_count": len(runs), "workflow_runs": runs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func deployRun(title, headSHA string) workflowRun {
	return workflowRun{DisplayTitle: title, HeadSHA: headSHA, HTMLURL: "https://github.com/ochanuco/app/actions/runs/1"}
}

func testConfig(t *testing.T, r *testRepo, apiURL, sha string) config {
	dir := t.TempDir()
	return config{
		apiURL:              apiURL,
		serverURL:           "https://github.com",
		repo:                "ochanuco/app",
		workflowFile:        "deploy.yml",
		token:               "test-token",
		repoDir:             r.dir,
		sha:                 sha,
		environment:         "production",
		approvalEnvironment: "production-approval",
		outputPath:          filepath.Join(dir, "output"),
		summaryPath:         filepath.Join(dir, "summary"),
	}
}

const runLink = "（[run](https://github.com/ochanuco/app/actions/runs/1)）"

func TestRun(t *testing.T) {
	type scenario struct {
		cfg     config
		api     fakeAPI
		want    string // Job Summary
		warning bool
		// GITHUB_OUTPUT の approval-environment
		approval string
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, r *testRepo) scenario
	}{
		{"前回の記録がない", func(t *testing.T, r *testRepo) scenario {
			sha := r.commit("init", map[string]string{"manifests/a.yaml": "a", "migrations/0001.sql": "DROP TABLE a;"})
			api := fakeAPI{t: t, status: 200, body: runsBody(t)}
			cfg := testConfig(t, r, api.start().URL, sha)
			// 差分を出さないので、notices と migrations は見ない
			cfg.notices = []notice{{"manifests/", "手で反映する"}}
			cfg.migrationDirs = []string{"migrations/"}
			return scenario{cfg: cfg, approval: "production-approval", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(sha) + "\n" +
				"- 前回: 記録なし\n" +
				"- 承認: 要る（前回のデプロイの記録がない）。Environment `production-approval` で止まる\n\n" +
				"前回のデプロイが見つからないので、差分は出さない。\n"}
		}},
		{"前回の run を読めない", func(t *testing.T, r *testRepo) scenario {
			sha := r.commit("init", nil)
			api := fakeAPI{t: t, status: 403, body: `{"message":"Resource not accessible by integration"}`}
			return scenario{cfg: testConfig(t, r, api.start().URL, sha), warning: true, approval: "production-approval", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(sha) + "\n" +
				"- 前回: 記録なし\n" +
				"- 承認: 要る（前回のデプロイの記録がない）。Environment `production-approval` で止まる\n\n" +
				"前回のデプロイが見つからないので、差分は出さない。\n"}
		}},
		{"approval-environment が空なら承認を判定しない", func(t *testing.T, r *testRepo) scenario {
			sha := r.commit("init", nil)
			api := fakeAPI{t: t, status: 200, body: runsBody(t)}
			cfg := testConfig(t, r, api.start().URL, sha)
			cfg.approvalEnvironment = ""
			return scenario{cfg: cfg, approval: "", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(sha) + "\n" +
				"- 前回: 記録なし\n\n" +
				"前回のデプロイが見つからないので、差分は出さない。\n"}
		}},
		{"前回と同じ", func(t *testing.T, r *testRepo) scenario {
			sha := r.commit("init", nil)
			api := fakeAPI{t: t, status: 200, body: runsBody(t, deployRun("deploy app @ main", sha))}
			return scenario{cfg: testConfig(t, r, api.start().URL, sha), approval: "", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(sha) + "\n" +
				"- 前回: " + r.link(sha) + runLink + "\n" +
				"- 承認: 要らない。このまま出る\n\n" +
				"差分なし。前回と同じコミットを出す。\n"}
		}},
		{"前に進む", func(t *testing.T, r *testRepo) scenario {
			prev := r.commit("init", map[string]string{"src/a.go": "a"})
			r.mergePR(12, "機能を足す", map[string]string{"src/b.go": "b"})
			direct := r.commit("typo を直す", map[string]string{"README.md": "x"})
			api := fakeAPI{t: t, status: 200, body: runsBody(t, deployRun("deploy app @ main", prev))}
			cfg := testConfig(t, r, api.start().URL, direct)
			cfg.notices = []notice{{"manifests/", "手で反映する"}}
			cfg.migrationDirs = []string{"migrations/"}
			return scenario{cfg: cfg, approval: "", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(direct) + "\n" +
				"- 前回: " + r.link(prev) + runLink + "\n" +
				"- 承認: 要らない。このまま出る\n\n" +
				"### 含まれる変更\n\n" +
				"- " + r.link(direct) + " typo を直す\n" +
				"- [#12](https://github.com/ochanuco/app/pull/12) 機能を足す\n" +
				r.details(prev, direct)}
		}},
		{"notices と migration に当たる", func(t *testing.T, r *testRepo) scenario {
			prev := r.commit("init", map[string]string{"migrations/0001.sql": "CREATE TABLE a (id int);"})
			sha := r.mergePR(3, "設定と migration", map[string]string{
				"manifests/a.yaml":    "a",
				"manifests/b.yaml":    "b",
				"migrations/0002.sql": "CREATE INDEX i ON a(id);",
				"migrations/0003.sql": "DROP TABLE a;",
			})
			api := fakeAPI{t: t, status: 200, body: runsBody(t, deployRun("deploy app @ main", prev))}
			cfg := testConfig(t, r, api.start().URL, sha)
			cfg.notices = []notice{{"manifests/", "デプロイの後に手で反映する"}, {"docs/", "当たらない"}}
			cfg.migrationDirs = []string{"migrations/"}
			return scenario{cfg: cfg, approval: "production-approval", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(sha) + "\n" +
				"- 前回: " + r.link(prev) + runLink + "\n" +
				"- 承認: 要る（注意に当たる変更がある、追加系でない migration がある）。Environment `production-approval` で止まる\n\n" +
				"### 含まれる変更\n\n" +
				"- [#3](https://github.com/ochanuco/app/pull/3) 設定と migration\n\n" +
				"### migration\n\n" +
				"- `migrations/0002.sql`: 追加系だけ（1 文）\n" +
				"- ⚠️ `migrations/0003.sql`: 追加系でない文が 1 個\n" +
				"  - `DROP TABLE a`\n\n" +
				"### 注意\n\n" +
				"- デプロイの後に手で反映する（`manifests/` の 2 ファイル）\n" +
				r.details(prev, sha)}
		}},
		{"ロールバック", func(t *testing.T, r *testRepo) scenario {
			old := r.commit("init", nil)
			r.mergePR(5, "新しい画面", map[string]string{"web/a.ts": "a"})
			newer := r.commit("ログを足す", map[string]string{"web/b.ts": "b"})
			// run 名の SHA が head_sha より優先される
			api := fakeAPI{t: t, status: 200, body: runsBody(t, deployRun("deploy app @ "+newer, old))}
			return scenario{cfg: testConfig(t, r, api.start().URL, old), approval: "production-approval", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(old) + "\n" +
				"- 前回: " + r.link(newer) + runLink + "\n" +
				"- 承認: 要る（ロールバック）。Environment `production-approval` で止まる\n\n" +
				"### ロールバック\n\n" +
				"次の変更が production から外れる。\n\n" +
				"- " + r.link(newer) + " ログを足す\n" +
				"- [#5](https://github.com/ochanuco/app/pull/5) 新しい画面\n" +
				r.details(newer, old)}
		}},
		{"前回が今回の祖先ではない", func(t *testing.T, r *testRepo) scenario {
			base := r.commit("init", nil)
			r.git("checkout", "-q", "-b", "side")
			side := r.commit("main にないコミット", map[string]string{"side.txt": "s"})
			r.git("checkout", "-q", "main")
			sha := r.commit("main のコミット", map[string]string{"main.txt": "m"})
			api := fakeAPI{t: t, status: 200, body: runsBody(t, deployRun("deploy app @ main", side))}
			return scenario{cfg: testConfig(t, r, api.start().URL, sha), approval: "production-approval", want: "## production にデプロイする内容\n\n" +
				"- 今回: " + r.link(sha) + "\n" +
				"- 前回: " + r.link(side) + runLink + "\n" +
				"- 承認: 要る（前回のコミットが今回の祖先ではない）。Environment `production-approval` で止まる\n\n" +
				"前回のコミットが今回の祖先ではない。共通の祖先 " + r.link(base) + " からの変更を出す。\n\n" +
				"### 含まれる変更\n\n" +
				"- " + r.link(sha) + " main のコミット\n" +
				r.details(side, sha)}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			s := tt.setup(t, r)
			var stdout bytes.Buffer
			if err := run(context.Background(), s.cfg, http.DefaultClient, r.g, &stdout); err != nil {
				t.Fatal(err)
			}
			warning := "::warning::前回の deploy run を読めなかった（呼び出し側のジョブに actions: read が要る）\n"
			wantStdout := s.want
			if s.warning {
				wantStdout = warning + s.want
			}
			if got := stdout.String(); got != wantStdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", got, wantStdout)
			}
			if got := readFile(t, s.cfg.summaryPath); got != s.want {
				t.Errorf("summary:\n%s\nwant:\n%s", got, s.want)
			}
			if got, want := readFile(t, s.cfg.outputPath), "approval-environment="+s.approval+"\n"; got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		})
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFindPrevious(t *testing.T) {
	r := newRepo(t)
	head := r.commit("one", nil)
	other := r.commit("two", nil)
	short := r.git("rev-parse", "--short", other)
	tests := []struct {
		name    string
		api     fakeAPI
		want    previous
		warning bool
	}{
		{"成功した run がない", fakeAPI{status: 200, body: runsBody(t)}, previous{}, false},
		{"API がエラーを返す", fakeAPI{status: 404, body: `{"message":"Not Found"}`}, previous{}, true},
		{"JSON が読めない", fakeAPI{status: 200, body: `{`}, previous{}, true},
		{"run 名が main なら head_sha", fakeAPI{status: 200, body: runsBody(t, deployRun("deploy app @ main", head))}, previous{head, "https://github.com/ochanuco/app/actions/runs/1"}, false},
		{"run 名に @ がなければ head_sha", fakeAPI{status: 200, body: runsBody(t, deployRun("deploy app", head))}, previous{head, "https://github.com/ochanuco/app/actions/runs/1"}, false},
		{"run 名の SHA", fakeAPI{status: 200, body: runsBody(t, deployRun("deploy app @ "+other, head))}, previous{other, "https://github.com/ochanuco/app/actions/runs/1"}, false},
		{"run 名の短い SHA は完全な SHA にする", fakeAPI{status: 200, body: runsBody(t, deployRun("deploy app @ "+short, head))}, previous{other, "https://github.com/ochanuco/app/actions/runs/1"}, false},
		{"最後の @ の後ろを読む", fakeAPI{status: 200, body: runsBody(t, deployRun("deploy a @ b @ "+other, head))}, previous{other, "https://github.com/ochanuco/app/actions/runs/1"}, false},
		{"run 名の ref がコミットでなければ head_sha", fakeAPI{status: 200, body: runsBody(t, deployRun("deploy app @ deadbeef", head))}, previous{head, "https://github.com/ochanuco/app/actions/runs/1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.api.t = t
			cfg := testConfig(t, r, tt.api.start().URL, head)
			var stdout bytes.Buffer
			got, err := findPrevious(context.Background(), cfg, http.DefaultClient, r.g, &stdout)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if warned := strings.HasPrefix(stdout.String(), "::warning::"); warned != tt.warning {
				t.Errorf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestRelate(t *testing.T) {
	r := newRepo(t)
	a := r.commit("a", nil)
	b := r.commit("b", nil)
	r.git("checkout", "-q", "-b", "side", a)
	side := r.commit("side", nil)
	tests := []struct {
		name      string
		prev, sha string
		want      relation
		wantBase  string
	}{
		{"記録なし", "", b, relationNone, ""},
		{"同じ", b, b, relationSame, ""},
		{"ロールバック", b, a, relationRollback, ""},
		{"前に進む", a, b, relationForward, a},
		{"分かれている", side, b, relationDiverged, a},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, base, err := relate(context.Background(), r.g, tt.prev, tt.sha)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || base != tt.wantBase {
				t.Errorf("got %s %s, want %s %s", got, base, tt.want, tt.wantBase)
			}
		})
	}

	t.Run("前回のコミットがない", func(t *testing.T) {
		if _, _, err := relate(context.Background(), r.g, strings.Repeat("0", 40), b); err == nil {
			t.Error("エラーにならない")
		}
	})
}

func TestMatchNotices(t *testing.T) {
	r := newRepo(t)
	prev := r.commit("init", map[string]string{"manifests/a.yaml": "a"})
	sha := r.commit("change", map[string]string{"manifests/a.yaml": "b", "manifests/sub/c.yaml": "c", "manifests.txt": "x", "src/a.go": "a"})
	got, err := matchNotices(context.Background(), r.g, []notice{
		{"manifests/", "手で反映する"},
		{"docs/", "当たらない"},
		{"src/a.go", "a=b を含む文言"},
	}, prev, sha)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"- 手で反映する（`manifests/` の 2 ファイル）",
		"- a=b を含む文言（`src/a.go` の 1 ファイル）",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestClassifyMigrations(t *testing.T) {
	long := "INSERT INTO t VALUES ('" + strings.Repeat("あ", 200) + "')"
	tests := []struct {
		name      string
		dirs      []string
		change    map[string]string
		want      []string
		wantBlock bool
	}{
		{
			name: "追加系だけ",
			dirs: []string{"migrations/"},
			change: map[string]string{"migrations/0002.sql": "-- 新しいテーブル; DROP TABLE a\nCREATE TABLE b (\n  id int\n);\ncreate unique index i on b(id);\nCREATE INDEX j ON b(id);\nALTER TABLE b ADD COLUMN c int;\n" +
				"CREATE TABLE IF NOT EXISTS c (id int);"},
			want: []string{"- `migrations/0002.sql`: 追加系だけ（5 文）"},
		},
		{
			name:   "空の migration",
			dirs:   []string{"migrations/"},
			change: map[string]string{"migrations/0002.sql": "-- 何もしない\n"},
			want:   []string{"- `migrations/0002.sql`: 追加系だけ（0 文）"},
		},
		{
			name: "追加系でない文は 5 個まで見せる",
			dirs: []string{"migrations/"},
			change: map[string]string{"migrations/0002.sql": "CREATE TABLE b (id int);\nDROP TABLE a;\nDELETE FROM b;\nUPDATE b SET id = 1;\n" + long + ";\nPRAGMA foreign_keys = off;\n" +
				"ALTER TABLE b RENAME TO c;\nALTER TABLE c DROP COLUMN id;"},
			want: []string{
				"- ⚠️ `migrations/0002.sql`: 追加系でない文が 7 個",
				"  - `DROP TABLE a`",
				"  - `DELETE FROM b`",
				"  - `UPDATE b SET id = 1`",
				"  - `" + string([]rune(long)[:120]) + "`",
				"  - `PRAGMA foreign_keys = off`",
			},
			wantBlock: true,
		},
		{
			name:   "コメントだけの変更",
			dirs:   []string{"migrations/"},
			change: map[string]string{"migrations/0001.sql": "-- 最初のテーブル\nCREATE  TABLE a (id\n  int); -- 末尾\n"},
			want:   []string{"- `migrations/0001.sql`: コメントだけの変更"},
		},
		{
			name:      "既存の migration の書き換え",
			dirs:      []string{"migrations/"},
			change:    map[string]string{"migrations/0001.sql": "CREATE TABLE a (id bigint);"},
			want:      []string{"- ⚠️ `migrations/0001.sql`: 既存の migration の変更（M）。適用済みの migration は書き換えない"},
			wantBlock: true,
		},
		{
			name:      "既存の migration を消す",
			dirs:      []string{"migrations/"},
			change:    map[string]string{"migrations/0001.sql": deleteFile},
			want:      []string{"- ⚠️ `migrations/0001.sql`: 既存の migration の変更（D）。適用済みの migration は書き換えない"},
			wantBlock: true,
		},
		{
			name:   ".sql でないファイルと置き場の外は見ない",
			dirs:   []string{"migrations/"},
			change: map[string]string{"migrations/README.md": "DROP", "other/0001.sql": "DROP TABLE a;"},
			want:   nil,
		},
		{
			name:   "置き場を並べた順に出す",
			dirs:   []string{"db/b/", "db/a/"},
			change: map[string]string{"db/a/0001.sql": "CREATE TABLE a (id int);", "db/b/0001.sql": "CREATE TABLE b (id int);"},
			want: []string{
				"- `db/b/0001.sql`: 追加系だけ（1 文）",
				"- `db/a/0001.sql`: 追加系だけ（1 文）",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			prev := r.commit("init", map[string]string{"migrations/0001.sql": "CREATE TABLE a (id int);\n"})
			sha := r.commit("change", tt.change)
			got, blocked, err := classifyMigrations(context.Background(), r.g, tt.dirs, prev, sha)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if blocked != tt.wantBlock {
				t.Errorf("blocked = %v, want %v", blocked, tt.wantBlock)
			}
		})
	}
}

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"空", "", nil},
		{"コメントと空の文を除く", "-- a; b\n;;\n  ;", nil},
		{"空白を詰める", "CREATE\tTABLE  a\n(\n  id int\n);", []string{"CREATE TABLE a ( id int )"}},
		{"行の途中のコメント", "CREATE TABLE a (id int); -- DROP TABLE a;\nDROP TABLE b", []string{"CREATE TABLE a (id int)", "DROP TABLE b"}},
		// 文字列リテラルの中の -- と ; も区別しない
		{"文字列の中", "INSERT INTO a VALUES ('x;y--z');", []string{"INSERT INTO a VALUES ('x", "y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitStatements(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSafeStatement(t *testing.T) {
	tests := []struct {
		stmt string
		want bool
	}{
		{"CREATE TABLE a (id int)", true},
		{"create table if not exists a (id int)", true},
		{"CREATE INDEX i ON a(id)", true},
		{"CREATE UNIQUE INDEX i ON a(id)", true},
		{"ALTER TABLE a ADD COLUMN b int", true},
		{"ALTER TABLE a ADD b int", true},
		{"ALTER TABLE a DROP COLUMN b", false},
		{"ALTER TABLE a RENAME TO b", false},
		{"CREATE VIEW v AS SELECT 1", false},
		{"CREATE TRIGGER t AFTER INSERT ON a BEGIN SELECT 1", false},
		{"DROP TABLE a", false},
		{"DELETE FROM a", false},
		{"UPDATE a SET b = 1", false},
		{"INSERT INTO a VALUES (1)", false},
		{"PRAGMA foreign_keys = off", false},
	}
	for _, tt := range tests {
		t.Run(tt.stmt, func(t *testing.T) {
			if got := safeStatement.MatchString(tt.stmt); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApprovalReasons(t *testing.T) {
	tests := []struct {
		name          string
		rel           relation
		notices, gate bool
		want          []string
	}{
		{"記録なし", relationNone, false, false, []string{"前回のデプロイの記録がない"}},
		{"同じ", relationSame, false, false, nil},
		{"前に進むだけ", relationForward, false, false, nil},
		{"ロールバック", relationRollback, false, false, []string{"ロールバック"}},
		{"分かれている", relationDiverged, false, false, []string{"前回のコミットが今回の祖先ではない"}},
		{"notices", relationForward, true, false, []string{"注意に当たる変更がある"}},
		{"migration", relationForward, false, true, []string{"追加系でない migration がある"}},
		{"すべて", relationDiverged, true, true, []string{"前回のコミットが今回の祖先ではない", "注意に当たる変更がある", "追加系でない migration がある"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := approvalReasons(tt.rel, tt.notices, tt.gate); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	base := map[string]string{
		"GITHUB_TOKEN": "token",
		"REPO":         "ochanuco/app",
		"REPO_DIR":     "/work/app",
		"SHA":          "abc",
		"ENVIRONMENT":  "production",
		"WORKFLOW_REF": "ochanuco/app/.github/workflows/deploy.yml@refs/heads/main",
	}
	with := func(extra map[string]string) func(string) string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return func(k string) string { return env[k] }
	}

	t.Run("既定値", func(t *testing.T) {
		cfg, err := loadConfig(with(nil))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.apiURL != "https://api.github.com" || cfg.serverURL != "https://github.com" {
			t.Errorf("apiURL = %q, serverURL = %q", cfg.apiURL, cfg.serverURL)
		}
		if cfg.workflowFile != "deploy.yml" {
			t.Errorf("workflowFile = %q", cfg.workflowFile)
		}
		if cfg.notices != nil || cfg.migrationDirs != nil || cfg.approvalEnvironment != "" {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("notices と migrations", func(t *testing.T) {
		cfg, err := loadConfig(with(map[string]string{
			"NOTICES":    "manifests/=デプロイの後に手で反映する\n\nconfig/=a=b\nnoequal\n",
			"MIGRATIONS": "migrations/\n\ndb/\n",
		}))
		if err != nil {
			t.Fatal(err)
		}
		wantNotices := []notice{{"manifests/", "デプロイの後に手で反映する"}, {"config/", "a=b"}, {"noequal", "noequal"}}
		if !slices.Equal(cfg.notices, wantNotices) {
			t.Errorf("notices = %q", cfg.notices)
		}
		if !slices.Equal(cfg.migrationDirs, []string{"migrations/", "db/"}) {
			t.Errorf("migrationDirs = %q", cfg.migrationDirs)
		}
	})

	t.Run("足りない環境変数", func(t *testing.T) {
		_, err := loadConfig(with(map[string]string{"SHA": "", "GITHUB_TOKEN": ""}))
		if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "SHA") {
			t.Errorf("err = %v", err)
		}
	})
}
