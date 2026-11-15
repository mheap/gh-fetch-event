# gh-fetch-event

[act](https://github.com/nektos/act) is pretty great for testing and debugging GitHub Actions workflows locally, but it needs an `event.json` to be useful. By the time your workflow has failed and you want to try recreating it, the `event.json` file is long gone.

`gh-fetch-event` retrieves or reconstructs a `pull_request` or `push` event payload for a completed GitHub Actions run. It is a GitHub CLI extension written in Go and uses the authentication and host configuration from `gh`.

## Usage

```sh
gh fetch-event 123456789 --event pull_request.synchronize > event.json
act pull_request -e event.json
```

The `--event` selector is required:

- `--event=push` for push events
- `--event=pull_request.<action>` for PRs
- `--event=pull_request.<action>=value` for PR events that need filtering (e.g. `labelled`)

```sh
gh fetch-event 123456789 --event pull_request.opened --output event.json
gh fetch-event 123456789 --event pull_request.synchronize --explain
gh fetch-event 123456789 --event pull_request.labelled=bug
gh fetch-event 123456789 --event pull_request.labelled=bug --skip-missing-fields
gh fetch-event 123456789 --event pull_request.opened --repo OWNER/REPO
gh fetch-event 123456789 --event push --explain > event.json
act push -e event.json
```

## How retrieval works

The following steps are followed in order. If any step is successful, searching stops.

1. Looks for a run artifact named `debug-logs` or `event.json` and reads one matching event payload file from it. It accepts `event.json` and the event-specific filename produced by [mheap/debug-artifact](https://github.com/mheap/debug-artifact).
2. Search the repository Events API feed for a unique record matching the run metadata. This is only possible for events that occurred in the last six hours. Missing pull request fields are filled by default from current PR/repository data and the Events API actor. Push events are converted to webhook field names and augmented with repository and commit comparison data when available. Fields already present in the event are preserved. Use `--skip-missing-fields` to return the event record without this enrichment or inferred push details.
    1. Pull requests are matched by action and PR number
    2. Pushes are matched using the SHA and branch/ref, plus event time
3. The event is reconstructed using data from the APIs. The data may have changed since the run when using this strategy.

Push reconstruction uses the workflow run's head SHA and branch. It may infer `before` from the head commit's first parent, which can be wrong for multi-commit pushes or rewritten history. When both SHAs are available, commit details come from the compare API; `distinct`, the pusher email, and some historical values are not recoverable. The pusher name and sender may be inferred from the run actor, which can differ after a rerun.

## Source controls

These environment variables are useful for comparing retrieval sources:

- `GH_FETCH_EVENT_SKIP_ARTIFACT=true` skips run artifact lookup.
- `GH_FETCH_EVENT_SKIP_EVENT=true` skips the repository Events API feed.
- `GH_FETCH_EVENT_SKIP_API=true` disables current-state reconstruction when no artifact or matching Events API record is available. Run metadata and source lookup APIs are still used.

## Install and build

Build the extension executable at the repository root, then install the local checkout:

```sh
go build -o gh-fetch-event .
gh extension install .
```

Tagged releases publish precompiled binaries for GitHub CLI to install:

```sh
git tag v0.1.0
git push origin v0.1.0
gh extension install mheap/gh-fetch-event
```
