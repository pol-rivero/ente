# Ente Drive — backend design & implementation plan

> **Implementation status** (kept up to date by the orchestrator, see
> "Orchestration" below)
>
> - **Current:** full audit of the plan and stages 1–10 done (uncommitted in
>   the working tree; per-group commits on branch `audit-merge-trial`). Waiting
>   for the owner's review and commit. See "Audit (post stage 10)" below.
> - **Next:** none (Phase 3 must be refined into tasks first).
> - **Done (committed):** Stage 1 (44a090abe9), Stage 2 (9fa75916d8), Stage 3
>   (d4661aa40f), Stage 4 (44504ab2dc + review fixes 230e5761d2), Stage 5
>   (e95c6a06e8), Stage 6 (01ecf91cad), Stage 7 (f896e1131b), Stage 8
>   (23eceae9a7), Stage 9 (dca6dcaf8c), Stage 10 (42ce335cff).

Status: implemented through stage 10, audited; Phase 3 not started · Scope: `server/` (museum)
only · Clients are out of scope except where a server contract is defined for
them.

This plan turns museum into a backend for a Google-Drive-like, end-to-end
encrypted file store, without changing behaviour for existing Photos, Locker
and Auth clients. It is written for implementing agents: each task lists the
context, the change, the files involved, and acceptance criteria. Line numbers
are approximate (`~`) — re-locate symbols before editing.

---

## Orchestration

Instructions from the project owner to the orchestrating agent:

> Act as an orchestrator. You spawn agents to implement each part, then
> adversarial review agents (check their findings). Split the plan in 5 to 10
> stages, and implement them one at a time, allowing me to review and manually
> commit each stage before continuing.

> All the changes we make here must never break an existing deployment and
> should never modify the behavior of existing apps (only the new Drive),
> unless there is a clear reason why.

How this works in practice:

1. **Per stage:** one implementation agent → at least two adversarial review
   agents with different focuses → the orchestrator verifies each finding
   against the code → a fix agent applies the confirmed ones → summary to the
   owner. Never commit; the owner reviews and commits each stage manually.
   Wait for the owner's go-ahead before starting the next stage.
2. **Every implementation and review prompt** includes the compatibility
   guideline above and the comments rule (§0 rule 9).
   Reviewers check it explicitly, beyond client-facing response shapes:
   - existing deployments: hosted production and self-hosters; startup with
     old config files (no new required keys or changed defaults); migration
     lock levels and duration on large, populated tables;
   - rolling deploys: old and new binaries running against the migrated DB;
   - rollback to the old binary after Drive rows exist;
   - every existing app and consumer: Photos, Locker, Auth, Space, Paste,
     Cast, public albums/embeds, accounts, admin endpoints, crons, emails,
     error-level logs, latency of hot paths.
   Every behaviour difference for a non-Drive request must be removed or come
   with a stated reason.
3. Agents must not edit this file or `drive-client-guide.md`; the
   orchestrator updates the status block above after each step.

Stages (Phase 3 isn't staged: it must be refined into tasks first; optional
tasks 2.6 and 2.7 are skipped unless the owner asks):

| Stage | Tasks | Migrations | Status |
|---|---|---|---|
| 1 | 0.1 + 0.2 + 0.3 — `drive` app, plumbing, cross-app holes | 149, 150 | Committed (44a090abe9) |
| 2 | 1.1 + 1.2 — Drive max size, single-PUT guard, quota at multipart start, early expiry, V1 restriction | – | Committed (9fa75916d8) |
| 3 | 1.3 — resumable multipart uploads | 151 | Committed (d4661aa40f) |
| 4 | 1.7 — Drive quota reservation + cleanup-cron fixes | – | Committed (44504ab2dc, fixes 230e5761d2) |
| 5 | 1.4 — multipart server-side copy | – | Committed (e95c6a06e8) |
| 6 | 1.5 — async copy jobs | 152 | Committed (01ecf91cad) |
| 7 | 1.6 — resumable streaming replication | 153 | Committed (f896e1131b) |
| 8 | 2.1 + 2.2 — nested-folder schema, model, redaction | 154 (155–158 removed by the audit) | Committed (23eceae9a7) |
| 9 | 2.3 + 2.4 — tree lock, create with parent, move | – | Committed (dca6dcaf8c) |
| 10 | 2.5 — deleting folders (v3 on Drive, v4 recursive) | – | Committed (42ce335cff) |

Stages 1–10 are implemented. Where a task's text and its stage notes
disagree, the stage notes (and the audit section after them) describe what was
built; the task text is the original plan.

Stage 1 notes (deviations and deferred items, for later stages and the PR):
- RestoreFiles' header-app check applies only when Drive is involved (the plan
  asked for all apps; that would change Photos/Locker behaviour).
- `MoveFiles` had an unlisted cross-app hole; closed with the same Drive-only
  check. Cross-app rejections return a new 400 `CROSS_APP_FILE`.
- `apps.public-drive` has no default; Drive links fall back to the Locker
  host and URL format, and the origin check only accepts it when it's set.
- Deferred: a Drive capability bit in `serverApiFlag` (see the client guide
  §11); until then, use the Drive client only with updated servers. Rolling
  back past the Drive release is unsupported once Drive is live.

Stage 2 notes:
- Interim Drive cap: `DriveMaxFileSize` is **10 GiB** until 1.6
  (stage 7) lands. Today's replication downloads whole objects to local disk
  (`replication3.go` ~245) and its s3manager part sizing breaks near
  5 000 GiB, so a 5 000 GiB cap would let a Drive user break replication
  for every app. Stage 7 raises the constant to 5 000 GiB and adds the
  streaming-threshold test.
- Early expiry (1.2) is skipped for public-link Creates: the anonymous
  uploader acts as the owner and mustn't expire the owner's pending uploads.
- `Update` uses the stored `files.app` only when the header or the file is
  Drive; Photos/Locker cross-cases keep the header app (avoids changing
  internal-user limits for Photos↔Locker updates).
- Public upload routes use the collection's app when either the collection
  or the header is Drive; otherwise the header app, as before.
- The Photos/Locker "exceeds max file size" and "too many parts" 400s are
  unchanged (empty `{}` body; log text still prints `MaxFileSize`). Drive
  gets typed messages with the effective limit / minimum part length.
- Early-expiry blast radius beyond what 1.2 lists (both already fail today):
  old V1-URL Photos clients whose >10 GiB object 413s at Create, and
  `/files/copy` of a 10–20 GiB file to a non-internal user. Photos mobile then
  gets a 503 on retry instead of a repeat 413 and re-uploads.

Stage 3 notes:
- Both routes are Drive-only, decided by the stored `temp_objects.app`;
  other rows get 400 before any S3 call. `part_length` is stored for every
  app's V2 multipart rows (only the Drive-gated resume reads it).
- Status codes differ from the 1.3 text:
  - A lock conflict returns 409 `UPLOAD_BUSY` (retry), not 410. The cron only
    locks rows that are already expired, which resume's filter skips (410), so
    a NOWAIT failure on a live row means a concurrent resume, abort or Create.
  - After NoSuchUpload, an object of the wrong size also returns 410 (it can
    never be committed). On either "gone" verdict, the row is expired and
    released so the cron deletes the leftovers. HEAD 404s are retried twice
    (~2 s) first.
  - Released rows (`reservation_released`) are never live for resume/abort.
  - The full table is in the client guide §6.4.
- DELETE expires and releases the row and commits **before** the S3 abort,
  which is best effort (the cron finishes it). Resume keeps one transaction
  under a 60 s timeout: the S3 clients have no HTTP timeout and the DB pool is
  shared with every app.
- 1.2's `ExpireTempObjectNow` and DELETE already set `reservation_released`;
  1.7 doesn't need to add that.
- Tests use an `httptest` fake S3, not MinIO (no MinIO harness in the repo).
  It can't verify that the signed Content-Length of a re-issued last part is
  right; check with a real provider before release.
- Deploy note: migration 151 takes a brief ACCESS EXCLUSIVE lock on
  `temp_objects`. In a rolling deploy it can queue behind an old pod's cleanup
  batch (row locks held across S3 calls), stalling upload-URL issuance and
  Create for all apps until the batch commits (same as migration 143). Don't
  add `lock_timeout`: a failed migration leaves the DB dirty and blocks startup.
- For stage 4 (1.7):
  - The cron's failure path pushes an expired row's expiry forward a day
    without touching `reservation_released`. Naturally expired rows, and rows
    early-expired by old binaries, can re-enter the reservation sum for a day.
  - The "insert with `upload_id = NULL`, then update" ordering makes resume
    and abort return 400 in that window (they require an upload ID); map it to
    409 instead.
  - The restructured insert must keep writing `part_length`.
  - Resumed rows can now live up to 90 days.

Stage 4 notes:
- The owner committed the implementation (44504ab2dc) before review; the
  review fixes are 230e5761d2 (committed by the orchestrator, one-time
  exception).
- Consistent read: one READ COMMITTED statement (usage, member usage, Locker
  usage, reservations), not REPEATABLE READ: that snapshot would be taken at
  the lock statement, before the wait ends.
- The quota lock is only taken to reserve (upload starts). Create/Update
  checks are unlocked single-statement reads, so two unreserved Drive commits
  can still overshoot by one file, as Photos can today.
- Pool safety (review blocker): nothing takes a second pooled connection
  under the lock (bonus is read before, Locker usage is in the statement);
  at most 8 reservation transactions per process (waiting for a slot holds no
  connection); 10 s timeout plus `lock_timeout` → 503 `QUOTA_CHECK_BUSY`.
  Those 503s are logged at Error level by the generic handler.
- Reserving multipart rows are inserted as `is_multipart = FALSE` with
  `part_length` set and `upload_id` NULL ("pending"), and flipped to
  multipart when the upload ID is set. Older binaries' cron then removes them
  like a single PUT (a NULL-ID multipart row made the old cron loop on
  `Abort(UploadId="")`); the new cron also aborts orphan uploads for the key
  via `ListMultipartUploads`. A start whose row was cancelled meanwhile gets
  410.
- Admission is per key (reserved keys stay in the sum; the rest are checked),
  and also applies to Update and UpdateThumbnail. The reserved-key read and
  the sum are two statements: a user aborting their own key while committing
  it can undercount by that object.
- Public-link (collect) uploads into Drive don't reserve the owner's quota
  (anonymous link holders could otherwise lock the owner out for 14 days);
  their rows are inserted with `reservation_released = TRUE` and get the full
  check at Create. Deviation from 1.7's coverage list.
- `DELETE /files/multipart-upload` releases any own live Drive `file_upload`
  row (single PUT, pending or multipart).
- The cron's failure path releases Drive rows. Remaining gap: an **old**
  binary's cron failing on an unreleased Drive row pushes its expiry a day
  without releasing it (over-counts; worst case a spurious 426). Old pods
  during a deploy also don't lock or count reservations.
- Update of a Drive-app file with a Photos/Locker header takes the Drive
  quota path (stored app, rule 2); can only be stricter.
- For 1.4 (copy): reserve the whole batch in one `ReserveDriveUpload` call
  (destination rows `purpose = 'file_upload'`, `app = 'drive'`,
  `content_length` set); for multipart copies use the pending shape and
  `SetTempObjectUploadID` instead of the plan's `UPDATE … SET is_multipart =
  true`; set `File.Size`/`Thumbnail.Size` so admission's
  `content_length >= size` applies; decide what a `DELETE` of a copy
  destination row does to the running copy.

Stage 5 notes:
- Photos/Locker copies always make today's exact `CopyObject` call first.
  Only when it fails for an object above 4 768 MiB that is within the app's
  size limit do they fall back to the multipart copy (breaking-changes §3).
  The plan's premise ("CopyObject fails above 5 GiB") holds for B2 only;
  self-hosted stores copy up to 5 GiB (some beyond), and D12's DC naming
  means the provider can't be inferred.
- Drive-only (destination collection's stored app): per-file size pre-check,
  batch quota check (the reservation itself; breaking-changes §1 resolved as
  "gate to Drive"), sizes populated from `object_keys` (admission), 8 files
  at a time, cancellation on disconnect. Drive destination rows are created
  inside one `ReserveDriveUploads` call, not through `GetUploadURLs`.
- Multipart copies use the pending row shape and `SetTempObjectUploadID`
  (Stage 4), for every app. Upload ID recorded before the first part.
- s3copy: no `CopySourceIfMatch` (B2 doesn't support it; keys are never
  overwritten, so it guarded nothing); HEAD size must equal `object_keys` (400 for Drive on mismatch); one retry layer (SDK retries off
  for part copies, 1/4/16 s backoff, 10 min per attempt); at most 32
  concurrent part copies per process.
- A failed or cancelled Drive batch releases its reservations at once but
  keeps the rows 1 h, so the cron can't race a copy still finishing on the
  provider. Files already created in the batch stay (not atomic).
- `maxParts` is 10 000 when the hot DC is named `b2-eu-cen`, else 1 000. D12
  makes every self-hoster `b2-eu-cen`, so a Scaleway/Wasabi-backed self-host
  would assume 10 000. Owner accepted this (the self-host quickstart already
  uses `b2-eu-cen`); no stage 7 decision needed.
- Cross-app copies involving Drive return 400 `CROSS_APP_FILE` (was an empty
  500) and header mismatches are rejected before copying.
- Not verified on a real provider (fake S3 only, no MinIO harness): B2
  `UploadPartCopy` with a byte range, and B2's 5×10⁹-byte `CopyObject`
  limit.
- Large synchronous Drive copies can still be cut by proxy timeouts (60–100 s),
  which cancels and releases them: that's task 1.5.
- For 1.5: reuse `s3copy.Copy` + `copyOptions()` + the upload-ID recorder,
  `CheckFileSize`, `ReserveDriveUploads`, `ReleaseTempObjects`;
  `IsCopyAllowed` now returns the destination app but still takes a gin
  context; `copyDriveFiles`/`createCopy` and `Create` are request-scoped. A
  crashed Drive copy leaves live reserved rows for 14 days that the client
  can't DELETE (it never sees the keys): the worker must release or resume
  them on restart.

Stage 6 notes:
- Async only for `?async=true` (exactly) with a Drive header; `requestID` is
  a new optional body field (1–64 chars). Everything else takes the
  unchanged sync path. Enqueue runs the sync Drive checks and the batch
  reservation; the job row is inserted inside the reservation transaction.
  A same-`requestID` request returns the existing job before validation
  (also after any reservation error, so a concurrent retry near quota doesn't
  get 426).
- `IsCopyAllowed` and `access.GetCollection` now take `context.Context`;
  `Create` is a gin wrapper over `CreateWithContext`. Client info and request
  ID are read up front instead of from the pooled gin context inside the
  replaced-object goroutine (same values; removes a pre-existing race).
- Worker: lease-based (2 min, 30 s heartbeat bounded by the lease, DB clock),
  `FOR UPDATE SKIP LOCKED` claim, at most 2 jobs per process, one running job
  per user (fairness), 5 s poll plus wake-on-enqueue. Started only where
  `jobs.cron.skip` is false, like the other background jobs: **a deployment
  must have at least one cron pod**, or async jobs stay pending (holding their
  reservations for up to 14 days). Every cron pod now runs one cheap claim
  query every 5 s, Drive or not.
- Resume after crash/restart reuses the destination keys and reservation rows;
  committed items are found through `object_keys`; recorded and listed
  multipart uploads for the key are aborted and the row reset, then the file
  is copied again **from the start** (no part-level resume).
- Retries: INTERNAL-class errors go back to `pending` with backoff
  (1 min doubling, 30 min cap), up to 5 attempts (graceful-shutdown yields
  don't count) → `COPY_ATTEMPTS_EXCEEDED`. Typed errors are permanent.
  Panics are recovered (Drive goroutines only) and fail the job.
- Source checks at run time: access (`PERMISSION_DENIED`), file gone
  (`NOT_FOUND`; `VerifyAllFileIDsExistsInCollection` now returns a sentinel
  with the same message, still a 500 for the sync path), sizes stored at
  enqueue must match (`COPY_SOURCE_CHANGED`, 409), live reservation rows
  (`COPY_RESERVATION_LOST`).
- Drive-only S3 timeouts: 30 min single `CopyObject`, 5 min metadata calls,
  15 min complete; Photos/Locker copy calls unchanged.
- Rate limits: async Drive enqueue 60/min, job polling 200/min per user; sync
  copies stay unlimited. Finished jobs deleted after 7 days. Shutdown waits up
  to 5 s for jobs to yield.
- Deploy/rollback: old pods ignore `async` (sync 200) and 404 the poll route.
  Rolling back needs `152 down`, which drops pending jobs; their reservations
  stay until the rows expire (≤ 14 days, Drive quota only).
- Not fixed (documented in the client guide): a sync Drive copy whose process
  crashes keeps its reservation until expiry; Drive clients should always use
  async.
- Migration renumbering: 1.6 (stage 7) runs before phase 2, so it takes 153
  and phase 2 moves to 154–158. golang-migrate only applies versions above the
  current one, so shipping 158 before 153–157 would skip them.

Stage 7 notes:
- `DriveMaxFileSize` is now 5 000 GiB (1 000 replication parts × 5 GiB).
- Routing: an object streams iff it belongs to a Drive file and is larger
  than `replication.streaming-threshold` (default 1 GiB), or is larger than
  20 GiB (safety net). Photos/Locker objects always take the unchanged legacy
  path (one extra app lookup for objects of 1–20 GiB; a lookup error falls
  back to legacy). Deviation from the plan: legacy spools aren't in the byte
  budget and the legacy disk check is unchanged (`/`, `Bfree`).
- Migration 153 (`replication_uploads`) adds `source_etag`; a stored upload is
  reused only if part size and source ETag (when both non-empty) match. Each
  ranged GET's ETag is also compared (mismatch = transient, never completed).
- Aborts only on: source missing / size mismatch / ETag changed, object
  deleted, destination no longer wanted (re-checked before Complete, with
  `GetObjectState`), insert conflict (own upload only). NoSuchUpload/NoSuchKey
  only deletes the row (by upload ID); the sweeper handles real orphans.
- Liveness: `object_copies.last_attempt` is a compare-and-set lease (5 min
  heartbeat, only when bytes move, SDK hashing pass not counted); a failed
  attempt that moved bytes or failed transiently is re-picked in ~1 h, others
  in 24 h. Per-part download deadline (10 min + 1 s/MiB) plus idle timeout;
  fetch retries ~9.5 min.
- Photos protection: at most `max(1, workerCount/3)` streaming attempts per
  process (`replication.streaming-workers`); when full, the object is
  deferred ~10 min and the worker moves on. Streaming needs `Bavail ≥ spools
  in flight + part + 22 GiB` on `tmp-storage` and fails fast otherwise
  (Error + Discord once), so the legacy path's disk check isn't starved.
  Spool budget `replication.spool-budget` (default 40 GiB).
- Worker download: Range through the Cloudflare worker, accepting only an
  exact 206 (or a whole-object 200); after 3 consecutive failures, direct B2
  GETs (egress cost) with a 1 h re-probe and one Discord notice. Whether the
  deployed worker passes Range through is **unverified**.
- Orphan sweeper (`replication_sweeper.go`): first run 5–30 min after start,
  then every 6 h, one pod per run (task lock). Aborts uploads older than
  `replication.orphan-upload-days` (14) only for streaming-eligible keys with
  no tracked row, plus tracked uploads no longer needed; drops abandoned rows.
  Legacy Photos/Locker orphans are left alone (Wasabi auto-deletes after ~31
  days; Scaleway doesn't).
- Shutdown: replication stops picking new work; in-flight attempts resume.
- Not verified on real providers (fake S3 only): Wasabi object-lock with
  Content-MD5 on `UploadPart`, abort on compliance buckets, Scaleway
  ListParts on GLACIER uploads, `Initiated` in listings, B2 ETag stability for
  large files, 5 GiB part timings.
- Deploy/rollback: old pods replicate any object with the legacy path once
  its `last_attempt` is > 24 h old (huge objects fail the disk check; near
  5 000 GiB s3manager would need > 5 GiB parts). Rolling back past this stage
  leaves stored uploads on Wasabi/Scaleway (no sweeper) and can't replicate
  Drive objects above ~20 GiB; rollback past Drive is unsupported anyway.

Stage 8 notes:
- `POST /collections` binds `ente.Collection`, so the parent fields are
  cleared on create until create-with-parent validation lands (stage 9).
  Side effect: a client sending a non-integer `parentID` now gets 400 instead
  of the key being ignored (no existing client sends it).
- Parent fields are filled only by owner reads (`Get`, `GetCollectionByType`,
  `GetCollectionsOwnedByUserV2`); `GetCollectionsSharedWithUser` doesn't
  select them; `ClearParent()` redacts them in `WithSharingDetailsForUser`,
  `GetCollection` (non-owner roles), public links and cast. Photos/Locker
  responses are checked byte-identical against a golden file recorded on the
  pre-154 code.
- *(Superseded by the audit: migrations 155–158, the parent FK and the
  single-process deploy procedure were removed. See "Audit (post stage 10)".)*
  The DB enforces only the row-level CHECKs from 154; the parent's existence,
  owner, app, type and liveness, and key format, are enforced by the 2.3/2.4
  controller checks under the tree lock.

Stage 9 notes:
- **Ship together with stage 10.** v3 deletes don't take the tree lock or
  check children yet, so a v3 delete of a Drive parent (or a create racing it)
  leaves a live child under a deleted parent. Such orphans also escape the
  move depth check (height skips deleted rows), so the 64 cap could be
  exceeded. If stage 9 is ever deployed alone, stage 10 must repair or reject
  those rows.
- Tree lock: `pg_advisory_xact_lock(hashtextextended('ctree:<owner>'))`,
  shared helper `lockAdvisoryXact` with the quota lock (key string unchanged,
  `quota:<id>`). Per-owner cap of 2 in-flight tree changes, then 8 per-process
  slots, 5 s total budget → 503 `COLLECTION_TREE_BUSY` (Error-level log, like
  `QUOTA_CHECK_BUSY`). Everything under the lock runs on the tx connection.
- For stage 10: the 5 s budget bounds the whole transaction, so a v4
  recursive delete needs its own longer budget; `TrashV3`/`ScheduleDelete`
  use their own connections (link/cast revocation, `repo.DB` tx) and must be
  moved onto the tree-lock tx, or the pool test will catch the deadlock.
  Account deletion's `ScheduleDelete` calls don't take the lock (user can't
  act then). No code un-deletes collections.
- Create with parent is Drive-only; Photos/Locker still ignore the fields.
  Parent errors are the same `INVALID_PARENT` for missing / not yours /
  deleted / wrong type (no existence leak). Key fields: format check only.
- Move: `newParentID` required (`null` = root, via `ente.NullableInt64`).
  Moved collection missing / not yours → 404 `NOT_FOUND`; own deleted → 404
  `COLLECTION_DELETED`; not a Drive folder → 400 `INVALID_COLLECTION`. Empty
  200; only the moved row's `updation_time` is bumped. Rate limit 500/min.

Stage 10 notes:
- Drive clients use only `DELETE /collections/v4/:id?keepFiles=&recursive=`
  (both required). v3 on a Drive collection runs the same code as v4 with
  `recursive=false`. v4 decides everything inside the tree tx: missing or not
  yours → 404 `NOT_FOUND`; not Drive / `uncategorized` / `favorites` → 400
  `INVALID_COLLECTION`; already deleted → 200; bad params → 400
  `BAD_REQUEST`. Precedence `SUBTREE_TOO_LARGE` (400) > `COLLECTION_NOT_EMPTY`
  (409) > `HAS_CHILDREN` (409). Rate limit 500/min. A Postgres
  deadlock in any tree tx → 503 `COLLECTION_TREE_BUSY` (a Drive file move
  between two folders of a tree being deleted can trigger one; the move side
  stays a 500).
- Recursive deletes use their own pool (2 per process, plus the per-owner
  cap of 2) and a 30 s budget (`collections.recursive-delete-timeout-seconds`);
  the subtree cap is `collections.max-recursive-delete` (10 000). With
  `keepFiles=true`, the subtree's rows are locked (`FOR UPDATE`, by id) before
  the live-files check, so a concurrent upload is either seen or rejected.
- **Commits into deleted Drive folders:** `FileRepo.Create`, `AddFiles`
  and `MoveFiles` bump a Drive target with `AND is_deleted = FALSE` and
  return 404 `COLLECTION_DELETED` on 0 rows; the row lock orders them against
  the delete. Covers public collect, sync and async copy (all go through
  `Create`). `RestoreFiles` was already guarded; `CreateMetaFile` is
  Locker-only. Photos/Locker SQL unchanged (same race listed in §6).
- Queue and worker: Drive deletes (v3, v4, account deletion) go to
  `trashCollectionDrive`, drained every minute by
  `CleanupTrashedDriveCollections` (run lock `CollectionTrashDrive`, budget
  `jobs.drive-collection-trash.budget-seconds`, default 50 s). The worker picks
  the Drive path by stored app. *(The per-owner `:drive` lock, the 5 min
  per-item timeout and Drive processing in the V3 cron were replaced by the
  audit: see "Audit (post stage 10)".)* A lost run lock stops the run. `removeAllFilesAddedByOthers` (shared with
  `TrashV3`) is still unbounded.
- Worker per item: a live collection is dropped with an Error log, a missing
  row (Drive queue only) with a Warn. Live children of a deleted folder (left
  by old pods) are moved to the root under the tree lock (Warn). Subtree and
  children checks walk live nodes only; the ancestor walk (depth/cycle) still
  crosses deleted ancestors. Owner files also in another live owned folder are
  unlinked, the rest trashed, in batches with per-batch commits (idempotent on
  re-run and crash).
- Non-Drive paths unchanged: `TrashV3`, the V3 cron (`trashNonDriveCollection`
  is the old body), lock names and leases, logs. `ScheduleDelete(id)` now
  delegates to `ScheduleDeletesTx` (`= ANY($n)` with one id: same statements,
  order, plan and queue). Every deployment now runs the idle Drive cron (3
  indexed queries a minute, no logs).
- Rollback and orphan repair SQL: `drive-plan-breaking-changes.md`
  ("Deploying and rolling back the Drive release").
- Not covered by tests: the in-flight tx rollback on a lost run lock, and
  Create/Move racing an uncommitted delete (only `AddFiles` is tested in that
  window).

Audit (post stage 10). Seven adversarial reviews of the plan and the whole
range (uploads/quota, copy/replication, folder tree, existing-app
compatibility, security, simplification, docs), then three fix groups. These
notes supersede earlier stage notes where they disagree.
- **Migrations are 149–154.** 155–158 (composite FK, its unique index, the
  `parent_id` index, VALIDATE) are deleted; 154 is one `ALTER TABLE`. No
  concurrent index builds remain, so the single-process deploy procedure is
  gone (a concurrent build deadlocked with golang-migrate's blocking lock, and
  other pods crash-looped after its 15 s lock wait). 155 had also switched
  Photos/Locker PK lookups to the new index. Tree queries (subtree, height,
  children, re-root, ancestors) are owner-scoped; only `CreateTx` (create with
  parent), `SetParentTx` (move) and `ReRootLiveChildrenTx` write `parent_id`,
  all under the tree lock; `repo.Create` clears parent fields. Migration 152
  lost the `file_copy_jobs.app` column; its index is now
  `file_copy_jobs_unfinished_user_idx`.
- **Uploads/quota:**
  - Resume and abort hold no transaction during S3 calls: lock NOWAIT, read,
    commit; ListParts/HEAD/signing outside; then conditional updates (a row
    committed, aborted or expired meanwhile → 410). Previously a burst of
    resumes during a provider slowdown could exhaust the shared DB pool.
  - Resume part URLs are valid for `min(7 d, expiry − 1 h)` (at least
    `min(5 min, remaining)`), so the cron can't abort an upload whose URLs
    still work.
  - New `GET /files/uploads` lists the caller's live Drive reservations
    (client guide §6.3), for reservations whose start response was lost.
    A reservation whose commit errored after landing is expired again. The
    Drive `CreateMultipartUpload` call has a 60 s timeout. Copy paths aren't
    covered by the lost-commit release.
  - Reserved commits are re-checked against committed usage (see 1.7
    "Admission"). A retried Drive `POST /files` whose object is already a live
    object of the user returns the existing file instead of 426.
  - Quota lock: per-subscription cap of 2 in front of the 8 process slots;
    `lock_timeout` = remaining deadline − 500 ms, so the DB times out first.
- **Copy/replication:**
  - At most 10 unfinished async copy jobs per user (429
    `TOO_MANY_COPY_JOBS`, counted under the quota lock); claims serialised by
    a global advisory xact lock (one running job per user now holds across
    pods).
  - On shutdown, in-flight streaming replications hand their object back
    (pickable after ~2 min instead of ~24 h); `StopReplication` waits up to
    10 s.
  - A failed abort keeps its `replication_uploads` row so the sweeper retries
    it (an untracked upload of a deleted object leaked forever on Scaleway);
    a stored upload that can't be aborted blocks only that destination.
  - s3copy no longer escapes keys (all keys are `<userID>/<uuid>`).
- **Drive folder trash:**
  - The per-owner `:drive` lock is gone: Drive trashing runs only under the
    cluster-wide `CollectionTrashDrive` run lock. The V3 cron moves Drive
    items to the Drive queue instead of processing them; a non-Drive item in
    the Drive queue goes back to V3 with an Error log.
  - Fairness: each item gets a 20 s slice (bounded by the run budget); an
    unfinished item yields between batches (Info) and moves to the back of
    the queue. Each batch has a 2 min hard timeout (Error). Before, one huge
    delete held every run and blocked all other users' folder deletes.
  - The run-lock heartbeat stops the run one beat before the lease could
    expire when extensions keep failing (single failures are Warn).
  - Photos/Locker `POST /trash/empty` no longer drains Drive empty-trash items
    inline (pre-Drive behaviour restored); account deletion enqueues the
    Drive empty-trash item only for users with Drive collections.
- **Compatibility fix:** `POST /collections` decodes the parent fields
  leniently again; malformed values are ignored for Photos/Locker (as before
  stage 8) and rejected with 400 only for Drive.
- **Simplifications:** shared advisory-lock tx helper (quota and tree locks),
  shared transient-retry helper (part copies and replication part uploads),
  duplicated constants and helpers merged, `fakeMultipartS3` replaced by
  `internal/testutil/fakes3`, shared test helpers in `internal/testutil`,
  DB test container runs with fsync off (~35× faster suite).
- **Verification round** (two reviews of the merged fixes, then fixes):
  - Copy reservations are marked (`temp_objects.is_copy`, migration 151):
    `GET /files/uploads` doesn't list them and resume/abort answer 404 for
    them (a client cleaning up "lost" reservations broke running copies).
  - The Drive trash worker moves any failed item (batch timeout or error) to
    the back of the queue and skips items that already failed in the run; a
    stuck item no longer blocks every other owner.
  - Resume re-checks the row after signing (410 if released meanwhile); its
    conditional updates use NOWAIT (409 `UPLOAD_BUSY`) instead of waiting on
    the cron. A resume with less than 1 h 5 min left and no progress (or
    capped by the 90-day limit) returns 410 and releases the row.
  - Abandoned replication rows whose abort keeps failing go to the back
    (`replication_uploads.abort_failed_at`, migration 153); one Warn per sweep.
  - Migrations 150, 151 and 154 retry their lock with a 200 ms `lock_timeout`
    (unbounded loop), so a long reader or autovacuum no longer stalls every
    query on `files`/`temp_objects`/`collections`.
  - Drive empty-trash items use the lock `EmptyTrash:drive:<item>`.
  - `test-with-postgres.sh` removes the container's volume (`rm -f -v`); it
    had leaked one anonymous volume per run.
- **Dev/staging databases that ran the pre-audit migrations** must be
  reset before switching: at version 155–158 the new binary refuses to start,
  and migrations 151, 152 and 153 changed in place. Migrate down to 150 with
  the old files (or recreate the database), then start the new binary.
- **Accepted, not changed:** see §6 (extra query on add/move/restore-files,
  resume vs Create size rule, `keepFiles=true` scan cost). Phase 3 risks the
  audit found are listed under Phase 3.

---

## 0. Ground rules for implementing agents

1. **Never break existing clients.** Photos, Locker and Auth clients in the wild
   cannot be updated. Every change must be additive, or gated to
   `app == drive`. Existing endpoints keep their request/response shapes and
   error codes for non-Drive apps; new JSON fields are optional and use
   `omitempty`. When a fix would also be correct for Photos (e.g. a quota
   check), gate it to Drive anyway and list the Photos follow-up in §6.
2. **Gate Drive-only behaviour on `ente.Drive`**, not on collection type
   (`folder` is already used by Photos device albums and by all Locker
   collections). When the decision concerns an existing file or collection,
   use the **stored** app of that row (`collections.app`, `files.app`), not the
   request header.
3. **Migrations**: golang-migrate v4 (`go.mod`), `migrations/N_name.{up,down}.sql`,
   run at startup (`cmd/museum/main.go` ~1212). Each file runs as a single
   implicit transaction. Latest on `main` is 148; numbers 149–154 are used
   below. If `main` has moved on when you rebase, renumber while preserving the
   relative order. Conventions:
   - `ALTER TYPE ... ADD VALUE` alone in its file; anything that references
     the new value (CHECKs, defaults, casts) goes in a **later** file.
   - `CREATE INDEX CONCURRENTLY` alone in its file.
   - Constraints are added `NOT VALID` in one file and `VALIDATE`d in a
     **separate later** file (precedent: 137/138), so the validation scan
     doesn't run while holding the lock taken by `ADD CONSTRAINT`.
   - Always write a `.down.sql`.
4. **Nullable columns**: new nullable columns are scanned with
   `sql.NullString`/`sql.NullInt64` (or pointer types) and written with
   `NULLIF($n, '')` / explicit `nil`. Never write `''` where the schema
   expects NULL.
5. **Tests**: unit tests with `go test ./...`; DB-backed tests with
   `./scripts/test-with-postgres.sh docker` (or `host`). New repo logic needs
   DB-backed tests in `pkg/repo/*_test.go` / `pkg/controller/**/_test.go`.
   Every task touching shared code paths must include a test proving the
   Photos/Locker behaviour is unchanged. Run `go build ./... && go vet ./...`.
6. **Errors**: use existing `ente.Err*` values / `ente.NewBadRequestWithMessage`
   and `stacktrace.Propagate`; add new typed errors to `ente/errors.go`.
7. **New routes** get a rate-limit entry in `pkg/middleware/rate_limit.go`
   (follow the upload-URL routes, ~217-222) and use query params, not bodies,
   on `DELETE` routes.
8. One task ≈ one PR. Don't bundle unrelated refactors.
9. **Comments**: keep them to a minimum. Prefer self-documenting code (clear
   names, small helpers) and add a comment only when it's strictly needed: a
   non-obvious *why* (a compatibility constraint, an interim value, a trap
   for the next editor). No doc comments that restate the signature, no
   per-case narration in tests.

---

## 1. Decisions (defaults — implement these unless told otherwise)

| # | Decision | Default |
|---|---|---|
| D1 | App identity | New `ente.Drive = "drive"`; clients send `X-Client-Package: io.ente.drive*` |
| D2 | Quota | **Shared** with the user's Ente subscription (same path as Photos). No Drive-specific caps or stored Drive file count. |
| D3 | Max file size | Constants (not configurable for now); measured on the **encrypted** object. Photos/Locker keep 10 GiB (20 GiB internal Photos users). Drive cap **5 000 GiB** (= 1 000 parts × 5 GiB, the replication ceiling). Provider docs state these limits ambiguously ("5 GB" parts, "5 TB" objects), but a real-world test by the team (2026-10) confirmed 5 000 GiB objects work on B2, Wasabi and Scaleway. Don't exceed 1 000 × 5 GiB: Wasabi/Scaleway replication is capped at 1 000 parts and parts at 5 GiB. Public-link ("collect") uploads into Drive folders use a separate, lower cap (10 GiB). |
| D4 | Folder hierarchy | Server-side `collections.parent_id`; same owner + same app enforced by FK; nesting only for `app = drive` and type `folder` (enforced by CHECK + controller). Max depth 64. |
| D5 | Root | Implicit. Top-level folders have `parent_id = NULL`. Files at the root live in the Drive app's `uncategorized` collection (existing per-(owner, app) singleton; can't be deleted, can only be shared as VIEWER — the root is never shared as a whole). |
| D6 | Who creates subfolders | v1: only the owner of the parent. Collaborator-created subfolders are deferred to phase 3. |
| D7 | Folder keys | Every Drive folder keeps `encryptedKey` (wrapped with owner master key, unchanged). Every **child** folder additionally stores `parentEncryptedKey`/`parentKeyNonce` (folder key secretbox'd with the parent folder's key). Captured from day one so inherited sharing (phase 3) needs no server backfill. The server only checks their format, so clients must verify the wraps and repair bad ones with a move to the same parent (client guide §5.1). |
| D8 | File placement | Client convention: an owner's Drive file is in exactly one *owned* folder (use move, never add). Not server-enforced. Collaborator contributions are the exception: the file stays in the uploader's own folder and is additionally linked into the shared folder (see §5). |
| D9 | Sharing v1 | Per folder only: sharing a folder shares the files directly inside it, not subfolders. Non-owners never see `parentID`/`parentEncryptedKey` in phase 2 (every shared folder appears as a root to sharees). Inherited sharing is phase 3. |
| D10 | Versioning | None. `PUT /files/update` keeps replacing content. |
| D11 | Thumbnails | Unchanged server requirement. Drive clients upload an encrypted placeholder thumbnail and set `noThumb: true` in pubMagicMetadata (Locker precedent). |
| D12 | Self-hosting | Hot DC must be named `b2-eu-cen` for cleanup/replication (already documented). No change. |
| D13 | Collection sync | Drive clients use `GET /collections/v2` (unpaged, used by all current clients). `/collections/v3` is unused by any client; fixing it is optional (task 2.6). |

---

## 2. Dependency graph

```
0.1 ─► 0.2 ─► 0.3                                   (Phase 0, serial)
            │
            ├─► 1.1 ─► 1.2 ─► 1.3 ─► 1.7             (serial: all edit GetMultipartUploadURLWithMetadata / temp_objects)
            ├─► 1.4 ─► 1.5                           (serial: both rewrite file_copy.go)
            │   1.6                                  (independent; can start any time; migration 153)
            │
            └─► 2.1 ─► 2.2 ─► 2.3 ─► 2.4 ─► 2.5      (Phase 2, serial)
                       2.6, 2.7                      (independent, optional)
                                   └──────────────► Phase 3
```

The three Phase 1 chains (1.1→1.7, 1.4→1.5, 1.6) and Phase 2 can proceed in
parallel once Phase 0 is merged. 1.1 needs `ente.Drive` from 0.2.

---

## Phase 0 — Dedicated `drive` app (≈2 days)

Why not register as `photos`: Photos clients would show Drive folders (even
"hidden" ones appear in the Hidden section), mobile merges all "default hidden"
collections into one and trashes the rest, desktop ML downloads every synced
file, desktop export exports them, and web uploads into same-named
collections. Isolation today rests entirely on the `app` filter in
collection/trash queries.

### Task 0.1 — Migrations (149, 150)

- `149_add_drive_app.up.sql`: `ALTER TYPE app ADD VALUE IF NOT EXISTS 'drive';`
  (enum from migration 53, extended in 68; used by `tokens`, `otts`,
  `collections`, `files`). Down: no-op with a comment (enum values can't be
  dropped safely).
- `150_files_app_supported_drive.up.sql`: drop and re-add the
  `files_app_supported` CHECK from migration 145 as
  `CHECK (app IN ('photos', 'locker', 'drive')) NOT VALID` (it is still
  NOT VALID today). Down restores the old constraint.
- `temp_objects.app` and `events.app` are TEXT — no change.

**Accept**: migrations apply on a fresh DB and on a DB at 148; downs apply;
inserting a `files` row with `app='drive'` succeeds.

### Task 0.2 — App plumbing

Required (missing any of the **bold** ones silently does the wrong thing):

| Where | Change |
|---|---|
| `ente/app.go` | Add `Drive App = "drive"`; include in `IsValid` (also gates JWT session apps, `pkg/middleware/auth.go` ~77, and admin OTTs) and `IsValidForCollection`. |
| `pkg/utils/auth/auth.go` `GetApp` (~78) | Map prefix `io.ente.drive` → `ente.Drive`. (Unknown packages fall back to Photos.) |
| `pkg/repo/usage_file_count.go` `fileCountDelta` (~80) | Return `(0, 0, true)` for Drive (D2). Today `ok=false` → `ErrInvalidApp` on file create (`pkg/repo/file.go` ~787) and restore (`pkg/repo/collection.go` ~901). |
| `pkg/repo/usage_file_count.go` `activeOwnedFileCountDeltas` (~91) | Don't flag `app = 'drive'` rows as ambiguous (it would wipe stored counts on every trash); exclude them from both counts. |
| `pkg/repo/usage_file_count_init.go` (~48-69) | Allow `'drive'` so Drive users stay eligible for count initialisation. |
| **`pkg/repo/queue.go`** | Add `TrashEmptyDriveQueue` constant **and** its entry in `itemDeletionDelayInMinMap` (~19-29, `-1 * 24 * 60` like the others). Without the map entry `GetItemsReadyForDeletion` errors with "missing delay" (~126) and Drive trash is never emptied. |
| **`pkg/repo/trash.go` `EmptyTrash` (~472)** | Route Drive to `TrashEmptyDriveQueue`. **Today every non-Locker request goes to the Photos queue → emptying Drive trash would empty Photos trash.** |
| **`pkg/controller/trash.go` `ProcessEmptyTrashRequests` (~120)** | Add a third loop for `TrashEmptyDriveQueue` with `ente.Drive`. |
| **`pkg/controller/data_cleanup/controller.go` (~143, ~187)** | Also `EmptyTrash(..., ente.Drive)` during account deletion. |
| **`pkg/controller/user/user.go` (~309)** | Add `ente.Drive` to `RemoveTokensForApps` (otherwise Drive sessions survive account deletion / access reset — security issue). |
| `pkg/controller/user/userauth.go` (~420, ~509, ~594) | Include Drive in `shouldEnforceStorageWarningDeletionLoginBlock`; display name "Ente Drive"; OTT email template text. |
| `pkg/repo/public/collection_link.go` `GetAlbumUrl` (~38), `pkg/repo/public/file_link.go` (~44) | Drive link host from new config `apps.public-drive` (`viper.SetDefault` in `cmd/museum/main.go` ~110; entry in `configurations/local.yaml` next to `public-locker`). |
| `pkg/controller/user/user_details.go` (~61, ~168) | Treat Drive like Photos when computing displayed usage (total minus Locker). |
| `pkg/controller/user/account_deletion_summary.go` + `ente/details` | Add Drive file count to the summary. |
| `pkg/controller/file_meta.go` (~23) | Leave Locker-only. |
| Tests listing apps in `pkg/repo/*_test.go` | Extend. |

Needs no change (verified): CORS (`main.go` ~1439), per-app tokens/OTTs,
favorites/uncategorized uniqueness per (owner, app), `usercache/count.go`
(live-count fallback), `usage.go` (Photos quota path per D2), first-upload
email (Photos-only).

**Accept** (DB-backed tests):
- A request with `X-Client-Package: io.ente.drive` creates a `drive`
  collection; it is absent from `/collections/v2` and `/trash/v2/diff` for the
  same user with Photos/Locker headers, and vice versa.
- File upload + create into a Drive collection succeeds; Photos/Locker stored
  file counts are unchanged; trashing a Drive file doesn't invalidate them.
- Emptying Drive trash leaves Photos trash untouched (and vice versa); the
  Drive queue is actually drained by the cron.
- Account deletion empties Drive trash and revokes Drive tokens.

### Task 0.3 — Close cross-app holes for Drive

Pre-existing gaps that let memberships cross apps (they don't matter much
between Photos and Locker today, but Drive isolation relies on them):
- `AddFiles` (`pkg/controller/collections/file_action.go` ~25) compares only
  the header app with the target collection's app. When either the target
  collection or any file has `app = drive` (`files.app`, migration 145; may be
  NULL for legacy rows → treat NULL as `photos`), require all to match.
- `RestoreFiles` (~55-88, repo `pkg/repo/collection.go` ~885) doesn't check
  app at all: require the target collection's app to match the request app,
  and for Drive targets, the files' app.

- Not a hole, but worth knowing: per-collection endpoints such as the file
  diff (`pkg/controller/collections/files_diff.go` ~14) and
  `GET /collections/file` check access, not app. A user's Drive token can
  read that same user's Photos collections by ID. This isn't a privacy issue
  (it's the same user), but app isolation in sync relies on the listing
  queries filtering by app. Leave it as is; don't build Drive features that
  assume the server rejects another app's collection IDs.

**Accept**: tests that a Photos client can't add/restore Drive files into
Photos collections and vice versa; existing Photos↔Photos flows unchanged.

---

## Phase 1 — Large files, up to 5 000 GiB (≈3.5–4.5 weeks server)

Ceilings that bound D3: replication uploads to Wasabi and Scaleway are capped
at 1 000 parts in code (`pkg/controller/replication3.go` ~136-138, ~152,
~163), and parts at 5 GiB, so 1 000 × 5 GiB = 5 000 GiB is the maximum. Provider
docs write "5 GB"/"5 TB" without defining the units; a real-world test by the
team (2026-10) confirmed 5 000 GiB objects with 5 GiB parts on B2, Wasabi and
Scaleway. One known decimal limit remains: B2's server-side `CopyObject`
rejected 5 GiB sources in 2020 (5 × 10⁹ bytes max), see 1.4.

### Task 1.1 — Drive max file size + single-PUT guard

- **Caps are constants** (not configurable for now). Leave
  `MaxFileSize` (10 GiB) and `InternalUserMaxFileSize` (20 GiB) in
  `pkg/controller/file.go` (~64-66) untouched, and add:
  ```go
  // Measured on the encrypted file object.
  const DriveMaxFileSize = int64(5000) << 30       // 5 000 GiB = 1 000 parts × 5 GiB
  const DrivePublicMaxFileSize = int64(10) << 30   // public-link (collect) uploads into Drive folders
  ```
- **Unit tests:**
  - `DriveMaxFileSize ≤ 1 000 × ente.MaxMultipartPartSize` (the replication
    ceiling);
  - `DrivePublicMaxFileSize ≤ DriveMaxFileSize`;
  - from 1.6 on, the streaming-replication threshold is below
    `DriveMaxFileSize`. The legacy s3manager path's `total/1000 + 1` part
    sizing would produce 5 GiB + 1 byte parts at exactly 1 000 × 5 GiB.
- `isFileSizeAllowed` (~75-93) becomes app-aware and returns the effective
  limit; error messages at the V2 call sites (~375, ~1168) print it (today they
  always print `MaxFileSize`).
- **App source**: in `Create` use the target collection's app (already
  validated equal to the header app). In `Update` (~248-283) use the existing
  file's stored app (`files.app`; NULL → `photos`), never the header — today a
  Drive-header client could replace a Photos file with an oversized object.
  Public-collection upload routes (`pkg/api/public_collection.go` ~132-170)
  also pass the header app: derive it from the public collection instead, and
  use `DrivePublicMaxFileSize` for Drive collections. Otherwise an
  anonymous uploader could reserve 5 000 GiB of the owner's quota.
- The cap applies to the encrypted object (Create HEADs only the file object;
  the thumbnail isn't counted). Clients must account for the secretstream
  overhead (§5).
- **Single-PUT guard** (Drive only, per rule 1): `GetUploadURLWithMetadata`
  (~366) rejects `contentLength > ente.MaxMultipartPartSize` (5 GiB) with a
  400 telling the client to use multipart. Providers reject such PUTs anyway;
  this just fails early instead of after the upload. Leave Photos/Locker
  unchanged (their S3-level failure for >5 GiB single PUTs is pre-existing).
- **Part-size guidance**: in the V2 multipart path (~1173-1178), when
  `partCount > 10 000`, the 400 message includes the minimum part length
  `ceil(contentLength / 10 000)`.
- `ente/filedata/preview_upload.go` keeps its own 10 GiB cap (previews only).

**Accept**: unit tests for each app / internal-user combination; a Drive V2
multipart request for 1 000 GiB (with ≥ 1 000 GiB quota available, see 1.2)
is accepted and a Photos one rejected; a public-link upload to a Drive folder
is held to the public cap; a Drive
`Update` of a Photos-app file is held to the Photos limit; a 6 GiB single-PUT
URL request is rejected for Drive and behaves as today for Photos.

### Task 1.2 — Quota at multipart start (Drive) + early cleanup of oversized uploads

- `pkg/controller/file.go` V2 multipart (~1195): for `app == drive`, pass
  `&req.ContentLength` instead of `nil` to `UsageCtrl.CanUploadFile` (matches
  the single-PUT path ~381). **Photos keeps `nil`**: Photos mobile doesn't map
  a 426 from the multipart-URL call to its storage-full UI and would delete
  its multipart track (`mobile/apps/photos/lib/module/upload/service/multipart.dart`
  ~113, `file_uploader.dart` ~813-822). See §6.
- On `ErrFileTooLarge` in `Create`/`Update` (any app; the object can never be
  committed), set the temp object's `expiration_time` to now (new repo method
  `ObjectCleanupRepo.ExpireTempObjectNow`). The cleanup cron (every ~5-6 min)
  then aborts and deletes it instead of waiting 14 days. Accepted exception to
  rule 1: if a Photos user's internal-user flag is enabled after the 413, a
  retried Create now fails and the client re-uploads the file, where today it
  would succeed with the kept object.
- **Never** expire early on `ErrStorageLimitExceeded`, for any app:
  - Photos mobile retries a quota-rejected Create with the same object key
    after the user upgrades (`file_uploader.dart` ~787-797,
    `multipart.dart` ~200-225).
  - For Drive, early expiry would delete a finished multi-TB upload.
  - Instead, Drive uploads holding a reservation (1.7) skip the quota re-check
    at Create, because they were admitted at start. Unreserved ones keep the
    normal 14-day window so the client can retry Create after an upgrade.
- Reject V1 upload-URL routes (`GET /files/upload-urls`,
  `GET /files/multipart-upload-urls`) for Drive by extending the existing
  `RestrictLegacyUploads` middleware (`pkg/api/file.go` ~206-216,
  `cmd/museum/main.go` ~611). Do **not** gate inside `GetUploadURLs`:
  `/files/copy` calls it internally (`file_copy.go` ~97).

**Accept**: a Drive user with 1 GiB left gets 426 when requesting multipart
URLs for 5 GiB; a Photos user's behaviour is unchanged; an oversized Create
leaves its temp object with `expiration_time <= now`; a quota-rejected Create
(any app) keeps its temp object.

### Task 1.3 — Resumable multipart uploads (migration 151)

Today all part URLs are issued up front (7-day validity), the upload ID is only
embedded in URLs, the server can't list uploaded parts or re-sign URLs, and the
temp object expires 14 days after creation regardless of activity.

- Migration `151_temp_objects_resume.up.sql`: add to `temp_objects`
  - nullable `part_length BIGINT`, populated in the V2 multipart path;
  - nullable `resume_parts_completed INT`;
  - `reservation_released BOOLEAN NOT NULL DEFAULT FALSE`, used by 1.7.
- `POST /files/multipart-upload-url/resume` `{objectKey}` returns either
  `{completedParts: [{partNumber, eTag, size}], partURLs: {"<n>": url}, completeURL}`
  or `{completed: true}`.
  - Run everything in one transaction, starting with
    `SELECT … FOR UPDATE NOWAIT WHERE object_key = $1 AND expiration_time > now`.
    The cleanup cron holds `FOR UPDATE` on expired rows during slow S3 calls
    (`pkg/repo/object_cleanup.go` ~91-96), so a lock failure or an expired row
    returns 410 rather than racing or hanging behind the cron.
  - Validate the row:
    - `user_id = caller` (rows predating migration 143 have NULL `user_id`;
      reject them);
    - `is_multipart`, purpose `file_upload`;
    - non-NULL `content_length` and `part_length`, which rejects V1 rows and
      rows created before 151.
  - Call `ListParts` on the row's bucket, paginating with `PartNumberMarker`
    (≤ 1 000 parts per page).
  - **On `NoSuchUpload`:** HEAD the object.
    - If it exists with size = `content_length`, the client completed the
      upload but lost the response. Return `{completed: true}` and extend the
      expiry to at least now + 7 days (within the cap); the client proceeds to
      Create.
    - Return 410 only when the object is absent.
  - Re-sign URLs only for missing parts, plus a fresh complete URL. Sign
    `Content-Length` but not MD5: part MD5s aren't stored, even when the
    original request had them.
  - **Expiry on progress:** only when the completed-part count is greater than
    `coalesce(resume_parts_completed, 0)`. Then store the count and set
    `expiration_time = now + 2 × URL validity`, capped at
    `created_at + 90 days` (5 000 GiB at 20 Mbit/s ≈ 25 days). Without
    progress, don't extend.
- `DELETE /files/multipart-upload?objectKey=`: same ownership checks; call
  `AbortMultipartUpload`, then set `expiration_time = now` — **do not delete
  the row**. If the upload was already completed, abort returns NoSuchUpload
  (treated as success, `pkg/controller/object_cleanup.go` ~262-276) and the
  row is the only record of a full-size object; the cron's existence check
  (~133-140) then deletes it safely, or skips it if it was committed.
- Rate-limit both routes like the existing upload-URL routes.
- Public-collection variants: not needed for Drive v1.

**Accept**: MinIO integration test: start a 3-part upload, upload part 1,
resume → 1 completed part + URLs for 2-3 and an extended expiry; a resume
with no progress (including the first) doesn't extend; complete without
telling the server, then resume → `{completed: true}`, Create succeeds; delete
after complete leaves the object deletable by the cron and doesn't touch
committed objects; another user's objectKey → 404; resume on a row the cron
holds → 410 without blocking.

### Task 1.4 — Multipart server-side copy (`POST /files/copy` > 5 GiB)

`/files/copy` = "save a copy of files shared with me" (`IsCopyAllowed`,
`pkg/controller/collections/file_action.go` ~270-307 rejects own files). It
uses one `CopyObject` per object (`pkg/controller/file_copy/file_copy.go`
~191-205), which providers reject above 5 GiB; failures return an empty 500
and leave partial state.

- New helper `pkg/utils/s3copy/copy.go`:
  `Copy(ctx, client, bucket, srcKey, dstKey string, size int64) error`
  - `size <= 4 768 MiB` → `CopyObjectWithContext`. Not 5 GiB: B2's
    `CopyObject` was confirmed in 2020 to reject sources above 5 × 10⁹ bytes
    ("Copy source too big: 5368709120"; rclone still uses 4 768 MiB). The
    team's upload test doesn't cover copy, and multipart copy costs nothing
    extra here.
  - Else `CreateMultipartUpload` → `UploadPartCopy` with `CopySourceRange`.
    Use an equal part size `max(256 MiB, ceil(size / maxParts))`, rounded up
    to 1 MiB, where `maxParts` is per data center: 10 000 for the B2 hot
    bucket (512 MiB parts at the 5 000 GiB cap), 1 000 for
    Scaleway/Wasabi/R2. Use a bounded worker pool (~8 per object); per-part retry with backoff;
    no `CopySourceIfMatch` (unsupported on B2); `CompleteMultipartUpload` with
    parts sorted; deferred `AbortMultipartUpload` with `context.Background()`
    on error. URL-escape `CopySource`. aws-sdk-go v1.34.13 already has these
    APIs — no `go.mod` change.
- Record the upload ID on the destination's existing `temp_objects` row
  (`UPDATE ... SET is_multipart = true, upload_id = $2 WHERE object_key = $1`)
  so the cleanup cron can abort it after a crash. Also set the row's
  `content_length` (`GetUploadURLs` leaves it NULL, `file_copy.go` ~97), so
  copies count toward the 1.7 reservation.
- In `file_copy.go`:
  - Pre-check per-file size (`isFileSizeAllowed` for the destination
    collection's app) and total quota (`CanUploadFile`) before any S3 work
    (resolves the TODO at ~89).
  - Populate `File.Size`/`Thumbnail.Size` from `object_keys` so `Create`
    verifies copied sizes (currently 0 → check skipped).
  - Replace the unbounded goroutines (up to 200 concurrent copies) with an
    errgroup with a limit and a cancellable context.

**Accept**: MinIO integration test copying an object above a test-lowered
threshold via the multipart path; a failing part aborts the upload; the size
pre-check rejects before S3 work; Photos copy of small files unchanged.

### Task 1.5 — Async copy for large files (migration 152)

A multi-GiB server-side copy can take minutes and can't sit inside an HTTP
request behind proxies (nginx 60 s, Cloudflare 100 s defaults).

- **Prerequisite refactor**: `FileController.Create` takes a `*gin.Context`
  and uses `ctx.Request.Context()` (`pkg/controller/file.go` ~143-148);
  `onDuplicateObjectDetected` and `IsCopyAllowed` also take gin contexts.
  Split them into a `context.Context` + explicit-parameters core that both the
  HTTP path and the worker call.
- Migration: `file_copy_jobs` (id, user_id, request_id TEXT, app,
  src_collection_id, dst_collection_id, items JSONB, status, result JSONB,
  error, created_at, updated_at) with `UNIQUE (user_id, request_id)`.
  `request_id` is client-supplied; a retry with the same ID returns the
  existing job (no constraint over the JSONB items).
- `POST /files/copy`: async only when the request opts in (`?async=true` with
  a `requestID`) **and** `app == drive`; otherwise behaviour is exactly as
  today. Async returns `202 {jobID}`.
- `GET /files/copy/:jobID`: only the job's owner (404 otherwise); returns
  status and `oldToNewFileIDMap` when done.
- Worker started from `main.go`; `FOR UPDATE SKIP LOCKED`; runs in a context
  detached from any request; **re-runs `IsCopyAllowed` before copying** (the
  sharer may have revoked access since enqueue); uses the 1.4 helper.
- Rate-limit job polling.

**Accept**: same `requestID` twice → one job; worker restart resumes pending
jobs; access revoked after enqueue → job fails with permission error, no
objects left behind; another user can't read the job; Photos sees unchanged
behaviour.

### Task 1.6 — Resumable streaming replication (migration 153)

**Today:**
- Replication downloads the entire object to a temp file before re-uploading
  it (`pkg/controller/replication3.go` ~245-250, ~295-343, ~425-449). That
  needs free disk of size + 2 GiB **per worker** (6 by default).
- `EnsureSufficientSpace` (`pkg/utils/file/file.go` ~40) checks `/`, not
  `replication.tmp-storage`, and counts `Bfree`, not `Bavail`.
- A 5 000 GiB object takes ~15 h per destination at 100 MB/s, ~30 h for
  Wasabi + Scaleway.
- museum doesn't drain work on SIGTERM (`cmd/museum/main.go` ~1131-1136), so
  every deploy kills in-flight attempts.
- An attempt is only re-picked after 24 h (`pkg/repo/object_copies.go` ~31).

So the new path **must survive restarts and resume where it left off**,
otherwise very large objects may never replicate.

- **Migration 153**: `replication_uploads (object_key, dest_dc, upload_id,
  part_size, created_at, PRIMARY KEY (object_key, dest_dc))`.
- **Threshold:** objects above a config threshold (default 1 GiB) take the new
  path; below it, keep today's path to limit blast radius. The threshold must
  be below `DriveMaxFileSize` (unit test, 1.1).
- **Per attempt, per destination:**
  1. HEAD the destination object. If it exists with the expected size, mark
     it replicated and stop. This also covers Wasabi compliance-hold buckets,
     where re-uploading is denied.
  2. If a `replication_uploads` row exists, reuse its upload ID and part size,
     and call `ListParts` (paginated) to skip finished parts. Otherwise call
     `CreateMultipartUpload` and insert the row. Set
     `StorageClass = GLACIER` on Create for Scaleway, as the current upload
     does (~433-435).
  3. Part size `max(64 MiB, ceil(size / 1 000))`, rounded up to 1 MiB
     (exactly 5 GiB at the 5 000 GiB cap; never above 5 GiB).
  4. Upload the missing parts with bounded parallelism (e.g. 2 per object).
     Retry each part with backoff on transient errors.
  5. `CompleteMultipartUpload`, then the existing HEAD size verification
     (~460-477; it compares only `ContentLength`, so multipart ETags are fine),
     then delete the row.
- **Abort** (and delete the row) only on permanent errors: `NoSuchUpload`, a
  size mismatch, or a deleted source object. Never abort on transient failures
  or shutdown.
- **AccessDenied on compliance-hold destinations:** keep today's
  "AccessDenied ⇒ verify with HEAD" rule (~438-441) for Create, UploadPart
  and Complete.
- **Fetch each part once:** a ranged `GetObject` (`Range: bytes=a-b`) from the
  hot bucket, spooled to a uniquely named temp file under
  `replication.tmp-storage` (name it by object key, part number and attempt;
  `CreateTemporaryFile` currently names files by object key only). Upload the
  same spooled part to **every** pending destination before deleting it, which
  avoids doubling hot-bucket reads.
  - SDK v1 `UploadPart` needs an `io.ReadSeeker`, so it computes Content-MD5
    for you. Keep it: on AWS, uploads to Object Lock buckets require
    Content-MD5 (or a checksum header), and Wasabi compliance-hold buckets
    probably behave the same way (unverified).
  - Streaming the GET body straight into a presigned part PUT is a possible
    later optimisation. It drops the automatic MD5, so compute it per part
    first.
- **Worker download path:** `downloadFromB2ViaWorker` rejects anything but 200
  (~329). Accept 206 and check `Content-Range` and length. The deployed
  Cloudflare worker's source isn't in the repo (`infra/workers/data-puller` is
  a stub), so verify Range passthrough against the deployed worker. If it
  doesn't support Range, use direct S3 GETs for this path, and account for the
  B2 egress cost.
- **Disk budget:**
  - `EnsureSufficientSpace(path, size)` checks `replication.tmp-storage` using
    `Bavail`.
  - In-flight part spools and small-object spools take from a **blocking**
    semaphore with a configurable byte budget (default e.g. 40 GiB). Workers
    wait instead of failing; a failure would cost a 24 h re-pick.
  - Worst case without the budget: 5 GiB × 2 parts × 6 workers = 60 GiB.
- **Liveness:**
  - Add per-request and idle timeouts to the download `http.Client` (today
    `&http.Client{}`, ~303) and to the SDK clients.
  - A heartbeat pushes the `object_copies` row's `last_attempt` forward only
    when bytes moved since the previous beat. A hung attempt then still gets
    re-picked.
- **Orphan sweeper:** periodically `ListMultipartUploads` on the Wasabi and
  Scaleway buckets and abort uploads older than N days (config, default 14)
  that have no `replication_uploads` row.
- **Optional:** on SIGTERM, stop picking new replication work.

**Accept** (MinIO source/destinations, test-lowered threshold):
- a multi-part replication succeeds with peak spool ≤ the budget;
- killing the worker mid-way and restarting resumes from the next missing
  part using the stored upload ID;
- a transient part error is retried without aborting;
- an existing destination object is skipped via HEAD;
- the Scaleway destination receives `StorageClass = GLACIER`;
- a hung download is re-picked;
- the sweeper aborts an orphaned upload.

### Task 1.7 — Quota reservation for in-flight uploads (Drive only, required)

With per-file sizes up to 5 000 GiB, concurrent uploads that each pass the
quota check could together park many times the user's quota in S3 for weeks.

- **What counts.** For `app == drive`, `CanUploadFile`
  (`pkg/controller/usage.go` ~54-200) adds the sum of `content_length` over
  `temp_objects` rows that are
  - unexpired, `reservation_released = false` (migration 151);
  - `app = 'drive'`, purpose `file_upload` or copy;
  - owned by the subscription's users;
  - excluding the object keys being committed.

  Photos quota behaviour is unchanged; its abandoned uploads would otherwise
  block near-quota users for 14 days.
- **Consistent read.** Compute current usage and the reservation sum in **one
  SQL statement**, or in a REPEATABLE READ transaction. `FileRepo.Create`
  deletes the temp row and increments usage in one transaction without the
  lock (`pkg/repo/file.go` ~94-104). Two separate READ COMMITTED reads could
  see neither the usage nor the temp row and admit a whole extra file.
- **Lock.** Use a namespaced key so it never collides with Phase 2's tree lock:
  `pg_advisory_xact_lock(hashtextextended('quota:' || subscriptionAdminID, 0))`.
- **Coverage, all Drive:**
  - V2 multipart start;
  - V2 single PUT (up to 5 GiB each). Bypass the `UploadResultCache` fast path
    (`usage.go` ~58-68) for Drive.
  - copies (1.4/1.5 set `content_length`);
  - the family per-member limit (`usage.go` ~186-198): add the member's own
    reservations to `memberUsage`.
- **Ordering.** Restructure Drive upload starts so the lock doesn't span S3
  calls:
  1. Tx: lock → consistent usage + reservation read → check → insert the temp
     row (multipart: `upload_id = NULL`). Commit.
  2. `CreateMultipartUpload` outside the tx, then update the row with the
     upload ID. On S3 failure, release and expire the row.
- **Admission.** *(Changed by the audit.)* A Drive Create whose temp rows all
  hold unreleased reservations isn't blocked by other reservations, but is
  re-checked against committed usage (no reservations) + its size, against
  quota + 50 MiB + bonus (and the family member limit). Otherwise reserving,
  filling the quota through Photos and then committing would reach 2× quota.
- **Releasing.** Commit deletes the row. Abort (1.3) and early expiry (1.2) set
  `reservation_released = true` together with the expiry. The cron's failure
  path pushes expiry forward by a day (`pkg/controller/object_cleanup.go`
  ~124-131), but a released row never re-enters the sum.
- **Cleanup-cron fixes, required for rows with NULL `upload_id`:**
  - `RemoveTempObject` and `SetExpiryForTempObject`
    (`pkg/repo/object_cleanup.go` ~127-152) filter on `upload_id = $n` for
    multipart rows. The NULL is scanned as `""`, so they never match: the row
    stays forever and takes a slot in every `LIMIT 1000` batch. Filter on
    `object_key` only (it's the primary key).
  - In `removeUnreportedObject`, when `IsMultipart && UploadID == ""`, call
    `ListMultipartUploads(Prefix = objectKey)` and abort what it finds. That
    covers a crash between `CreateMultipartUpload` and the row update. Then
    continue with the normal existence check and delete. Never call
    `AbortMultipartUpload` with an empty upload ID.

**Accept:**
- two concurrent 6 GiB Drive multipart starts with 10 GiB free → one gets 426;
- a Create committing concurrently with a reservation check never lets the
  check undercount;
- an aborted upload frees its reservation immediately and stays freed after a
  failed cleanup;
- a family member's per-member limit includes their reservations;
- a reserved Drive upload commits even if quota was consumed meanwhile;
- a multipart row with NULL `upload_id` is cleaned up by the cron;
- Photos quota checks are unchanged.

## Phase 2 — Nested folders (≈3–4 weeks server)

- **Threat model:** with `parent_id` the server learns the shape of each
  user's folder tree (edges, depth, fan-out). Names stay encrypted. This is
  accepted (Proton Drive makes the same trade-off) but must be written into
  the threat model / security docs before Drive ships (open: §6).
- **Integrity:** the server can't forge `parentEncryptedKey`, so a client can
  detect a server-side re-parent by checking that the parent wrap decrypts
  with the claimed parent's key (see the client guide). The server can still
  replay an older valid (parent, wrap) pair, rolling a folder back to a
  previous location; that isn't detectable.
- **Rejected alternative:** keeping the parent pointer only in encrypted
  `pubMagicMetadata`. The server can't validate it (ciphertext), collection
  magic metadata has no version check, so a concurrent edit from another
  device can overwrite it, and there are no atomic subtree moves or deletes.
  Fine for a prototype, not for v1.

### Task 2.1 — Schema (migration 154)

- 154 (after 149 has committed, so `'drive'` is usable), one statement:
  ```sql
  ALTER TABLE collections
    ADD COLUMN parent_id BIGINT NULL,
    ADD COLUMN parent_encrypted_key TEXT NULL,
    ADD COLUMN parent_key_nonce TEXT NULL,
    ADD CONSTRAINT collections_parent_not_self CHECK (parent_id IS NULL OR parent_id <> collection_id) NOT VALID,
    ADD CONSTRAINT collections_parent_drive_folder CHECK (parent_id IS NULL OR (app = 'drive' AND type = 'folder')) NOT VALID,
    ADD CONSTRAINT collections_parent_key_present CHECK (
      (parent_id IS NULL) = (parent_encrypted_key IS NULL) AND
      (parent_id IS NULL) = (parent_key_nonce IS NULL)) NOT VALID;
  ```
  NOT VALID constraints still apply to new and updated rows (rule 4: existing
  insert paths must write NULL, not `''`). They're never validated: every
  existing row has NULL parent columns.
- No foreign key and no new index (the original plan had a composite FK,
  its unique index, a `parent_id` index and a VALIDATE migration; the audit
  removed them because concurrent index builds on `collections` deadlock with
  golang-migrate's blocking lock during multi-pod startup). Tree queries are
  owner-scoped and read the owner's Drive rows through
  `collections_owner_id_index` (a 10 000-folder subtree takes ~9 ms on a
  1M-row table). Same owner, Drive app, folder type and liveness of the parent
  are enforced by the controller under the tree lock; collections are only
  hard-deleted by the user cascade, which removes the whole tree.

**Accept**: migrations apply on a populated DB; a Photos collection create
succeeds after 154 (DB test); a row with `parent_id` set and `app='photos'` is
rejected.

### Task 2.2 — Model, serialization, redaction

- `ente/collection.go` `Collection`: add `ParentID *int64 json:"parentID,omitempty"`,
  `ParentEncryptedKey *string json:"parentEncryptedKey,omitempty"`,
  `ParentKeyNonce *string json:"parentKeyNonce,omitempty"` (pointers, so
  absent ⇔ NULL). Verified safe for old clients (web zod `looseObject`, Locker
  web `z.object` strips unknown keys, Dart `fromMap`, Rust serde).
- Repo `Create` (`pkg/repo/collection.go` ~40-65) writes the three columns as
  NULL unless `ParentID` is set.
- **Owner reads** (populate): `Get` (~67), `GetCollectionByType` (~133),
  `GetCollectionsOwnedByUserV2` (~153, ~187).
- **Non-owner reads** (never populate in phase 2, D9): don't select the new
  columns in `GetCollectionsSharedWithUser` (~252-301, feeds `/collections/v2`
  and `/v3` via `sharedCollectionResponses`, `pkg/api/collection.go` ~79-133);
  nil them in `WithSharingDetailsForUser` (~95-130) and in the controller's
  single-get path (`pkg/controller/collections/collection.go` ~96); blank them
  in public link (`pkg/controller/public/collection_link.go` ~228) and cast
  responses.

**Accept**: JSON snapshot tests for owner / sharee / public link / cast;
Photos and Locker responses byte-identical to before for existing data.

### Task 2.3 — Tree lock helper + create with parent

- Helper `lockCollectionTree(tx, ownerID)`:
  `SELECT pg_advisory_xact_lock(hashtextextended('ctree:' || $1, 0))`.
  Every structural change for a Drive owner takes it **first**, then re-reads
  the rows it validates (READ COMMITTED: reads after acquiring the lock see
  committed state). Users: create-with-parent (2.3), move (2.4), all deletes
  of Drive collections including v3 (2.5).
- Extend `POST /collections` (`pkg/controller/collections/collection.go` ~46-81).
  If `parentID` is set: require `app == drive` and type `folder`; under the
  tree lock, the parent exists, isn't deleted, has the same owner as the actor
  (D6) and app `drive`, type `folder`; `parentEncryptedKey`/`parentKeyNonce`
  present and valid (same format check as owned keys: 48-byte secretbox +
  24-byte nonce, `pkg/controller/collections/key_validation.go` ~20-25);
  depth ≤ 64. Create runs in the same transaction as the checks.
- New errors in `ente/errors.go`: `ErrInvalidParent`, `ErrCollectionCycle`,
  `ErrMaxDepthExceeded`, `ErrHasChildren` (distinct from
  `ErrCollectionNotEmpty`, which means "has files"), and `ErrSubtreeTooLarge`
  for 2.5. All are `ApiError`s with their own `Code`: a plain
  `ErrBadRequest` reaches the client as `{}`, so it can't tell them apart.

### Task 2.4 — Move / re-parent folder

`POST /collections/move-collection`
`{collectionID, newParentID: int|null, parentEncryptedKey?, parentKeyNonce?}`
(next to the collection routes, `cmd/museum/main.go` ~750-783; rate-limited).
In one transaction:
1. `lockCollectionTree(owner)`.
2. Validate: actor owns the moved collection; it is `app = drive`, type
   `folder`, not deleted; the new parent (if any) passes the 2.3 checks; key
   fields present iff `newParentID` is non-null.
3. Cycle + depth check with a recursive CTE over the new parent's ancestors
   (depth-capped), plus the moved subtree's height (descendant CTE):
   ```sql
   WITH RECURSIVE anc(id, pid, d) AS (
     SELECT collection_id, parent_id, 1 FROM collections WHERE collection_id = $newParent
     UNION ALL
     SELECT c.collection_id, c.parent_id, a.d + 1
     FROM collections c JOIN anc a ON c.collection_id = a.pid
     WHERE a.d < 64)
   SELECT bool_or(id = $moved), max(d) FROM anc;
   ```
4. `UPDATE collections SET parent_id = $p, parent_encrypted_key = $k,
   parent_key_nonce = $n, updation_time = now WHERE collection_id = $moved`.

Sync needs no query change: the moved row's `updation_time` bump surfaces it
in `/collections/v2` for the owner.

**Accept**: concurrent-move test (A→under B and B→under A in parallel) never
produces a cycle; moving to root clears the key fields; depth cap enforced;
moving a non-folder or non-Drive collection is rejected.

### Task 2.5 — Deleting folders

Today: `DELETE /collections/v3/:id?keepFiles=` → `TrashV3`
(`pkg/controller/collections/collection.go` ~135-184) → `ScheduleDelete`
(`pkg/repo/collection.go` ~1215) → queue → worker → `repo.TrashV3`
(~1132-1189), which trashes the owner's files (removing them from **all**
collections, `pkg/repo/trash.go` ~156-163) and unlinks others' files. No lock
is taken; link/cast revocation (~171-178) runs outside any transaction.

- **v3 on Drive collections**: take the tree lock; check "no live children"
  (`ErrHasChildren`) and run `ScheduleDelete` in the **same transaction**
  (otherwise a concurrent create-with-parent can produce a live child of a
  deleted parent — the FK doesn't check `is_deleted`). Non-Drive collections
  keep today's code path.
- **New `DELETE /collections/v4/:id?keepFiles=&recursive=`** (Drive only;
  `recursive=false` behaves like v3-on-Drive above):
  - Under the tree lock: collect the subtree with a recursive CTE (cap from
    config, default e.g. 10 000 folders; above it return the new
    `ErrSubtreeTooLarge`, HTTP 400, code `SUBTREE_TOO_LARGE`, and change
    nothing), revoke public links and cast
    tokens for each **inside the transaction**, mark all collections and their
    shares deleted, bump `updation_time`, enqueue all IDs (`QueueRepo.AddItems`).
  - `keepFiles=true`: require zero live files across the whole subtree.
  - `keepFiles=false`: new repo method used by the worker for `app = drive`
    collections (there's no existing primitive — `RemoveFilesV3` refuses
    owner-owned files, ~921-923). Per batch, in one transaction:
    `lockFiles` (`pkg/repo/file_lock.go` ~11); for each owner file, if it has a
    live membership in a **non-deleted collection owned by the same owner**,
    just unlink it from this collection; otherwise trash it; files added by
    others are unlinked as today. Then run the same end-of-run check as
    `TrashV3` (~1169-1186), unchanged: every file still live in the folder
    must be another user's file, which gets unlinked; otherwise return an
    error so the item is retried. Owner files that were unlinked rather than
    trashed have already left the folder, so the check needs no change. Write
    it in the new method; **don't edit `TrashV3`**, which stays the path for
    every non-Drive collection. Marking the whole subtree deleted up front
    makes the result independent of worker order (the worker already
    serialises per owner, `pkg/controller/trash.go` ~210).
- **Dedicated queue**: `TrashCollectionQueueV3` is one FIFO queue shared by all
  users and apps, drained 100 items per minute (`pkg/controller/trash.go`
  ~101, `cmd/museum/main.go` ~1375). A 10 000-folder delete would delay every
  Photos/Locker album delete queued after it by up to ~100 minutes. So:
  - Add `TrashCollectionDriveQueue` and its `itemDeletionDelayInMinMap` entry
    (`pkg/repo/queue.go`, same `-1 * 24 * 60`; precedent:
    `TrashEmptyDriveQueue`).
  - Enqueue Drive collections there: v3-on-Drive, v4, and Drive collections
    during account deletion (`pkg/controller/data_cleanup/controller.go`
    ~131). Give `ScheduleDelete` a transaction-taking variant with a queue
    parameter (v3-on-Drive already needs it in the tree-lock transaction);
    the existing function keeps its signature and queue.
  - Drain it with its own cron function and `running` flag, scheduled
    separately, so a long Drive batch never holds up the Photos/Locker run.
    Reuse `trashCollection` (`pkg/controller/trash.go` ~204), which already
    takes the queue name.
  - Don't copy the 100-item cap: with it, a 10 000-folder tree needs at
    least ~100 runs (≥ 100 minutes) before all its files reach trash. Instead,
    keep fetching batches and processing them until the Drive queue is empty
    or a time budget (config, default ~50 s) runs out; the rest waits for the
    next run. The queue is Drive-only, so this can't affect Photos/Locker.
  - Per-app lock: for Drive collections (by stored app), `trashCollection`
    takes `CollectionTrash:<owner>:drive` instead of
    `CollectionTrash:<owner>` (~219). Photos/Locker keep today's exact name.
    Otherwise a user's long Drive drain holds the shared lock and their own
    album deletes keep missing it. Safe because the two never touch the same
    rows: Drive files can't be in Photos/Locker collections or vice versa
    (task 0.3), `TrashFiles` locks the file rows itself (`lockFiles`), and
    Drive trashing leaves the usage row alone (zero Photos/Locker file-count
    delta, task 0.2). Drive deletes stay serialised per owner.
  - No migration: `queue.queue_name` and `task_lock.task_name` are TEXT.
  - The worker picks the repo method by the collection's **stored app**, not
    by queue, so a misrouted item is still processed correctly.
- **Latency**: until a folder's item is processed its files are neither in
  trash nor restorable. Document in §5 that trash population is eventually
  consistent.
- Folder restore is out of scope; trashed files restore into a client-chosen
  live folder via `/collections/restore-files`.

**Accept**: deleting a 3-level tree removes all folders from the owner's sync
in one step; a file also linked into a live owned folder outside the tree is
unlinked, not trashed; concurrent "v3 delete parent" + "create child" never
leaves a live child of a deleted parent; v3 delete of a Drive parent with
children fails with `ErrHasChildren`; Photos v3 delete unchanged; a Photos
album deleted while a large Drive tree is queued, including one owned by the
same user, is processed on the next Photos/Locker run.

### Task 2.6 — Fix `/collections/v3` pagination (optional, independent)

No client calls `GET /collections/v3` (all use `/v2`), so Drive doesn't need
this (D13). If done: limit on distinct collections (not joined share/link
rows, repo ~150-250), add `hasMore` to the response (`pkg/api/collection.go`
~110-113), never split a timestamp group across pages, and port the oversize
path from `files_diff.go` (~100-108) for a group larger than `limit` (a
recursive delete stamps one timestamp on every row).

**Accept**: 1 500 collections sharing one `updation_time` with `limit=1000`
come back exactly once with correct `hasMore`.

### Task 2.7 — Batched file diff (optional, independent)

File sync is one `/collections/v2/diff` stream per folder (2 500 rows/page), so
a fresh device with N folders makes ≥N requests. Add
`POST /collections/v2/diff/batch` `{items: [{collectionID, sinceTime}]}` (cap
100 collections and 2 500 total rows; same tie-safe paging per collection;
same per-collection access checks and magic-metadata stripping as
`GetDiffV2`), returning per-collection `{diff, hasMore}`. Rate-limited.

---

## Phase 3 — Inherited (cascading) sharing (≈3–5 weeks; refine into tasks before starting)

Goal: sharing a folder grants access to its whole subtree; moving a folder in
or out of a shared subtree grants or revokes access. Design ("b2" — server
materialises inherited share rows):

- **Keys**: a sharee receives the subtree root key sealed to them (as today);
  descendants are decrypted via `parentEncryptedKey` (D7). Lifting D9's
  redaction: return parent fields to sharees who have access to the parent.
  Parent share/unshare/leave must bump the `updation_time` of the sharee's
  rows for every descendant, or their clients keep stale nesting.
- **Schema**: `collection_shares` gains `inherited_from BIGINT NULL`,
  `explicit_role role_enum NULL`, `inherited_role role_enum NULL`;
  `role_type` = the stronger of the two by an explicit rank function
  (**not** enum order: `role_enum` is `VIEWER, COLLABORATOR, OWNER, ADMIN`,
  migrations 60/109). All existing access checks
  (`pkg/controller/access/collection.go` ~22-52, `GetCollectionShareeRole`,
  the download `EXISTS` in `pkg/repo/object.go` ~96-151, diff, social, cast)
  keep working unchanged. Inherited-only rows store `encrypted_key = ''`.
- **Recompute**: one function `recomputeSubtreeAccess(tx, subtreeRoot)` under
  the tree lock, called on share, batch/bulk share, unshare, leave,
  join-via-link, create-with-parent, move, delete and account reset. Losing
  access applies the side effects of `UnShareContext`
  (`pkg/repo/collection.go` ~707-757): remove the sharee's added files, clean
  social data, revoke cast. Bump affected rows' `updation_time` so sync emits
  tombstones.
- **Readers to change**: `WithSharingDetailsForUser` (~120) and
  `GetCollectionsSharedWithUser` (~257) — the two readers of
  `cs.encrypted_key` — return `keyWrappedBy: parentID` + the parent wrap for
  inherited-only rows; `GetSharees` gains an `inherited` flag and its N+1
  (~331) is fixed.
- **Rules**: unsharing an inherited-only row is rejected ("remove it from the
  folder it was shared from"); no re-keying on move-out (parity with today's
  unshare); caps on depth and subtree size.
- **Collaborator-created subfolders** (lifts D6): a collaborator holding the
  parent key creates a child owned by the tree owner with only the parent
  wrap. Requires relaxing `collections.encrypted_key`/`key_decryption_nonce`
  NOT NULL (migration 1) for Drive child rows and a separate create path that
  skips `validateOwnedCollectionKey` (`collection.go` ~47); the owner's client
  lazily adds the master-key wrap.
- **Phase 3b — public links for a subtree (≈1–2 weeks)**: new
  `GET /public-collection/collections` (descendants with parent-wrapped keys),
  optional `collectionID` param on public diff/download/thumbnail/preview/
  file-data/upload routes, validated by one helper that checks the requested
  ID is within the token's subtree (cacheable in `LinkCache`).

- **Risks found in the post-stage-10 audit (resolve while refining):**
  - **Budgets.** Recompute runs under the tree lock, whose transaction
    budget is 5 s (30 s only for recursive delete), with 2 slots per owner.
    Moving a 10 000-folder subtree into a folder with 10 sharees would
    materialise ~100k `collection_shares` rows inside that budget. Phase 2
    move has no subtree-size cap, so adding one in phase 3 would newly reject
    moves that phase 2 allows: decide the cap (or a queued recompute) before
    Drive clients ship.
  - **Owner lock contention.** Sharee actions (leave, join-via-link,
    unshare) would take the owner's `ctree:<owner>` lock and per-owner slots;
    a popular shared tree could keep the owner's own creates and moves at 503.
  - **Unbounded side effects.** "Losing access" reuses
    `removeAllFilesAddedByOthers`, which is unbounded; inside a tree
    transaction it must be queued like deletes, not run inline.
  - **Unvalidated parent wraps** (D7): wraps written by a buggy client
    surface only when sharees can't decrypt. Client verification and repair
    is required from phase 2 on.

Rejected alternative: computing inherited access with recursive CTEs at check
time — touches ~25 call sites including the latency-sensitive download path,
and can't emit tombstones when access is lost.

---

## 5. Client contract summary (for whoever builds the Drive client)

- Send `X-Client-Package: io.ente.drive...`; use V2 upload endpoints only.
- Sync collections with `GET /collections/v2`; files per folder with
  `/collections/v2/diff` (or the batch endpoint if 2.7 ships).
- Folder = collection of type `folder`; top-level folders have no `parentID`;
  root files live in the Drive `uncategorized` collection.
- Child folders carry `parentEncryptedKey`/`parentKeyNonce` (folder key wrapped
  with the parent folder key) in create and move requests.
- File name in encrypted metadata `title`; renames via pubMagicMetadata
  `editedName`; generic `fileType` + MIME in encrypted metadata; placeholder
  thumbnail + `noThumb: true`.
- Keep each owned file in exactly one owned folder (move, don't add).
- **Contributing to someone else's shared folder**: upload into one of your
  own folders (e.g. your root), then `/collections/add-files` to the shared
  folder. The file is owned and billed by you and also appears in your folder.
- Search, sorting, folder sizes and name-conflict handling are client-side over
  the synced, decrypted metadata.
- **Large uploads**: single PUT only up to 5 GiB; above that, multipart with
  `partLength ≥ ceil(size / 10 000)` (≥ 5 MiB, ≤ 5 GiB). Persist
  `{objectKey, completeURL, partLength, part ETags, file key + stream header}`;
  on restart or an S3 403, call `/files/multipart-upload-url/resume`; on
  cancel, `DELETE /files/multipart-upload?objectKey=`. Uploads must keep making
  progress to stay alive (expiry extends on progress, 90-day absolute cap).
  Resume may answer `{completed: true}`: the upload finished but the complete
  response was lost, so go straight to Create. The size cap applies to the
  **encrypted** object: max plaintext ≈ cap − 17 bytes × ceil(cap / 4 MiB)
  (about 20.75 MiB less at 5 000 GiB). A quota rejection at Create keeps the
  upload, so retry Create after the user upgrades. A reserved Drive upload
  isn't re-checked at Create.
- **Deleting folders**: only `DELETE /collections/v4/:id?keepFiles=&recursive=`
  (both required; `recursive=false` for a plain delete). Folders disappear
  immediately; files show up in trash progressively (eventually consistent).
  Show a live folder whose parent is deleted or unknown at the root. Full
  contract in the client guide §5.1.
- **Sharing (until phase 3)**: sharing a folder shares only the files directly
  inside it; sharees see each shared folder as a root.

## 6. Open questions / follow-ups (not blocking)

- **Photos follow-ups** (deliberately not changed here): pass the size to the
  quota check at multipart start once Photos mobile maps a 426 from the
  multipart-URL call to `StorageLimitExceededError` and keeps the track;
  consider a shorter (not immediate) expiry for quota-rejected Photos uploads.
- **Thumbnail size** is never capped: a user can upload an unbounded object
  through a V1 Photos URL and commit it as a Drive file's thumbnail (object
  keys only carry the user-ID prefix). Pre-existing for Photos; bounded by
  quota. Consider a thumbnail cap for all apps.
- **Native Drive app login & billing**: the accounts passkey redirect
  allowlist has no Drive scheme (`web/apps/accounts/src/services/passkey.ts`
  ~189-200); Play Store purchases are tied to `PlayStorePackageName =
  "io.ente.photos"` (`pkg/controller/playstore.go` ~33). Needs a product
  decision before a native Drive client ships.
- **Photos: files committed into a deleted album** (found in stage 10).
  `FileRepo.Create`, `AddFiles` and `MoveFiles` bump the target collection
  without checking `is_deleted`, so a file committed after the trash worker
  listed the album's files ends up in no live album and not in trash. Fixed
  for Drive only; the same guard would fit Photos/Locker.
- **Threat model / security docs**: record that the server learns the shape
  of each user's Drive folder tree (Phase 2 threat-model note). Not written
  yet; needed before Drive ships.
- **Drive capability bit** in `serverApiFlag` (stage 1 deferral, client guide
  §11), so Drive clients can refuse old servers.
- **Add/move/restore-files cost one extra query for every app**
  (`validateFileApps` → `GetDistinctFileApps`, a PK lookup on ≤ 1 000 ids).
  Accepted; it could be folded into the ownership query later.
- **Resume vs Create size rule**: resume answers 410 when the completed
  object's size differs from `content_length`, while Create admits a reserved
  key whose object is smaller. Only a misbehaving client reaches it.
- **`keepFiles=true` deletes scan deleted `collection_files` rows**: a folder
  with a very long history can hit the tree budget (503) every time; the
  client can fall back to `keepFiles=false` for a folder it knows is empty.
- **`TrashV3`'s "investigate" error** reports `count - removedFiles` with the
  sign reversed (pre-existing; cosmetic, log text only).
- Whether Drive collections should be excluded from cast, social (comments /
  reactions) and embeddings endpoints, or simply unused by the Drive client.
- **Upstream or fork:** every change here is to Ente's server. Running Drive
  against Ente's hosted servers needs the changes merged upstream; otherwise
  this is a maintained fork, and migration numbers must be reconciled on every
  rebase.
- **aws-sdk-go v1** (`go.mod`, v1.34.13) reached end of support on
  2025-07-31. Nothing here requires upgrading (all APIs used exist in v1), but
  new S3 code should be easy to port to v2 later.
- **Scaleway Glacier restores** treat each part of a multipart object as a
  separate object, so restoring a 1 000-part file is about as slow as
  restoring 1 000 objects. Account for this in the disaster-recovery runbook
  (Glacier is the cold replica).

## 7. Out of scope / known limitations

- Files larger than 5 000 GiB of ciphertext (1 000 replication parts × 5 GiB).
- File version history (D10).
- Folder trash/restore.
- Server-side search (incompatible with E2EE).
- Random-access decryption within a file (secretstream format is sequential).
- Optimistic concurrency on collection rename / magic metadata
  (last-write-wins; accepted tradeoff).

## 8. References

Only sources that are useful while implementing. Provider pages were last
checked in 2026-10; limits change.

**S3 multipart semantics** (the server signs these calls; clients and
replication make them):
- AWS S3 limits (part 5 MiB–5 GiB, 10 000 parts):
  https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html
- `UploadPartCopy` (task 1.4):
  https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPartCopy.html
- `ListParts` paging (tasks 1.3, 1.6):
  https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListParts.html
- `ListMultipartUploads` (cleanup cron, replication sweeper):
  https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListMultipartUploads.html
- aws-sdk-go v1 `s3` package:
  https://docs.aws.amazon.com/sdk-for-go/api/service/s3/

**Providers:**
- Scaleway multipart uploads (1 000-part limit):
  https://www.scaleway.com/en/docs/object-storage/api-cli/multipart-uploads/
- Scaleway Glacier restore (per-part restore behaviour):
  https://www.scaleway.com/en/docs/object-storage/how-to/restore-an-object-from-glacier/
- Wasabi multipart uploads:
  https://docs.wasabi.com/docs/how-does-wasabi-handle-multipart-uploads
- Wasabi compliance mode (affects replication, task 1.6):
  https://docs.wasabi.com/apidocs/compliance-with-the-wasabi-s3-api.md
- Backblaze B2 large files and S3 `UploadPart`:
  https://www.backblaze.com/docs/cloud-storage-large-files,
  https://www.backblaze.com/apidocs/s3-upload-part
- B2 `CopyObject` rejecting 5 GiB sources (reason for the 4 768 MiB copy
  threshold in task 1.4): https://forum.rclone.org/t/copying-files-within-a-b2-bucket/16680

**Postgres 15** (the version in `compose.yaml`):
- `ALTER TYPE … ADD VALUE` can't be used in the same transaction (task 0.1):
  https://www.postgresql.org/docs/15/sql-altertype.html
- `NOT VALID` / `VALIDATE CONSTRAINT` (tasks 0.1, 2.1):
  https://www.postgresql.org/docs/15/sql-altertable.html
- `CREATE INDEX CONCURRENTLY` (task 2.1):
  https://www.postgresql.org/docs/15/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY
- Advisory locks (tree lock, quota lock):
  https://www.postgresql.org/docs/15/explicit-locking.html#ADVISORY-LOCKS
- `WITH RECURSIVE` (cycle and subtree queries):
  https://www.postgresql.org/docs/15/queries-with.html#QUERIES-WITH-RECURSIVE
