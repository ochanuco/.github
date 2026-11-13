package main

import (
	"context"
	"fmt"
	"time"
)

const (
	checkAttempts = 6
	checkInterval = 5 * time.Second
)

// release/production は production の先端に main の snapshot を 1 つ積むので、通常は BEHIND にならない。
// BEHIND なら update-branch（merge commit を作る）ではなく、workflow の再実行で作り直すよう止める
func runCheck(ctx context.Context, d deps) error {
	pr, err := d.gh.openReleasePR(ctx)
	if err != nil {
		return err
	}
	if pr == nil {
		fmt.Fprintln(d.stdout, "::notice::no open release PR; nothing to check.")
		return nil
	}
	for range checkAttempts {
		state, mergeState, err := d.gh.mergeState(ctx, pr.Number)
		if err != nil {
			state, mergeState = "UNKNOWN", "UNKNOWN"
		}
		// 確認の途中で merge（auto-merge など）や close されると mergeStateStatus は UNKNOWN のままになる
		if state == "MERGED" || state == "CLOSED" {
			fmt.Fprintf(d.stdout, "::notice::release PR #%d is already %s; nothing to check.\n", pr.Number, state)
			return nil
		}
		switch mergeState {
		case "BEHIND":
			fmt.Fprintf(d.stdout, "::error::release PR #%d is BEHIND production. Re-run this workflow to rebuild release/production on the latest production tip.\n", pr.Number)
			return errFailed
		case "UNKNOWN":
			fmt.Fprintln(d.stdout, "mergeability still computing; waiting.")
			d.sleep(checkInterval)
		default:
			fmt.Fprintf(d.stdout, "::notice::release PR #%d state=%s; branch is up to date.\n", pr.Number, mergeState)
			return nil
		}
	}
	fmt.Fprintln(d.stdout, "::error::mergeability not confirmed after retries; operator should verify the PR is not behind.")
	return errFailed
}
