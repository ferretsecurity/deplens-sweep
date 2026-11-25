# Repository inventory collection

deplens-sweep collects dependency inventories from selected repositories. These terms describe the scope and evidence of that collection.

## Language

**Connection**:
A named hosting provider, server, credential reference, and repository selection. Each connection has independent collection history.

**Batch**:
One invocation's repository selection, discovery status, and collection outcomes.
_Avoid_: Schedule, service

**Starter corpus**:
The fixed selection of 500 public GitHub repositories used for the dependency coverage study. At selection time, these were the most-starred eligible projects with a detected OSI-approved license, excluding forks and archived projects.

**Dependency source**:
A file that declares, fetches, or uses a dependency to build, test, package, deploy, or run the project. This includes project-specific scripts.
_Avoid_: Manifest as an umbrella term

**Dependency reference**:
One dependency value in a dependency source, whether or not the scanner extracts it.

**Repository inventory**:
The dependency sources and references observed at a repository revision within the scanner's supported scope.
_Avoid_: Vulnerability scan

**Coverage review**:
A comparison of a complete repository inventory with evidence at the same repository revision. It identifies missed sources, missed references, and limitations the scanner already reported.

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
