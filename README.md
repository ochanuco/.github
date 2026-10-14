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
| `release-pr.yml` | `main` → `production` のリリース PR を作る・更新する |

呼び出し側は commit SHA で固定する（`@<sha> # <短い説明>`）。更新は Renovate の PR で受ける。

必須の status check の名前は「呼び出し側のジョブ名 / 呼ばれた側のジョブ名」になる。check は `ci / check`、production の preflight は `preflight / check` に揃え、chabatake の ruleset はこの名前を必須にする。

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

### production の preflight（`preflight / check`）

production への昇格 PR に付ける check。repo ごとの手順は package.json の script（例: `preflight`）にまとめ、それを呼ぶ。

```yaml
# .github/workflows/production-preflight.yml
name: production preflight

on:
  pull_request:
    branches: [production]
    types: [opened, synchronize, reopened, ready_for_review]
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: production-preflight-${{ github.ref }}
  cancel-in-progress: true

jobs:
  preflight:
    uses: ochanuco/.github/.github/workflows/pnpm.yml@<sha>
    with:
      run: pnpm run preflight
    secrets:
      CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
      CLOUDFLARE_ACCOUNT_ID: ${{ secrets.CLOUDFLARE_ACCOUNT_ID }}
```

### リリース PR

PR の作成者を GitHub App にする。`GITHUB_TOKEN` で作った PR では後続の workflow が動かず、CODEOWNER が自分自身だと自分で承認できないため。App の `APP_ID` と `APP_PRIVATE_KEY` を repo の secret に置く。`production` ブランチは先に手で作っておく。

```yaml
# .github/workflows/production-release-pr.yml
name: production release PR

on:
  push:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: production-release-pr
  cancel-in-progress: false

jobs:
  release-pr:
    uses: ochanuco/.github/.github/workflows/release-pr.yml@<sha>
    with:
      checklist: |
        - [ ] rollback 手順を直近で確認している
      deploy-note: このPRを `production` に merge すると、Cloudflare Workers Builds が production deploy を開始します。
      # repo 固有の節を足すときは、Markdown を出力するコマンドを渡す（例: chabatake の「反映される単位」）
      # body-command: .github/scripts/release-units.sh
    secrets:
      APP_ID: ${{ secrets.APP_ID }}
      APP_PRIVATE_KEY: ${{ secrets.APP_PRIVATE_KEY }}
```

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
