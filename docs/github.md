# GitHub repository and organization selection

Use `provider: github` with exact `owner/repository` paths, organization names,
or an explicit `all_accessible: true` opt-in. Public GitHub uses
`https://api.github.com` for discovery and `https://github.com` for HTTPS clones.
For GitHub Enterprise Server, set the hosting server URL, without `/api/v3`.
The CLI appends `/api/v3` for API requests and checks that clone URLs use HTTPS
on the configured server.

```yaml
results_dir: ./results
connections:
  - name: github
    provider: github
    server_url: https://github.com
    token_env: GITHUB_TOKEN
    include:
      repositories:
        - example/api
        - example/web
      organizations:
        - example
    exclude:
      repositories:
        - example/web
      organizations:
        - excluded-team
    all_accessible: false
    include_archived: false
    include_forks: false
deplens:
  executable: deplens
```

For an enterprise instance, replace `server_url` with a URL such as
`https://github.example.com`. Connection history remains separate by connection
name, provider, server, and repository ID. Renames retain the repository ID and
save the current path in metadata.

Set `GITHUB_TOKEN` in the environment. Use a personal access token that can read
the selected repositories through the REST API and HTTPS Git. Fine-grained
personal access tokens need repository Metadata and Contents read permissions.
See [GitHub's permissions reference](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens).
Private repositories also require any organization approval or SSO authorization
that the server enforces. SSH credentials and automatic token renewal are outside
this tool's scope.

API authentication uses a Bearer header. Git receives the token through its
child environment and askpass password prompt. Tokens are excluded from clone
URLs, process arguments, command diagnostics, and saved results. API redirects
are rejected. Provider error response bodies are not retained.

Repository exclusions match exact paths. Organization exclusions match the
repository owner and override explicit repository inclusions, including the
current owner returned by GitHub after a rename. Excluding `example` does not
exclude `example-other`. Organizations do not have subgroup selection semantics,
and nested names or wildcards are rejected. Exclusions win regardless of YAML
order.

Overlapping organization and repository selections run each provider repository
ID once per connection. Archived repositories and forks are excluded by default;
each has its own enabling option. This slice accepts one connection.

Organization discovery lists every page of the selected organization's accessible
repositories. Each batch discovers again, so newly added matching repositories
are picked up. To include the authenticated user's accessible repositories,
set `all_accessible: true` on the connection, with optional exclusions:

```yaml
connections:
  - name: github
    provider: github
    server_url: https://github.com
    token_env: GITHUB_TOKEN
    all_accessible: true
    exclude:
      repositories: [example/legacy]
      organizations: [excluded-team]
```

Add the top-level `results_dir` and `deplens` settings as in the first example.
All-accessible discovery uses GitHub's authenticated-user repository listing for
owned repositories, collaborator access, and organization membership. It does
not enumerate every public repository on GitHub. This opt-in can be combined
with exact repository and organization selections. All exclusions and
archive/fork defaults still apply. Missing inclusions remain a configuration
error unless this opt-in is true.

Pagination follows `rel="next"` links on the configured API origin. Public
GitHub pagination stays on `api.github.com`; enterprise pagination stays under
the configured installation's `/api/v3`. Untrusted URLs, loops, and invalid
responses cause discovery failure without discarding already known repositories.
A failed later page or an authorization error records a failed outcome, sets
`discovery_complete: false`, and makes the command exit nonzero. Independent
selected repositories and repositories from successful earlier pages still get
processed. A successful empty listing has no repository outcomes. Saved history
remains available when a repository disappears from the current selection.

See GitHub's [repository API reference](https://docs.github.com/en/rest/repos/repos)
for organization and authenticated-user repository visibility.

The CLI resolves the default branch commit before cloning and checks out that
exact commit, including if the branch advances during acquisition. It uses a
shallow clone and omits submodule and Git LFS content. Scan configuration,
history, metadata, batch outcomes, and cleanup follow the workflow described in
the [README](../README.md).

A repository lookup must succeed before emptiness can be considered. A commit
lookup returning HTTP 409 with the structured message `Git Repository is empty.`
produces an `empty` outcome without cloning. A missing default branch, access
failure, or another conflict response remains a failure. GitHub documents that
[HTTP 409 can mean an empty or unavailable repository](https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-your-git-database).
Independent selected repositories continue after a failure, and the batch returns
nonzero for failures or incomplete discovery.
