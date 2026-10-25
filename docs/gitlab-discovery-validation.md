# GitLab auto discovery validation

Validated on 2026-10-24 after changing `auto` to make fallback local to each
group. A child group tries recursive listing even when its parent needed
direct-project listing and subgroup traversal. `recursive` and `walk` retain
their existing behavior.

## Local checks

Passed:

- `go test ./... -count=1`
- `go vet ./...`
- `GOFLAGS=-race go test ./... -run 'TestGitLabAuto(NestedFallback|ChildFailure|RecoversRepeated500|FallbackIsLocal)' -count=1`

The regression tests cover repeated HTTP 500 at the root and a nested group,
successful recursive listing in siblings and grandchildren, pagination,
retaining projects before a later-page failure, deduplication, exclusions,
empty groups, and continuing independent branches after a permission failure.
Existing retry, cancellation, redaction, and explicit-walk tests also pass.

## Live discovery

The full group test used [gitlab-com](https://gitlab.com/gitlab-com).
GitLab initially reported 1,142 projects across 12 pages. Default archive and
fork exclusions selected 921 projects.

| Measurement | Result |
| --- | ---: |
| Unique selected repository IDs | 921 |
| Resolved default-branch commits | 847 |
| Confirmed empty repositories | 61 |
| Repository failures | 13 |
| Group-listing requests | 12 |
| Commit-lookup requests, including retries | 850 |
| Total discovery requests | 862 |
| Discovery duration | 683.59 seconds |
| Group fallbacks | 0 |

A separate complete recursive listing returned the same 921 selected IDs.
There were no missing, extra, or duplicate IDs. Its requests and duration are
outside the discovery measurements above.

The live test returned nonzero because of 13 repository failures:

- Twelve projects had no default branch and reported `empty_repo: false`.
  Direct project reads confirmed private or disabled repository access.
- `gitlab-com/gl-infra/auto_deploy_dashboard` returned HTTP 500 on all three
  commit-lookup attempts. A subsequent direct commit request also returned 500.

These failures did not prevent complete group enumeration. They remain visible
failures under the existing policy; access restrictions do not count as empty
repositories. This group had no recursive-listing HTTP 500, so nested fallback
was verified with local fixtures rather than a naturally failing live group.

### Rate-limit constraint

An initial unpaced attempt on `gitlab-org`, which reported 3,775 projects,
stopped after 177 requests because GitLab requested a retry delay exceeding the
client's 30-second limit. Its response headers identified a free-plan burst
limit of 100 requests per minute. `booking-com` returned HTTP 404 with the
available token.

The complete `gitlab-com` test started requests 750 ms apart. This pacing exists
only in the opt-in live test; production concurrency and retry limits are
unchanged. The measured duration is not a throughput benchmark or evidence for
the original 17-minute estimate. An unpaced CLI run with this token can still
hit the rate limit.

See [the live-test command](gitlab.md#live-discovery-test) to repeat the test.
Local evidence from this run is in the ignored `test-run/group-discovery/`
directory: `gitlab-com.json`, `gitlab-com.log`, and `project-failures.json`.

## End-to-end scan sample

The rebuilt CLI cloned, scanned, and saved complete inventories for:

- `gitlab-org/cli`: 5 dependency sources at
  `a6fd50cecb4bbe630dfb03a621cbe2cedfef4aef`.
- `gitlab-org/api/client-go`: 8 dependency sources at
  `85bf78109b8eaaf99d9361c226f2ebd88dc12e35`.

Both were fresh attempts using the real deplens executable. The batch exited
successfully with two complete outcomes and no failures, and temporary
checkouts were removed. As in the existing sample configuration, `testdata`
was excluded because it contains intentionally invalid dependency fixtures.
The local summary is `test-run/group-discovery/sample-summary.json`.
