# Ente Drive — changes visible to existing clients

Companion to [`drive-plan.md`](./drive-plan.md). Lists every change in this
work that alters the **external** behaviour of an existing app (Photos, Locker,
Auth): a request that returns a different status, body or side effect than it
does today. Changes that are purely internal (extra queries, refactors,
schema) aren't listed here.

Each entry states what changes, who is affected, and whether it's accepted or
must be gated to `app == drive`.

---

## Deploying and rolling back the Drive release

Not a client-visible change, but the operational notes for shipping stages
1–10 (one release: none of it is deployed yet).

**Migrations 149–154.** Every one is catalog-only or creates a new table, so
an ordinary rolling deploy works; no single-process step is needed. Each of
these takes a brief `ACCESS EXCLUSIVE` lock:
- 150 on `files` (swaps the app CHECK, `NOT VALID`, no scan);
- 151 on `temp_objects` (adds nullable columns);
- 154 on `collections` (adds the folder-tree columns and `NOT VALID` CHECKs).

A plain `ALTER TABLE` queues behind any open transaction on its table (or a
running autovacuum) and, while it waits, blocks every app's queries on that
table. So these migrations retry the lock in a loop with a 200 ms
`lock_timeout` and a 0.5 s pause: other queries wait at most ~200 ms at a
time, and the migration applies as soon as the table is free (tested: a 5 s
reader delayed the migration 5 s, other reads and writes ≤ 202 ms). The loop
never gives up, because a failed migration would leave the version dirty and
block every museum process. A migration still waiting behind a long
transaction (`pg_dump`, an anti-wraparound vacuum) delays startup: other
starting processes wait at most 15 s for golang-migrate's lock and then exit,
so they restart until it's done. Check `pg_stat_activity` and
`pg_stat_progress_vacuum` before deploying.

**Requirements.**
- At least one museum process with `jobs.cron.skip: false`. It runs the
  async copy worker and the Drive folder-trash cron; without one, async copy
  jobs stay pending (holding their reservations for up to 14 days) and
  deleted Drive folders never reach the trash.
- Streaming replication (Drive objects above
  `replication.streaming-threshold`) needs free space on `tmp-storage` of at
  least the spools in flight + one part + 22 GiB; it fails fast otherwise.

**Rolling back** is unsupported once Drive data exists (any row with
`app = 'drive'`). Before that, run the `.down.sql` files 154 → 149 first:
an older binary refuses to start on a database version it doesn't know.
If you roll back anyway after Drive was used:
- 152 down drops pending async copy jobs; their reservations stay until the
  rows expire (≤ 14 days, Drive quota only).
- 153 down forgets in-progress streaming uploads. Wasabi expires them;
  Scaleway keeps the parts (billed) until they're aborted by hand.
- Old binaries don't drain the `trashCollectionDrive` and `trashEmptyDrive`
  queues. Folders deleted before the rollback disappear from sync, but their
  files are neither trashed nor visible, and still count towards quota; an
  account deletion can't bring usage to zero, so `storageCheck` retries until
  its attempt limit. To let the old binary finish the folder deletes (with
  Photos semantics: it also trashes owned files linked into another live
  folder):

  ```sql
  UPDATE queue q SET queue_name = 'trashCollectionV3'
  WHERE q.queue_name = 'trashCollectionDrive' AND NOT q.is_deleted
    AND NOT EXISTS (SELECT 1 FROM queue v
                    WHERE v.queue_name = 'trashCollectionV3' AND v.item = q.item);
  ```

  After moving forward again, the new V3 cron moves any Drive items left in
  `trashCollectionV3` back to the Drive queue.
- Old binaries delete Drive folders and commit files without the tree lock
  and the deleted-folder guard, so a live folder can end up under a deleted
  parent. The new worker moves such children to the root when it processes
  the parent; to repair any left over, run once after moving forward:

  ```sql
  UPDATE collections AS c
  SET parent_id = NULL, parent_encrypted_key = NULL, parent_key_nonce = NULL,
      updation_time = now_utc_micro_seconds()
  FROM collections AS p
  WHERE c.parent_id IS NOT NULL AND c.parent_id = p.collection_id
    AND c.owner_id = p.owner_id AND p.is_deleted AND NOT c.is_deleted;
  ```
- Locks left by a rolled-back or crashed process (`CollectionTrashDrive`)
  make `CleanupExpiredLocks` log one "Non zero expired locks" Error, as for
  other cron locks.

---

## 1. `POST /files/copy`: upfront total-quota check (task 1.4)

**Not changed for Photos/Locker.** The plan proposed a batch-level quota
pre-check for every app; stage 5 gated it to `app == drive`. Photos/Locker
keep today's per-file check at Create (a batch of small files can still push
usage past quota, as before). Kept for the record.

---

## 2. Early expiry of uploads rejected as too large (task 1.2, stage 2)

**Change.** When `POST /files` or `PUT /files/update` returns 413
(`ErrFileTooLarge`) for any app, the uploaded file object's temp row is
expired at once, and the cleanup cron deletes the object within minutes
instead of after 14 days. Public-link Creates are excluded.

**Who notices.** Only a client that retries Create with the same object key
after a 413. Today that retry gets 413 again, unless the user's internal-user
flag was turned on in the meantime. Now, once the cron has run, it gets 503
`OBJECT_SIZE_FETCH_FAILED`, and Photos mobile drops its multipart track and
re-uploads. A 413 at Create is reachable only through old V1-URL clients, a
config change, or `/files/copy` of a 10–20 GiB file.

**Status.** Accepted (plan 1.2): the object can never be committed.

---

## 3. `POST /files/copy`: multipart fallback for large objects (task 1.4, stage 5)

**Change.** For Photos/Locker, today's single `CopyObject` call is still made
first, unchanged. Only when it fails for an object above 4 768 MiB that is
within the app's size limit does the server retry the copy in parts
(`UploadPartCopy`) instead of returning the error.

**Who notices.** Photos "save a copy" of a large shared file whose
`CopyObject` fails today: on B2 (production) objects above 5×10⁹ bytes; on
other stores, objects above their own `CopyObject` limit (5 GiB on AWS). These
requests now succeed instead of returning an empty 500. If the multipart copy
also fails, the client still gets a 500 (`{}`; `INTERNAL_ERROR` if the
destination row vanished). A Warn log is added on fallback.

**Status.** Accepted: only requests that fail today change, from error to
success.
