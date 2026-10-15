# .github

ochanuco の repo で共有するもの。repo の設定と ruleset は [chabatake](https://github.com/ochanuco/chabatake) の `github/` が持ち、ここには repo の中から使うものを置く。

この repo は public。置いたものはすべて公開されるので、secret や個人の設定は書かない。secret は呼び出し側から渡す。

## reusable workflow

| workflow | 用途 |
|---|---|
| `pnpm.yml` | Node.js + pnpm の check。依存を入れてから `run` を実行する |
| `uv.yml` | Python + uv の check。`run` に依存の入れ方から書く |
| `rust.yml` | Rust の check。既定は fmt・clippy・test。追加のターゲットやツール（wasm-pack など）は入力で受ける |
| `go.yml` | Go の check。既定は build・vet・test。版は go.mod から読む |
| `dotnet.yml` | .NET の check。restore から test までを `run` で渡す |
| `deploy.yml` | 本番へのデプロイ。main か main 上の SHA だけを出し、承認が要るときは別の Environment で止まる |
| `deploy-plan.yml` | `deploy.yml` の前段。前回のデプロイから何が出るかを Job Summary に書き、出す SHA と承認の要否を決める |
| `release-pr.yml` | `main` → `production` のリリース PR を作る・更新する。Environment の承認が使えない private の repo（chabatake）用 |

呼び出し側は commit SHA で固定する（`@<sha> # <短い説明>`）。更新は Renovate の PR で受ける。

必須の status check の名前は「呼び出し側のジョブ名 / 呼ばれた側のジョブ名」になる。check は `ci / check` に揃え、chabatake の ruleset はこの名前を必須にする。

### check（`ci / check`）

```yaml
# .github/workflows/ci.yml
name: ci

on:
  pull_request:
    branches: [main]
  push:
    branches: [main]

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  ci:
    uses: ochanuco/.github/.github/workflows/pnpm.yml@<sha>
```

`run` を渡さなければ `pnpm run typecheck` と `pnpm test` を実行する。pnpm の版は `package.json` の `packageManager` から読むので、repo 側に必ず書く。uv の repo は `uv.yml` を使い、`run` を必ず渡す。どの workflow も `runs-on`（既定は ubuntu-latest）を受け、pnpm・uv・go は `working-directory` も受ける。

CI の最低限は「ビルドか型チェックが通る」こと。テストがある repo はテストも `run` に入れる。Node.js の repo は npm を使わず pnpm にする。

### デプロイ

本番は main のコミットだけを出す。既定は main への push で自動的に出し、承認が要るのは `deploy-plan.yml` が危ないと判定したときだけにする。認証情報は Environment の secret `OP_SA_DEPLOY`（1Password の service account）から `op run` で渡す。Cloudflare Workers の本番の Worker はこの経路だけで出し、Workers Builds には接続しない。Workers Builds を使うのは、PR ごとの Preview（Preview Builds の `wrangler preview`）や staging の Worker が要る repo だけにする。

```yaml
# .github/workflows/deploy.yml
name: deploy
run-name: deploy <repo> @ ${{ inputs.ref || github.sha }}

on:
  push:
    branches: [main]
  workflow_dispatch:
    inputs:
      ref:
        description: デプロイする ref（main、または main 上のコミット SHA。古い SHA はロールバック）
        required: true
        default: main

permissions:
  contents: read

# run 全体を直列にする。承認待ちの run があると後の run は待ち、待っている run は最新の 1 つだけが残る。
# 名前は deploy.yml のジョブの concurrency（deploy-<environment>）と変える（同じ名前だと deadlock として取り消される）
concurrency:
  group: deploy-run-production
  cancel-in-progress: false

jobs:
  plan:
    uses: ochanuco/.github/.github/workflows/deploy-plan.yml@<sha>
    permissions:
      contents: read
      actions: read
    with:
      ref: ${{ inputs.ref || 'main' }}
      approval-environment: production-approval
      notices: |
        migrations/=D1 migration を含む。デプロイの後に手で apply する
  deploy:
    needs: plan
    uses: ochanuco/.github/.github/workflows/deploy.yml@<sha>
    with:
      ref: ${{ needs.plan.outputs.sha }}
      approval-environment: ${{ needs.plan.outputs.approval-environment }}
      op-env-file: .github/deploy/production.env.op
      run: scripts/deploy.sh production
    secrets: inherit
```

Environment は 2 つ置く。

| Environment | reviewers | secret | 役目 |
|---|---|---|---|
| `production` | なし | `OP_SA_DEPLOY` | デプロイのジョブが使う |
| `production-approval` | あり | なし | 承認が要るときだけ、デプロイの前に止まる |

どちらも deployment branch を main に限る。

`plan` は Job Summary に今回と前回の SHA、承認の要否と理由、含まれる PR、変更ファイルを書く。承認が要るのは次のとき。承認待ちの run を開くと Summary が見えるので、内容を確かめてから承認する。

- 前回のデプロイの記録がない
- ロールバック（前回より古い SHA を指定した）、または前回のコミットが今回の祖先ではない
- `notices` に書いたパスに変更がある。`notices` は、デプロイの前後に手作業が要るパスの先頭と文言を 1 行ずつ書く

前回と今回の決め方:

- `plan` は `main` を run を起動した時点の SHA（`github.sha`）に固定し、`deploy` にはその SHA を渡す。承認を待つ間に main が進んでも、承認した内容がそのまま出る
- 前回の SHA は、同じ workflow の直近の成功 run から読む。run 名の `@` の後ろがコミットならそれ、`main` ならその run の head SHA。このため `run-name` は `... @ ${{ inputs.ref || github.sha }}` の形にし、`plan` には `actions: read` を渡す

手動でだけ出す repo は、`push` のきっかけと `approval-environment` を外し、`production` に reviewers を置く。この場合はすべての run が `deploy` ジョブの前で承認を待つ。

### リリース PR

chabatake 専用。Environment の承認が使えない private の repo で、`production` ブランチへのリリース PR のマージを承認とする。呼び出し方は chabatake の `.github/workflows/release-pr.yml` を参照する。

PR の作成者は GitHub App にする。`GITHUB_TOKEN` で作った PR では後続の workflow が動かないため。App の `APP_ID` と `APP_PRIVATE_KEY` を repo の secret に置く。

## 版

`v<major>.<minor>.<patch>` のタグと GitHub Release で版を出す。入力を消す・必須にするなど呼び出し側の書き換えが要る変更は major、入力を足す変更は minor、それ以外は patch にする。呼び出し側は `@<sha> # v1.2.3` の形で固定し、Renovate が新しい版の PR を出す。

## Renovate の preset

| preset | 中身 | 使う repo |
|---|---|---|
| `github>ochanuco/.github` | `config:recommended`、action の SHA 固定、月曜朝、公開から 7 日経った版だけ、`dependencies` ラベル。更新は tooling と runtime（本番で動く依存）に分け、それぞれ non-major と major の PR にまとめる | Renovate を使うすべての repo |
| `github>ochanuco/.github:automerge` | tooling の non-major を CI が通ったら自動マージする。runtime は人がレビューする | `ci / check` が必須になっている repo だけ。check がない repo では検証なしでマージされる |

repo 側はコメントを書ける `renovate.json5` にし、`extends` に並べたうえで repo 固有のルール（ファイルごとのまとめ方など）だけを足す。

```json5
{
  $schema: 'https://docs.renovatebot.com/renovate-schema.json',
  extends: ['github>ochanuco/.github', 'github>ochanuco/.github:automerge'],
}
```

preset 自体（`default.json`、`automerge.json`）は `.json` のままにする。Renovate は `github>ochanuco/.github` の形では `.json` の preset しか探さないため。説明は各ルールの `description` に書く。
