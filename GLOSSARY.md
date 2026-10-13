# Repository inventory collection

deplens-sweep collects dependency inventories from selected repositories. These terms describe the scope and evidence of that collection.

## Language

**Connection**:
A named hosting provider, server, credential reference, and repository selection. Each connection has independent collection history.

**Batch**:
One invocation's repository selection, discovery status, and collection outcomes.
_Avoid_: Schedule, service

**Dependency source**:
A file that describes or uses dependencies, as defined by deplens.
_Avoid_: Manifest as an umbrella term

**Dependency reference**:
One dependency value extracted from a dependency source.

**Repository inventory**:
The dependency sources and references observed at a repository revision within the scanner's supported scope.
_Avoid_: Vulnerability scan

**Attempt**:
An effort to acquire and analyze a selected repository revision. An attempt can complete or fail.

**Complete result**:
A saved inventory whose analysis finished without failed or partial extraction. Unsupported extraction and reported coverage gaps alone do not make a result incomplete.

**Reuse**:
A new check that references a complete result with matching repository identity, revision, scanner, and effective scan configuration.
_Avoid_: Fresh scan

**Check time**:
When the batch checked a repository's current inputs. It is separate from the time the referenced inventory was scanned.

**Unprocessed repository**:
A selected repository whose attempt could not start before the batch ended.
_Avoid_: Failed attempt
