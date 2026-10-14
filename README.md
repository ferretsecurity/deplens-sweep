# deplens-sweep

Collect dependency inventories from selected GitHub and GitLab repositories in
one Go CLI invocation. deplens-sweep discovers repositories, resolves their
default-branch commits, reuses matching complete results, shallow-clones work
that needs analysis, invokes an installed deplens CLI, saves JSON, and removes
checkouts. Run it through your own scheduler when needed.

## Build and run

Install Go 1.25.6 or newer, Git, deplens, and a POSIX shell. The execution and
locking code targets Linux. Recovery of surviving commands uses Linux pidfds,
available from Linux 5.3.

```sh
go build -o deplens-sweep ./cmd/deplens-sweep
export GITLAB_TOKEN=...
export GITHUB_TOKEN=...
./deplens-sweep --config sweep.yaml
```

Use tokens with API discovery and repository read access. Tokens live in the
referenced environment variables. They are supplied to API headers and Git's
askpass environment, without appearing in clone URLs or process arguments.
Known tokens are redacted from saved JSON and command output.

```yaml
results_dir: ./results
temp_dir: ./checkouts
workers: 3
timeout: 60m
min_free_bytes: 10737418240
connections:
  - name: company-gitlab
    provider: gitlab
    server_url: https://gitlab.example.com
    token_env: GITLAB_TOKEN
    include:
      groups: [engineering]
      repositories: [tools/build]
    exclude:
      groups: [engineering/retired]
      repositories: [tools/legacy]
    include_archived: false
    include_forks: false
  - name: company-github
    provider: github
    server_url: https://github.com
    token_env: GITHUB_TOKEN
    include:
      organizations: [example-company]
      repositories: [example-tools/helper]
    exclude:
      repositories: [example-company/legacy]
deplens:
  executable: deplens
  # rules: ./rules.yaml
  # extend_rules: [./extra-rules.yaml]
  # ignore: [.git, node_modules, .venv, venv, vendor, .tox, .mypy_cache, .pytest_cache]
  # disable_rules: [go-mod]
  # disable_checks: [go-sum-missing]
  # exclude_presets: [snyk]
```

Paths are relative to the configuration file. A bare executable name is looked
up in PATH. Each connection needs a unique name and its own token reference and
selection. Connections selecting the same repository run independently, while
overlapping selections within one connection run once per provider repository
ID. All connections share the same deplens configuration.

GitLab connections can set `gitlab_group_discovery: walk` to list direct projects
and visit subgroups explicitly. The default, `auto`, retries an HTTP 500 page
once, then switches to walking if the retry also returns HTTP 500. Use
`recursive` to disable fallback. See
[GitLab discovery](docs/gitlab.md) for retry and partial-failure behavior.

Selections use exact paths. GitLab groups include all nested subgroups; excluded
groups remove their entire subtree. GitHub organizations do not have subgroup
semantics. Exclusions override every inclusion. Archived repositories and forks
are excluded by default, with separate options to enable them. Missing explicit
selection is an error unless the connection sets `all_accessible: true`.
All-accessible discovery still applies exclusions and archive/fork settings.

Use HTTPS for real servers. HTTP is accepted for local fixtures. HTTPS clone
URLs must match the configured hosting server and contain no credentials.
GitHub.com uses api.github.com for discovery; self-hosted GitHub uses its
`/api/v3` endpoint. GitLab uses `/api/v4` under the configured installation path.
API pagination and redirects cannot forward credentials to another origin.

Only the default branch is supported. The resolved commit is checked out and
verified even if the branch moves during acquisition. Clones use shallow history
and skip submodules and Git LFS content. Shallow history can still have a large
working tree. Submodules can be selected as separate repositories.

## Results and reuse

The command prints a JSON batch summary and saves it at
`results_dir/batches/<batch-id>.json`. It records selections, discovery status,
per-repository outcomes, result references, counts, and check times as work
finishes. Its status starts as `running` and ends as `complete` or `incomplete`.
Historical directories do not imply membership in the current batch.

```text
results/
  batches/<batch-id>.json
  connections/<connection-name>/<repository-key>/
    latest.json
    attempts/<attempt-id>/
      scan.json
      metadata.json
```

Repository keys use the provider, server URL, and stable provider repository ID.
Renaming a repository preserves its history. Each actual attempt has separate
scanner JSON and metadata describing its repository, revision, scanner identity,
effective configuration, timing, acquisition scope, and outcome. The scanner
identity includes its executable digest and Go build version when available.

Reuse requires a complete saved result matching the repository identity,
revision, scanner, and effective configuration, including custom rule contents.
A reused outcome references the original inventory and scan time while recording
a new check time. It does not copy the inventory. Every actual attempt remains
in history, and `latest.json` points to the latest complete result.

Outcomes distinguish `complete`, `reused`, `failed`, `empty`, and `unprocessed`.
Partial or failed extraction, analysis errors, failed checks, invalid output,
and process failures prevent completeness and reuse. Unsupported extraction,
coverage gaps, skipped checks, and policy findings alone do not. Valid partial
JSON is saved for diagnosis and retried in a later batch. An older complete
inventory never substitutes for a failed current attempt with changed inputs.

Confirmed empty repositories need no clone or scan. Access errors are failures,
not evidence of emptiness. The command continues after individual failures and
returns nonzero for failed repositories, incomplete discovery, or unfinished
work. Saving failures also return nonzero.

Deplens records the absolute temporary checkout root in its output. Cleanup
removes that directory. Interpret inventory paths using the saved repository
and revision metadata. History has no automatic retention or deletion policy.

## Execution and recovery

The default worker limit is three. Each worker saves its outcome and cleans its
checkout before accepting another repository. The default timeout is 60 minutes
for cloning and scanning together, including clone retries and retry delays.
Timed-out subprocess groups are terminated and the attempt is recorded as failed.

The default free-space reserve is 10 GiB on the checkout filesystem. Below the
reserve, clones pause until active work finishes and cleans up. If space remains
insufficient, selected work that cannot start is recorded as unprocessed and the
batch returns nonzero. Empty repositories and matching complete results still
proceed. The reserve is an admission threshold, not a disk quota. Set
`min_free_bytes: 0` to disable the reserve.

API and clone retries have at most three attempts with finite increasing delays.
Authentication and permission errors do not enter the transient retry loop.
Analysis failures are retried by a later invocation. See the retry documentation
for delay bounds and provider rate-limit handling.

A results-directory lock rejects overlapping invocations. After interruption,
a normal invocation recovers only tool-owned abandoned work for that results
location, retains saved complete results, and retries unfinished attempts. It
needs no resume command. Unrelated temporary files and histories remain intact.

There is no internal scheduler, database, vulnerability analysis, downstream
inventory comparison, or automatic source retention.

## Detailed configuration

- [GitLab discovery](docs/gitlab.md)
- [GitHub discovery](docs/github.md)
- [Multiple connections](docs/connections.md)
- [Reuse and result references](docs/reuse.md)
- [Workers and timeouts](docs/execution.md)
- [Locking and recovery](docs/recovery.md)
- [Retries](docs/retries.md)
- [Disk pressure](docs/disk.md)

## Verify

Tests invoke the public CLI against local provider fixtures, temporary Git
repositories, and controlled external process/resource conditions. They inspect
exit status, JSON, authentication, revision accuracy, and cleanup.

```sh
go test ./...
go vet ./...
SWEEP_REAL_DEPLENS=/absolute/path/to/deplens go test ./...
```

The real-scanner integration skips when SWEEP_REAL_DEPLENS is unset. With it set,
the test scans a known requirements file and checks the saved dependency inventory.
