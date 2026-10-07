# Temporary failures and retries

Each API request and each Git clone can make at most three attempts in one
batch. Retries wait 250 milliseconds after the first failure and 500
milliseconds after the second. A successful retry produces one repository
outcome, not a separate result for every acquisition attempt.

GitHub and GitLab requests retry HTTP 429, 500, 502, 503, and 504, plus transport
timeouts and interrupted connections or response bodies. GitHub HTTP 403 retries
only when `Retry-After` is present or `X-RateLimit-Remaining` is zero. Ordinary
403, 401, 404, invalid JSON, and other permanent errors fail without retries.
Known empty-repository responses retain their separate empty outcome.

`Retry-After` accepts seconds or an HTTP date. The next request waits at least
the requested delay and at least the normal retry delay. The maximum accepted
server delay is 30 seconds. A longer or invalid delay produces a visible failure;
the command does not retry before that server delay. Requests have a 30-second
HTTP timeout. A successful API response body is limited to 16 MiB. A failed
pagination page retries that page, preserving earlier pages and known selected
repositories. API redirects remain rejected, and pagination stays on the
configured API origin.

Git clone retries use a conservative list of network and temporary server error
messages. Authentication, permission, missing repositories, certificate errors,
disk errors, and unrecognized errors do not retry. Git uses English diagnostics
for this classification. Raw Git diagnostics stay private, and inherited Git
trace settings and injected configuration parameters are removed. Credential
helpers remain disabled.

Before retrying a clone, the command removes its incomplete checkout. Cloning,
retry waits, checkout verification, and scanning share the same per-repository
`timeout`. Retries never renew that budget. If it expires, the attempt fails and
its checkout is removed. Other selected repositories can still run. Exhausted
API or clone retries produce failed outcomes and a nonzero batch exit status.

Deplens analysis runs once per attempt. An analysis failure can run again in a
later batch, rather than repeatedly scanning inside this retry loop.
