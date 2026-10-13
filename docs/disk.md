# Free space for temporary checkouts

The CLI checks free space before starting each clone. Set the reserve in bytes
at the top level of the configuration:

```yaml
min_free_bytes: 10737418240
```

The default is 10 GiB. The value must be a nonnegative integer. An explicit
`min_free_bytes: 0` disables the minimum reserve. It does not suppress errors
when the command cannot inspect the checkout filesystem.

The check uses available blocks for an unprivileged user on the filesystem
receiving temporary checkouts. When `temp_dir` is set, it checks the actual
workspace beneath that directory. Otherwise, workspaces live under
`results_dir/.work`. Free space on a separate results filesystem does not count
as checkout capacity.

When space drops below the reserve, the coordinator stops starting clones.
Active attempts can finish, save their outcomes, and remove their checkouts.
The coordinator rechecks space after each completion. It waits only while
active work remains that might release space. If space is still insufficient
when no attempts are active, it marks the remaining repositories as
`unprocessed` and ends with a nonzero exit status. It still discovers later
connections and can record empty repositories or reuse complete inventories
without a new clone.

A free-space inspection error also prevents clone admission and records an
unprocessed outcome. A storage or checkout cleanup failure stops the batch.
An actual disk failure during an active clone or analysis produces a failed
attempt. Previously saved complete inventories remain available.

The reserve is an admission threshold. It does not reserve blocks or impose a
quota. Active clones, scanners, and other processes can still consume the
remaining space. A shallow clone can have a large working tree. Check the batch
summary's `unprocessed` count and repository errors to identify work that did not
start. Increase available space, lower the reserve if appropriate for the
machine, and run another batch to process those repositories.
