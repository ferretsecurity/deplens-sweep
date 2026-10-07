# Inventory history and reuse

Each selected repository is identified by its connection name, provider, server,
and provider repository ID. Its path is descriptive metadata. Renaming a project
keeps its history, and each new batch records the current path.

Before cloning, sweep resolves the default-branch commit and checks retained
attempts for a complete matching result. Reuse requires the same repository
identity, commit, scanner identity, and effective scan inputs. Scanner identity
includes the executable's SHA-256 digest and its Go module version when available.
The scan input fingerprint includes the effective scanner flags, acquisition
scope, and SHA-256 digests of custom rule file contents. Editing a rule file in
place triggers a scan. Moving an identical rule file does not. Rule extensions
keep their configured order. Changing selections or execution limits does not
change the analysis inputs.

A result is eligible only when both `scan.json` and `metadata.json` exist as
regular files, metadata records a complete attempt with matching inputs and valid
timing, and the scanner JSON passes completeness validation. Sweep rejects links
in artifact paths. It retries missing or invalid artifacts, failed scans, partial
extraction, and analysis errors. Unsupported extraction and coverage gaps alone
remain complete. If custom rule files change during analysis, that attempt fails
rather than publishing an inventory under the earlier fingerprint.

Every actual attempt has its own directory under
`connections/<connection>/<identity>/attempts/<attempt-id>/`. Sweep retains its
metadata and any available scanner output. A successful attempt publishes
`latest.json` only after saving both artifacts. A failed attempt keeps the older
latest complete reference. Reuse may select an older matching complete attempt
when inputs return to a previously scanned state; it does not rewrite latest.

Batch repository outcomes use a `result` path relative to the results directory.
A `reused` outcome points to the original attempt and writes no copy of its
inventory. Its `checked_at` is the new check time, while its `scanned_at` stays
equal to the original scan time. Metadata saves `started_at` for the attempt,
`scanned_at` when analysis starts, and `completed_at` when the attempt finishes.
Acquisition failures have no `scanned_at`. Metadata also saves
`input_fingerprint`, `scanner_version`, effective `configuration`, and acquisition
`scope`.

Use `batches/<batch-id>.json` to determine the current batch's inventory. Older
history directories and `latest.json` do not imply current selection or success.
Repositories that disappear or leave the selection keep their histories. A later
selection can reuse a matching retained result. A failed current attempt stays a
failed outcome even if a complete older inventory exists for other inputs.
