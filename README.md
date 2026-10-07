# deplens-sweep

See [Inventory history and reuse](docs/reuse.md) for result references and reuse inputs.

Run one batch of dependency inventory scans with an installed `deplens` CLI. This
initial version supports one named GitLab connection and exact project paths.
It processes repositories sequentially and keeps each scan attempt on disk.

## Build and run

Install Go 1.25.6 or newer, Git, and deplens. HTTPS cloning uses Git's askpass
support and a POSIX shell. Build the CLI:

```sh
go build -o deplens-sweep ./cmd/deplens-sweep
./deplens-sweep --config sweep.yaml
```

Set the token in the environment variable named by your connection. For GitLab,
use a token with API read access and repository read access. Configuration stores
only the variable's name. The CLI sends the token in the API header and supplies
it to HTTPS Git authentication through the child environment and askpass. It
never inserts the token into a clone URL or command arguments. External command
error output is omitted, and known tokens are redacted from saved JSON.

```yaml
results_dir: ./results
temp_dir: ./checkouts
connections:
  - name: company
    provider: gitlab
    server_url: https://gitlab.example.com
    token_env: GITLAB_TOKEN
    include:
      repositories:
        - engineering/api
        - engineering/web
    exclude:
      repositories:
        - engineering/web
    include_archived: false
    include_forks: false
deplens:
  executable: deplens
  # rules: ./rules.yaml
  # extend_rules: [./extra-rules.yaml]
  # ignore: [.git, node_modules, .venv, venv, vendor, .tox, .mypy_cache, .pytest_cache]
  # disable_rules: [go-mod]
  # disable_checks: [go-sum-missing]
  # exclude_presets: [snyk]
```

Paths are relative to the configuration file. A bare `deplens.executable` name is
looked up in `PATH`. `temp_dir` is optional and defaults to the system temporary
directory. Missing `ignore` uses the list shown above, matching current deplens
defaults. An explicit empty list scans all directories. Every repository gets
the same scan configuration. Output is always JSON; generation flags are not
supported.

Selections must be exact project paths. Missing inclusions are an error.
Exclusions win regardless of order, including the canonical path returned by
GitLab. Duplicate project IDs run once. Archived repositories and forks are
excluded by default, with independent options to include each. Unknown YAML
keys, GitHub, multiple connections, group selection, all-accessible selection,
concurrency, timeouts and disk thresholds are rejected by this initial version.
These are separate implementation slices of the parent spec.

The configured URL may include the installation's base path. Use HTTPS for real
servers. HTTP is accepted for local fixtures. Clone URLs must use HTTPS on the
configured host, have no embedded credentials, and remain within the configured
installation path. API redirects are rejected rather than forwarding a token.

Each scan resolves the default branch's commit first, makes a shallow clone,
and checks out that exact commit. If the branch advanced, Git fetches the
resolved commit and verifies `HEAD` before scanning. An unavailable commit is a
failed attempt. Submodules and Git LFS content are not downloaded. Each checkout
is removed before the next repository starts.

Run this command through cron, systemd, CI, or another external scheduler when
needed. The CLI has no scheduling loop.

## Saved results

The CLI prints its JSON batch summary and writes the same summary under
`results_dir/batches/<batch-id>.json`. It saves the summary after every repository
outcome. The summary contains the configured selections, current repository
paths, resolved commits, outcomes, counts, and relative result references.
A historical repository directory does not imply membership in this batch.

History is connection-scoped:

```text
results/
  batches/<batch-id>.json
  connections/<connection-name>/<repository-key>/
    latest.json
    attempts/<attempt-id>/
      scan.json
      metadata.json
```

The repository key hashes provider, server URL and provider repository ID. A
rename keeps its history when the server, connection name and repository ID stay
the same. Each attempt retains separate scanner output and metadata. Metadata
records the current path, revision, scanner identity, effective scan flags,
start/end times, acquisition scope and outcome. Scanner identity includes its
SHA-256 executable digest and the Go build version when present. This also
identifies development builds that have no released version string.

`complete` means Git acquired the verified revision, deplens exited successfully,
and structured analysis reported no partial extraction, failed extraction,
analysis error diagnostics or failed check executions. Unsupported extraction,
coverage warnings, skipped checks and policy findings do not make a scan fail.
`failed` means discovery, acquisition, analysis or saving did not complete.
Confirmed empty repositories have an `empty` outcome and are not cloned.
An API access error is a failure, never evidence that a repository is empty.

Valid scan JSON from a failed or partial analysis is retained for diagnosis.
Missing or invalid output has metadata without a `scan.json` file. `latest.json`
points to the latest complete result only after its scan and metadata files have
been saved. A failed current attempt does not replace that pointer or count as
an older successful scan. Earlier attempts remain available. This initial slice
scans again on every invocation; unchanged-result reuse is a later slice.

Deplens records the absolute temporary checkout root in its output. That directory
is removed after scanning. Use the saved repository and revision metadata to
interpret paths in the inventory.

The command continues after individual repository failures and returns a nonzero
exit status if any repository fails or discovery is incomplete. A storage failure
also returns nonzero. Concurrent invocations and crash recovery are not yet
supported in this slice; use one invocation per results directory.

## Verify

Tests invoke the public CLI with a local GitLab HTTP fixture, real temporary Git
repositories, a Git process fixture that redirects HTTPS to local files, and a
controlled deplens executable. They inspect exit status, saved output, revision,
authentication and checkout cleanup.

```sh
go test ./...
go vet ./...
SWEEP_REAL_DEPLENS=/absolute/path/to/deplens go test ./...
```

The final command also runs the real scanner integration. It scans a known
requirements file and checks its extracted dependency through the saved JSON.
The real-scanner test skips when `SWEEP_REAL_DEPLENS` is unset.
