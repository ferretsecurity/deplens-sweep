# First batch run

Use [sweep-test.yaml](../sweep-test.yaml) to scan four public repositories:

- GitHub: [go-chi/chi](https://github.com/go-chi/chi) and [go-chi/render](https://github.com/go-chi/render).
- GitLab: [gitlab-org/cli](https://gitlab.com/gitlab-org/cli) and [gitlab-org/api/client-go](https://gitlab.com/gitlab-org/api/client-go).

The config scans one repository at a time, allows five minutes per repository,
and requires 1 GiB of free space before starting a clone. Edit the repository
lists to try your own projects. Paths are relative to the config file.
This demo keeps the usual ignored directories and also skips `testdata`.
The GitLab CLI repository contains a duplicate-key YAML fixture in that directory;
including it produces a failed extraction and an incomplete batch. Remove
`testdata` from the ignore list if you want those fixtures in your inventory.

## Set your tokens

Open `.env` in the repository root and paste the token values between the quotes:

```sh
GITHUB_TOKEN='paste-your-github-token-here'
GITLAB_TOKEN='paste-your-gitlab-token-here'
```

`run-test.sh` loads `.env` and exports both values for the CLI. The config's
`token_env` fields name those environment variables. `.env` is ignored by Git
and has file permissions `600`, so only your user can read or write it.
The CLI itself does not automatically load `.env`; use the wrapper below.

## Create temporary tokens

### GitHub

1. Open [the fine-grained token form](https://github.com/settings/personal-access-tokens/new?name=deplens-sweep-test&expires_in=7). It prefills the name and a seven-day lifetime.
2. Keep your personal account as the resource owner.
3. Under Repository access, select Public repositories. These examples need no added permissions; fine-grained tokens include read access to public repositories.
4. Generate the token and paste it into `GITHUB_TOKEN` in `.env`.

See [GitHub's token instructions](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens).
For a later private-repository run, select the repository owner and specific
repositories, with Metadata and Contents read permissions. See
[GitHub's permissions reference](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens).

### GitLab

1. Open [Personal access tokens](https://gitlab.com/-/user_settings/personal_access_tokens). You can also reach it through your avatar, Edit profile, then Access and Personal access tokens.
2. Under Generate token, select Legacy token.
3. Name it `deplens-sweep-test` and set the expiry date to seven days from the day you create it.
4. Select only `read_api` and `read_repository`.
5. Generate the token and paste it into `GITLAB_TOKEN` in `.env` before leaving the page.

See [GitLab's token instructions](https://docs.gitlab.com/user/profile/personal_access_tokens/)
and [scope definitions](https://docs.gitlab.com/security/tokens/access_token_scopes/).

Both tokens expire automatically. To stop using them earlier, delete the GitHub
token or revoke the GitLab token on their respective token settings pages.

## Run

Use Linux and Git. Both CLIs have been built into `test-run/bin` in this
workspace, and the config points to that scanner.

```sh
cd /home/jekos/ghq/github.com/ferretsecurity/deplens-sweep
mkdir -p test-run
./run-test.sh > test-run/summary.json
python3 -m json.tool test-run/summary.json
```

`summary.json` captures the batch summary printed to stdout. Errors go to the
terminal's stderr. An incomplete batch returns a nonzero exit status; inspect
each repository's `outcome` and `error` fields to see why.

## Inspect the results

```text
test-run/
  summary.json
  results/
    batches/<batch-id>.json
    connections/<connection-name>/<repository-key>/
      latest.json
      attempts/<attempt-id>/
        scan.json
        metadata.json
```

- The batch JSON records selected repositories, outcomes, counts, and timings.
- `scan.json` contains the dependency inventory, check runs, and findings.
- `metadata.json` records the repository, scanned revision, scanner identity,
  configuration, and attempt outcome.
- `latest.json` references the latest complete inventory for that repository.

The summary's `result` paths are relative to `test-run/results`. Temporary
checkouts are removed after each attempt. If a clone fails before the scanner
runs, its attempt can have metadata without `scan.json`.

Run the same command again to try reuse. Repositories with unchanged revisions,
scanner, and scan configuration should report `reused` when their retained
results are complete. Their original `scanned_at` stays the same, and
`checked_at` advances. Changed inputs and failed or partial results trigger a
new attempt.

The `test-run` directory is ignored by Git so generated output stays local.

To rebuild the binaries later, use Go 1.25.6 or newer:

```sh
go build -o ./test-run/bin/deplens-sweep ./cmd/deplens-sweep
cd ../deplens
go build -o ../deplens-sweep/test-run/bin/deplens ./cmd/deplens
```
