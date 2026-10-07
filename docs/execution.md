# Workers and deadlines

Set batch execution limits at the top level of the YAML configuration:

```yaml
workers: 3
timeout: 60m
```

Both settings are optional. `workers` defaults to 3 and must be a positive
integer. `timeout` defaults to 60 minutes and must be a positive Go duration,
for example `30m`, `90s`, or `1h`.

The worker limit bounds active repository attempts within one invocation. It
works with one connection and exact repository selections. Each active attempt
has its own temporary checkout. A worker saves its repository outcome and
removes that checkout before taking another repository. The coordinator writes
batch summaries as outcomes arrive, so concurrent scans do not write the same
summary at once.

The timeout is one total budget for Git acquisition, checkout verification, and
deplens analysis together. Discovery happens before this budget starts. A slow
clone uses time that the scanner would otherwise have. On timeout, the command
kills the affected process group, including its child processes, saves a failed
outcome, removes the checkout, and continues other selected repositories. Any
failed attempt makes the batch exit status nonzero. SIGINT and SIGTERM cancel
active work and mark repositories that have not started as unprocessed.

Process group cleanup also runs after a command exits, so background children
cannot keep working after their attempt finishes. Children must remain in the
command's process group. External commands that deliberately detach into a new
process group are outside this cleanup guarantee. SIGKILL of deplens-sweep
bypasses normal cleanup.

A shallow clone reduces Git history. It can still have a large working tree.
Increasing `workers` increases the number of simultaneous working trees and the
scanner processes consuming memory and CPU. Choose the worker count for the
available machine capacity.
