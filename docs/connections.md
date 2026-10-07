# Multiple connections

One invocation can select repositories through several uniquely named
connections. Each connection has its own provider, server, token reference,
inclusions, exclusions, and archive/fork settings. Missing credentials or invalid
configuration fail before repository processing starts. Authentication failures
from a provider are recorded as runtime discovery failures.

```yaml
results_dir: ./results
connections:
  - name: github-work
    provider: github
    server_url: https://github.com
    token_env: GITHUB_WORK_TOKEN
    include:
      repositories: [company/service]
  - name: gitlab-work
    provider: gitlab
    server_url: https://gitlab.example.com
    token_env: GITLAB_WORK_TOKEN
    include:
      repositories: [engineering/service]
    exclude:
      repositories: [engineering/retired]
  - name: gitlab-audit
    provider: gitlab
    server_url: https://gitlab.example.com
    token_env: GITLAB_AUDIT_TOKEN
    include:
      repositories: [engineering/service]
deplens:
  executable: deplens
  disable_rules: []
```

The top-level `deplens` configuration applies to every selected repository. There
are no connection or repository scan overrides. Tokens come from the named
environment variables. Each provider request and Git clone uses the current
connection's credentials; the scanner receives none of the configured token
environment variables.

Selections deduplicate by provider repository identity within each connection.
Connections intentionally do not deduplicate across accounts. In this example,
`engineering/service` has separate attempts and reuse state under `gitlab-work`
and `gitlab-audit`. An inventory saved through one account cannot satisfy another
connection's first scan. Connection names are part of stored identity, so keep
names stable when continuing a connection's history.

One batch summary combines all connections. Each repository outcome and saved
metadata includes `connection`, `provider`, `server_url`, and `repository_id`.
History remains separated when servers have the same repository ID or path.
Changing one connection's selection leaves other selections and reuse untouched.

Runtime discovery, authentication, clone, and scanner failures do not prevent
independent connections from processing. The combined batch reports the failures
and exits with nonzero status. Successful inventories remain saved and reusable
on later runs. A failure to persist shared results or clean up work can stop the
batch because further processing may no longer be safe.
