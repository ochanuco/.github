# .github

ochanuco の repo で共有するもの。repo の設定と ruleset は [chabatake](https://github.com/ochanuco/chabatake) の `github/` が持ち、ここには repo の中から使うものを置く。

この repo は public。置いたものはすべて公開されるので、secret や個人の設定は書かない。secret は呼び出し側から渡す。

## reusable workflow

| workflow | 用途 |
|---|---|
| `pnpm.yml` | Node.js + pnpm の check。依存を入れてから `run` を実行する |
| `uv.yml` | Python + uv の check。`run` に依存の入れ方から書く |
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

`run` を渡さなければ `pnpm run typecheck` と `pnpm test` を実行する。uv の repo は `uv.yml` を使い、`run` を必ず渡す。

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
    secrets:
      APP_ID: ${{ secrets.APP_ID }}
      APP_PRIVATE_KEY: ${{ secrets.APP_PRIVATE_KEY }}
```
