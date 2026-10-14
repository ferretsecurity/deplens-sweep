# GitLab repository and group selection

Use `provider: gitlab` with exact project paths, group paths, or an explicit
`all_accessible: true` opt-in. A group inclusion discovers its projects and every
nested subgroup. By default, discovery uses GitLab's recursive project listing. Projects shared from
outside the selected group are excluded from that listing. Discovery follows all
pages, using GitLab's Link or X-Next-Page headers.

```yaml
results_dir: ./results
connections:
  - name: company
    provider: gitlab
    server_url: https://gitlab.example.com
    token_env: GITLAB_TOKEN
    gitlab_group_discovery: recursive
    include:
      repositories:
        - engineering/api
      groups:
        - engineering
        - products/mobile
    exclude:
      repositories:
        - engineering/legacy
      groups:
        - engineering/private
    all_accessible: false
    include_archived: false
    include_forks: false
deplens:
  executable: deplens
```

Set `gitlab_group_discovery` on each GitLab connection:

- `recursive` lists each selected group with `include_subgroups=true`. Two
  consecutive HTTP 500 responses on one page stop that listing after one retry.
- `walk` lists direct projects with `include_subgroups=false` and
  `with_shared=false`, then lists direct subgroups and visits each one. Both
  endpoints paginate with `per_page=100`. Traversal is sequential and skips
  excluded subgroup trees and groups already visited.

Omitted or empty values currently use `recursive`. This is the transitional
behavior before automatic fallback is added. Other values and nonempty settings
on GitHub connections are configuration errors. The setting affects group
inclusions only; exact projects and `all_accessible` keep their existing behavior.
Walking requests retain the ordinary three-attempt retry budget.

A group path matches its subtree. Excluding `engineering/private` excludes
`engineering/private/project` and `engineering/private/nested/project`, including
explicitly included projects. It does not exclude
`engineering/private-other/project`. Repository exclusions match exact project
paths. Exclusions apply to both configured paths and current paths returned by
the provider, and override inclusions regardless of YAML order.

Overlapping repository and group selections run each provider repository ID once
per connection. Archived projects and forks are excluded by default. Set each
independent enabling option to include that kind of project. Paths accept no
wildcards. Every batch discovers again, so new matching projects are picked up
and existing complete inventories can be reused when their inputs match.

To select every project the token can access, set `all_accessible: true` on the
connection. This can be combined with exact repository or group inclusions. For
example:

```yaml
connections:
  - name: company
    provider: gitlab
    server_url: https://gitlab.example.com
    token_env: GITLAB_TOKEN
    all_accessible: true
    exclude:
      groups: [engineering/private]
      repositories: [engineering/legacy]
```

Add the top-level `results_dir` and `deplens` settings as in the first example.
All-accessible listing uses the authenticated Projects API, without a membership
filter. Exclusions and archive/fork defaults still apply. Missing repository and
group inclusions remain a configuration error unless this opt-in is true.

Use a token with API read access and repository read access. API requests send
PRIVATE-TOKEN authentication; HTTPS cloning uses the existing askpass workflow.
The configured server may include an installation base path. Pagination must
remain on that server's API path, without credentials or fragments, and redirects
are rejected. Response bodies from provider errors are not saved.

Discovery failure, including a failed later page or selected nested group,
records a failed outcome, marks discovery incomplete, and makes the command exit
nonzero. Repositories already discovered and independent exact selections still
get processed. During walking, a failed direct-project listing still allows
subgroup discovery. A failed subgroup page retains children from earlier pages,
and known independent branches still get processed. Failures report the affected
group and never restart the walk. A successful empty listing has no repository outcomes. Neither
access errors nor malformed responses count as an empty listing.

A batch describes only its current selected projects and outcomes. Existing
histories survive when projects are excluded, deleted, or no longer returned.
Selected projects use the same default-branch resolution, shallow acquisition,
scan configuration, result persistence, reuse, and cleanup described in the
[README](../README.md).

API behavior follows GitLab's [Groups API](https://docs.gitlab.com/api/groups/),
[Projects API](https://docs.gitlab.com/api/projects/), and
[REST pagination documentation](https://docs.gitlab.com/api/rest/).
