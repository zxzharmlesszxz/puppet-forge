# Release Review and Comparison

[Українська версія](RELEASE_REVIEW.uk.md)

## Access

Open **Browse / Compare** next to a release in Manage Modules or the team's Modules page.
The owning team administrator can review local releases. A global administrator can also review upstream releases.
Readers, publish tokens, and administrators of other teams cannot use the review endpoint.
Every request checks current SQL resource state, including requests made with an old SHA256 link.
Pending releases are available only through the administrative review/download routes; normal install routes remain closed.
Blocked or deleted modules/releases cannot be browsed, compared, or downloaded through review routes.

The route is `GET /manage/modules/{owner}/{name}/review`.
Parameters are `version`, optional `base`, `file`, `mode=unified|split`, and optional snapshot guards `sha256`/`base_sha256`.
Without `base`, the previous readable version in semantic-version order is selected. An explicit empty `base` disables comparison.
Both versions must belong to the same module; the administrator may select other existing versions explicitly.
The default selected file is `metadata.json`.

## Review Flow

1. Inspect the archive tree and filter file paths. Open files without navigating away from the review page.
2. Select two releases for comparison. The manifest includes added, removed, modified, and unchanged files.
3. Review unified or side-by-side changes. Diff hunks contain three context lines and can be collapsed individually.
4. Check the source/target versions and SHA256 fingerprints. File size and permissions are shown separately; permission-only changes remain visible.
5. Approve a Pending release or block it with a required reason. The approval form submits the displayed target SHA256.

A changed target archive causes approval to fail with `409 Conflict`; reopen and inspect the replacement.
Changing the base does not alter the target archive being approved.
There is no requirement to open every file: this is a review aid, not a substitute for CI checks.
Review views and administrative archive downloads do not count as installation usage.
Successful views, denied administrative access, and archive inspection failures are written to the existing audit log with the module/release identity.
Successful views also record both snapshot hashes and the selected path, not source contents.

Global and team audit pages show each moderation decision, its previous and new
state, the required comment, and any replacement reference. The reviewed archive
SHA256 is available under Technical context. Historical moderation comments
remain visible even when recorded using the older audit attribute name.

## Implementation and Limits

Archive inspection and diff computation run in Go. Chroma supplies syntax tokens, and go-udiff computes textual changes.
The UI uses server-rendered templates, local CSS, and a small progressive-navigation script, with no editor runtime, CDN, or Node.js runtime dependency.
Puppet `.pp` and EPP use the Puppet lexer; EPP highlighting is lexical, not template execution or validation.
Other known extensions use Chroma's filename matching. Unknown files fall back to escaped plain text.

Fixed per-request safety limits:

- At most two review operations per application replica, including highlighting; excess requests return `503` rather than queue indefinitely.
- A 15-second request context bounds SQL/storage work and is checked between rendering stages. Library calls are not forcibly preempted; input and output caps bound them.
- Each archive: at most 128 MiB compressed, 64 MiB expanded, and 4096 tar entries, including directories.
- Paths: at most 1024 bytes and 31 separators; total file-path bytes at most 256 KiB per archive.
- Each preview/diff input: at most 64 KiB, 2000 lines, valid UTF-8, and no NUL bytes.
- Highlighted markup: at most 2 MiB per source.

Traversal paths, mixed/wrong archive roots, duplicate paths, unsupported tar entry types, and conflicting file/directory paths are rejected.
Archives are scanned sequentially and never extracted to the server filesystem. Only the selected file's bounded preview is retained.
The compressed archive SHA256 is checked against the stored release fingerprint before its contents are shown.
Binary, non-UTF-8, or oversized files remain in the manifest with an explicit preview-unavailable message.
An archive-wide limit fails the review; omitted content is never reported as unchanged.

Comparison ignores gzip timestamps, tar ordering, and the versioned root directory. It compares relative paths, content hashes, and permission bits.
Renames are currently displayed as removal plus addition. There is no automatic code execution, editing, semantic Puppet validation, or cross-file content search.
The first implementation does not cache archive contents or diff results; every file request performs a bounded scan.
Review responses use `Cache-Control: no-store`; browser state retains only navigation/scroll information, not source text in persistent storage.

## Lifecycle

Existing releases without a state record remain Available. API uploads create Pending releases, excluded from ordinary catalogs and downloads until approval.
Approval of the first release also makes its Pending module Available. Deprecated remains downloadable; Blocked does not.
Unblocking restores the previous state, so unblocking an unapproved release returns it to Pending.
Available and Deprecated releases participate in `latest`; changing state recalculates it transactionally with the audit event.
Administrators retain the emergency **Upload and allow** form with a mandatory reason. It is not the normal CI publishing workflow.
Ordinary deletion checks observed usage within `ACTIVE_RELEASE_TTL`; neither `latest` nor a missing Blocked state is an absolute prohibition.
Force deletion requires a reason and exact identity confirmation and remains audited.

Keep storage buckets private. No review route returns a public or signed storage URL; external IAM and previously downloaded copies cannot be revoked by the viewer.

## Verification

The test suite includes syntax escaping, line-number mapping, Unicode, empty files, EOF newline changes, input/output bounds, archive traversal, binary previews, Pending/Blocked states, stale hashes, administrative scope, and usage isolation.
Browser tests cover file-only navigation, retained filters/wrapping, browser history, and failed fetches.
Run the repository's unit/integration/browser checks described in [DEVELOPMENT.md](DEVELOPMENT.md). Adding tests is not a claim that a local test run has passed.
