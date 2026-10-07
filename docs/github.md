# GitHub repository selection

Use `provider: github` and exact `owner/repository` paths. Public GitHub uses
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
    exclude:
      repositories:
        - example/web
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

Exclusions override inclusions, including the canonical path returned by GitHub.
Duplicate IDs are processed once within the connection. Archived repositories
and forks are excluded by default; each has its own enabling option. This slice
accepts one connection and exact repository selection. It does not discover
organizations or all repositories accessible to the token.

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
