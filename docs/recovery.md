# Locking and interruption recovery

Sweep takes an exclusive nonblocking `flock` on `<results_dir>/.lock` before it
changes batch state or recovers abandoned work. A competing invocation exits
with `results directory is locked by another invocation`. Paths that resolve to
the same results directory share the lock. The lock file stays in place; the
kernel releases its lock when the process exits or crashes. Child Git and scanner
processes do not inherit the lock descriptor.

Temporary work uses a private namespace under `temp_dir`, or under
`<results_dir>/.work` when `temp_dir` is absent. The namespace name includes a
digest of the canonical results path. Each invocation creates a `run-<batch-id>`
directory with a `.owner.json` marker. The marker records the format, canonical
results location, run name, owner PID and process creation identity, and a unique
command ownership token. Tokens for hosting providers never enter this marker.

On the next invocation, sweep inspects marked runs in that namespace. It removes
only directories with valid matching ownership markers whose owner has stopped.
It preserves unknown entries, symlinks, and markers for other results locations.
An owner that is still active prevents cleanup. Changing `temp_dir` changes the
namespace inspected by recovery, so keep the same temporary root when recovering
an interrupted invocation.

After a forced kill, Git or scanner descendants may still be active. Sweep gives
external commands an inherited `SWEEP_WORK_OWNER` environment value. Recovery
finds surviving processes with that run's token, pins each with a Linux pidfd,
verifies its creation identity, and terminates it before removing the checkout.
It waits for termination and preserves the checkout if safe termination fails.
PID reuse cannot direct a pidfd signal at an unrelated replacement process.
Recovery of surviving commands requires Linux pidfd support, introduced in
Linux 5.3. Normal cancellation also stops command process groups.

Attempt persistence uses `<results_dir>/.staging/run-<batch-id>/`, with the same
ownership marker. Sweep writes and flushes the scanner output and metadata there,
then renames the complete attempt directory into repository history. Only afterward
does it atomically replace `latest.json`. An interruption during a staged write
leaves the previous latest complete reference intact. Startup removes abandoned
marked staging directories. Unpublished output never qualifies for reuse.

Batch summaries start with `status: running` and no completed time. A completed
invocation records `complete` or `incomplete` and a completion time. A forced kill
leaves the interrupted batch's running record visible. The next invocation makes
a new batch, reuses fully saved matching complete results, and retries unfinished
work. It does not turn an older inventory into the result of an unfinished current
attempt. See [Inventory history and reuse](reuse.md) for result eligibility.
