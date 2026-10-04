# Ente Drive — client guide to the backend

Audience: authors of Ente Drive clients (web, desktop, mobile, CLI).
Companion to [`drive-plan.md`](./drive-plan.md), which is the server
implementation plan. This guide explains how a Drive client must talk to
museum: the existing APIs it builds on, the new APIs the plan adds, and the
gotchas we found while investigating the backend.

> **Status.** The server side of everything below is implemented, except
> phase 3 (inherited sharing) and the optional plan tasks 2.6/2.7. Tags:
> - **[existing]**: predates Drive; behaviour as of `main`.
> -: not implemented yet.
>
> Section 6 (large uploads) reflects the plan after two adversarial reviews.

---

## 1. Mental model

| Drive concept | Backend concept |
|---|---|
| Account | Ente user, master key derived on the client (same as Photos). |
| The Drive app's data space | `app = drive`, selected by a request header (§2). Completely separate from Photos and Locker data. |
| Folder | A **collection** of type `folder`, with its own symmetric key. |
| Folder tree | `collections.parent_id`. |
| "My Drive" root | Implicit. Top-level folders have no `parentID`. Files at the root live in the Drive `uncategorized` collection. |
| File | A **file** record with three parts: the encrypted file object (S3), an encrypted thumbnail object (S3, required), and encrypted metadata (inline). |
| File name, type, dates | Inside the encrypted metadata. The server never sees them. |
| Search, sort, folder size | Done entirely on the client over synced, decrypted metadata. |
| Sharing | Per folder (collection), with roles. Single-file public links also exist. |

**End-to-end encryption.** The server stores ciphertext and opaque blobs. It
can see IDs, owner, which collection a file is in, encrypted sizes (roughly
the plaintext size), timestamps, the sharing graph and the
shape of the folder tree. It can't see names, types, contents or anything in
(magic) metadata. Everything that needs that information happens on the client.

---

## 2. App identity

- Send `X-Client-Package: io.ente.drive<...>` on **every** request, including
  auth/login. The server maps the prefix to `app = drive`.
- ⚠️ **Gotcha: unknown or missing packages fall back to `photos`.** A
  request without the Drive header creates or reads Photos data, so Drive
  folders would show up in users' Photos apps. Treat a missing header as a bug
  and test for it.
- Auth tokens are bound to the app they were issued for. A Drive session token
  only works for Drive data.
- Drive shares the user's Ente subscription and storage quota with Photos (D2
  in the plan). There are no Drive-specific caps.
- Open product questions affecting native clients (plan §6): passkey login
  redirect allowlist and Play Store billing are both tied to Photos today.

---

## 3. Keys and encryption

- **Collection (folder) key**: random symmetric key. On create, send:
  - `encryptedKey` + `keyDecryptionNonce`: the folder key secretbox'd with the
    user's master key (48-byte ciphertext + 24-byte nonce, validated by the
    server). **[existing]**
  - For child folders, also `parentEncryptedKey` + `parentKeyNonce`: the folder
    key secretbox'd with the **parent folder's key**. Required whenever
    `parentID` is set, and must be re-sent on every move to a non-root parent
    (a move to the root takes no key fields).
    It exists so that a future "share a folder tree" feature only needs the
    root key.
- **Folder name**: `encryptedName` + `nameDecryptionNonce`, encrypted with the
  folder key. Don't use the legacy plaintext `name` field.
- **File key**: random per file. Sent as `encryptedKey` + `keyDecryptionNonce`,
  wrapped with the key of the collection the file is being added to. Adding,
  moving or restoring a file into another collection means re-wrapping the
  file key with that collection's key.
- **File and thumbnail contents**: libsodium secretstream
  (XChaCha20-Poly1305), in 4 MiB plaintext chunks; each encrypted chunk is
  4 MiB + 17 bytes. Each object has a `decryptionHeader`.
  - ⚠️ **No random access.** Each chunk's state depends on the previous one, so
    you can't decrypt from the middle of a file. Byte-range downloads work on
    the ciphertext, but decryption must start at byte 0 and run in order.
    In-file seeking (e.g. video scrubbing) needs a different format or derived
    previews; Photos uses HLS previews stored via `/files/data` for video.
  - Streaming encryption and decryption run in constant memory. Don't buffer
    whole files (see §6.5).
- **Metadata**: a JSON blob encrypted with the file key
  (`metadata.encryptedData` + `metadata.decryptionHeader`), sent inline. The
  whole create request is capped at **4 MiB**, so keep metadata small.

---

## 4. Syncing

The client keeps a full local, decrypted mirror of the user's Drive metadata.
There's no server-side "list folder" or search.

### 4.1 Collections

- `GET /collections/v2?sinceTime=<µs>` **[existing]**: every collection the
  user owns, plus those shared with them, that changed since `sinceTime`.
  Unpaged. Use this one.
- ⚠️ **Don't use `/collections/v3`.** No client uses it; it has known paging
  bugs (rows sharing a timestamp can be lost) and no `hasMore` (plan 2.6).
- Deleted collections come back with `isDeleted: true`. Unshared ones come
  back as tombstones with name and key fields nulled.
- A collection's `updationTime` changes whenever its files change (add, move,
  remove), not only when the collection itself does. **Use it to decide which
  folders to diff.**

### 4.2 Files

- `GET /collections/v2/diff?collectionID=<id>&sinceTime=<µs>` **[existing]**:
  one folder at a time. Response `{diff: [...], hasMore}`.
  - About 2 500 rows per page, but a page never splits rows that share a
    timestamp, so it can be bigger or smaller than that. Keep fetching while
    `hasMore`, using the max `updationTime` you received as the next
    `sinceTime`.
  - Rows with `isDeleted: true` mean "removed from this collection", which
    isn't the same as "trashed" (see §5.6).
- ⚠️ **Cost:** there's no account-wide file feed. A fresh device with N
  folders makes at least N requests. Optimise by only diffing folders whose
  `updationTime` changed. A batch endpoint `POST /collections/v2/diff/batch`
  may ship **[planned · 2.7, optional]**.
- Fetch a single file: `GET /collections/file?collectionID=&fileID=`
  **[existing]**.

### 4.3 Trash

- `GET /trash/v2/diff?sinceTime=` **[existing]**, filtered to the Drive app.

### 4.4 Search

Entirely client-side over the local mirror: name (`title`, or `editedName` if
the file was renamed), type, dates, and so on. Locker does a lowercase
substring match in memory; anything fancier (an index, fuzzy matching) is up
to you. Server-side search will never exist (it's incompatible with E2EE).

---

## 5. Folders and files

### 5.1 Folders

| Operation | API | Notes |
|---|---|---|
| Create | `POST /collections` **[existing]**, + `parentID`, `parentEncryptedKey`, `parentKeyNonce` | type `folder`; the parent must be your own live Drive folder (D6: not one shared with you), else 400 `INVALID_PARENT`. Both key fields are required with `parentID` and rejected without it (400 `BAD_REQUEST`; the server checks only their format, 48 + 24 bytes, not that they decrypt: treat as a client bug). Max depth 64 (a top-level folder is depth 1), else 400 `MAX_DEPTH_EXCEEDED`. 503 `COLLECTION_TREE_BUSY`: retry with backoff. The response echoes the parent fields. Not idempotent: a retry after a lost response creates a duplicate folder. ⚠️ Servers without 2.3 (older ones, or old pods during a deploy) **silently drop** the parent fields and create a top-level folder: check `parentID` in the response and move the folder if it's missing. |
| Move / re-parent | `POST /collections/move-collection` `{collectionID, newParentID: int \| null, parentEncryptedKey?, parentKeyNonce?}` | `newParentID` is **required**; `null` moves to root (then no key fields). Key fields as for create. Errors: moved folder missing or not yours → 404 `NOT_FOUND`; already deleted → 404 `COLLECTION_DELETED` (drop the queued move); not a Drive folder → 400 `INVALID_COLLECTION`; bad new parent → 400 `INVALID_PARENT`; into itself or a descendant → 400 `COLLECTION_CYCLE`; too deep (new parent's depth + moved subtree height > 64) → 400 `MAX_DEPTH_EXCEEDED`; 503 `COLLECTION_TREE_BUSY` → retry. Success: 200 with an empty body; re-sync `/collections/v2` (only the moved folder's `updationTime` changes). A move to the same parent is allowed (re-wraps the key). Rate limit 500/min per user (429). Old servers answer 404 (plain text): keep the move queued and retry. |
| Rename | `POST /collections/rename` `{collectionID, encryptedName, nameDecryptionNonce}` **[existing]** | Owner only. **Last write wins**, no version check. |
| Delete | `DELETE /collections/v4/:id?keepFiles=<bool>&recursive=<bool>` | The only delete a Drive client should use; `recursive=false` for a plain delete. See "Deleting folders" below. ⚠️ Don't use `DELETE /collections/v3` for Drive: old pods during a deploy run it without the tree lock. |
| Folder metadata | `PUT /collections/magic-metadata` (owner-only, private), `/public-magic-metadata` (visible to sharees), `/sharee-magic-metadata` (per sharee) **[existing]** | Collection magic metadata has **no version check**: last write wins. Merge carefully. |

**Deleting folders**:

`DELETE /collections/v4/:id?keepFiles=<bool>&recursive=<bool>`. Both
parameters are required.
- `recursive=false` deletes one folder, which must have no live subfolders.
  `recursive=true` deletes the folder and every live folder below it.
- `keepFiles=true` requires zero live files in every folder being deleted.
  `keepFiles=false` handles the files as described under "What happens to
  the files".

| Result | Meaning / what to do |
|---|---|
| 200, empty body | Deleted, or already deleted (idempotent: a retry after a lost response is safe). Re-sync `/collections/v2`. |
| 400 `SUBTREE_TOO_LARGE` | `recursive=true` and the tree has more folders than the server allows. Nothing was deleted. See "Tree too large to delete" below. |
| 409 `COLLECTION_NOT_EMPTY` | `keepFiles=true` but a folder being deleted has files. Ask whether to delete the files too (`keepFiles=false`). |
| 409 `HAS_CHILDREN` | `recursive=false` but the folder has subfolders. Offer a recursive delete. |
| 400 `INVALID_COLLECTION` | A non-Drive collection, or the Drive `uncategorized` (root) or `favorites` collection. Client bug. |
| 404 `NOT_FOUND` | The folder doesn't exist or isn't yours. Drop the queued delete. |
| 400 `BAD_REQUEST` | Missing or invalid `keepFiles`/`recursive`, or a non-numeric ID. Client bug. |
| 503 `COLLECTION_TREE_BUSY` | Too many concurrent folder changes (your own, or recursive deletes on the server). Retry with backoff. |
| 429 | Rate limit (500/min per user). |
| 404, plain text | The server predates v4 (an older server, or an old pod during a deploy). Keep the delete queued and retry later. Don't fall back to v3. |

When more than one error applies, the precedence is `SUBTREE_TOO_LARGE`, then
`COLLECTION_NOT_EMPTY`, then `HAS_CHILDREN`. For example, a non-recursive
`keepFiles=true` delete of a folder with both files and subfolders returns
`COLLECTION_NOT_EMPTY`.

A recursive delete of a big tree can take tens of seconds. Send **one delete
at a time** per account, use a client timeout of **at least 60 s**, and back
off on 503.

**What happens after a delete**:
- All folders in the subtree are marked deleted at once. They come back from
  `/collections/v2` with `isDeleted: true`. Their public links and cast
  sessions are revoked.
- A deleted folder's tombstone can show up again in later syncs while the
  server processes it. Treat repeats as no-ops.
- `/collections/v2/diff` on a deleted folder returns 404. Drop the folder's
  local contents instead of diffing it.
- With `keepFiles=false`, your own files are **trashed** unless they're also in
  another live folder of yours. Those files are just unlinked and stay in the
  other folder. Files other people added are unlinked from the folder, and
  they keep them.
- ⚠️ **Eventually consistent:** a background worker moves files to the trash
  progressively, which can take minutes for a large tree. Until then, some
  files are in neither a folder nor the trash, and can't be restored yet.
  Show a "deleting…" state, and don't treat missing trash entries as lost.
- An "empty trash" started during that window doesn't catch files that reach
  the trash later. Storage quota is freed only when the trashed files are
  permanently deleted (empty trash, delete forever, or after 30 days).
- Folder restore isn't supported. Trashed files are restored into a folder
  you choose.

**Tree too large to delete**:
- The server limits how many nested folders one recursive delete can cover.
  The limit is server configuration and can change: don't hardcode it, and
  don't try to pre-check folder counts against it.
- Over the limit, the delete fails with HTTP **400** and
  `{"code": "SUBTREE_TOO_LARGE"}`. Nothing is deleted. Match on `code`, never
  on `message`.
- Show the user an error explaining that there is a limit to how many nested
  folders can be deleted at once, and that they should pick a smaller folder
  (for example, delete some of its subfolders first). Don't retry
  automatically.

**Folders whose parent is missing**: show a live folder at the root when its
`parentID` points to a folder that's deleted or not in your local mirror.
This can happen briefly during a server deploy. The server repairs such
folders by moving them to the root, and the moved folder then syncs with
`parentID` absent.

**Things the server doesn't enforce, so the client must:**
- **Name uniqueness within a folder** (names are encrypted). Resolve conflicts
  on the client, e.g. "Report (1).pdf".
- **Conflicting edits from two devices** to folder name or metadata: last
  write wins. Concurrent moves are safe (server-side lock + cycle check), but a
  folder ends up wherever the last move put it.
- **Verify the tree you're shown**. The server stores
  `parentID` in plaintext and could lie about it. For each owned child folder,
  check that `parentEncryptedKey` decrypts with the claimed parent's key and
  yields the same folder key you get from `encryptedKey`. If it doesn't, the
  server re-parented the folder; treat it as a root and flag it. The server
  can't forge this, but it could replay an older valid parent (rolling a move
  back), which you can't detect. It can also present a folder as top-level
  (no `parentID`), which looks the same as a real move to the root.

### 5.2 The root

- Top-level folders: `parentID` absent.
- Root-level files live in the Drive `uncategorized` collection. Create it on
  first use (one per user per app; the server enforces uniqueness). It can't be
  deleted, and can only be shared as VIEWER; don't offer to share "My Drive".

### 5.3 Files: where they live

- **One owned folder per file** (D8). Move files with
  `POST /collections/move-files` `{fromCollectionID, toCollectionID, files: [{id, encryptedKey, keyDecryptionNonce}]}`
  **[existing]**. Up to 1 000 per call, atomic per call. You must own both
  folders and every file.
- ⚠️ **Don't use `add-files` for your own files.** It links a file into a
  second folder, which breaks the "one place" model, and trashing it later
  removes it from every folder at once.
- ⚠️ **You can't "remove" your own file from your own folder.**
  `POST /collections/v3/remove-files` refuses files owned by the collection
  owner. Move or trash them instead.
- Duplicate a file of yours: there's no server-side duplicate. Re-upload, or
  (if acceptable) link the same file into another folder with `add-files`.

### 5.4 File metadata and rename

- Encrypted metadata (immutable after upload, except via `/files/update`):
  `title` (original name), `fileType`, MIME type, creation/modification time,
  content `hash`. Choose and document your own `fileType` values. Locker uses
  `other` for generic files and `info` for non-file items; Photos uses
  `0 image / 1 video / 2 livePhoto`.
- **Rename** a file: set `editedName` in the file's **public** magic metadata
  via `PUT /files/public-magic-metadata` **[existing]**.
  - Send the current `version` and the full key set. The server increments
    the version, and rejects (HTTP **409**) a version mismatch or a request
    that drops more than 2 keys. On 409: refetch, merge, retry.
  - The version check isn't transactional, so two simultaneous writers can
    still race. Treat 409 as a hint, not a guarantee.
- Private file magic metadata (`PUT /files/magic-metadata`) is owner-only and
  stripped for sharees.

### 5.5 Thumbnails (required)

- The server **requires** a thumbnail object and decryption header for every
  file. For non-media files, upload a tiny encrypted placeholder (Locker
  encrypts a constant black JPEG) and set `noThumb: true` in public magic
  metadata so the UI doesn't render it.
- Replace a thumbnail later with `PUT /files/thumbnail` **[existing]**; it
  can't be larger than the current one.

### 5.6 Trash

| Operation | API |
|---|---|
| Trash | `POST /files/trash` `{items: [{fileID, collectionID}]}` (owner only, ≤ 1 000) **[existing]** |
| List | `GET /trash/v2/diff` **[existing]** |
| Restore | `POST /collections/restore-files` into a live folder you own **[existing]** |
| Delete forever | `POST /trash/delete` **[existing]** |
| Empty trash | `POST /trash/empty` (async, queued) **[existing]** |

- ⚠️ Trashing a file removes it from **every** collection it's in, not only
  the one you're looking at.
- Deleting a folder with `keepFiles=false` fills the trash progressively,
  not at once (§5.1, "What happens after a delete").
- Trash is kept for **30 days**, then deleted permanently.

### 5.7 Replacing file contents

- `PUT /files/update` **[existing]** swaps in new file + thumbnail objects
  (uploaded with the normal upload flow). Same size and quota checks as create.
- **No version history.** Old objects are deleted after about 24 days.

### 5.8 Duplicates

- The server can't detect duplicates (unique keys per file, so ciphertexts
  never match). Dedupe on the client using the `hash` in metadata.
- `GET /files/duplicates` **[existing]** only groups your files by encrypted
  size, as a cheap candidate list. Also, it isn't filtered by app.

---

## 6. Uploading and downloading

> Planned endpoints here follow plan tasks 1.1–1.7; shapes may still change.

### 6.1 Upload flow

1. **Get URL(s).** Only V2 endpoints; the V1 `GET` routes are rejected for
   Drive.
   - Single PUT: `POST /files/upload-url` `{contentLength, contentMD5}` →
     `{objectKey, url}`. **≤ 5 GiB** (Drive rejects larger).
     Upload with exactly that `Content-Length` and `Content-MD5` (both are
     signed into the URL).
   - Multipart: `POST /files/multipart-upload-url`
     `{contentLength, partLength, partMd5s?}` → `{objectKey, partURLs[], completeURL}`.
     Rules: `5 MiB ≤ partLength ≤ 5 GiB` (smaller only if it's a single
     part); at most **10 000 parts**, so for big files
     `partLength ≥ ceil(contentLength / 10 000)`. Each part URL is signed for
     that part's exact length (and MD5 if you sent `partMd5s`). If you send
     MD5s, you have to know them before uploading; omit them to stream.
2. **Upload** each part straight to S3; keep each part's `ETag`.
3. **Complete** (multipart): POST the S3 `CompleteMultipartUpload` XML (part
   numbers + ETags) to `completeURL`. This goes to S3, not museum.
4. **Upload the thumbnail** the same way (always required, §5.5).
5. **Create** the record: `POST /files` with `collectionID`, `encryptedKey`,
   `keyDecryptionNonce`, `file: {objectKey, decryptionHeader, size?}`,
   `thumbnail: {...}`, `metadata: {encryptedData, decryptionHeader}`,
   optional `pubMagicMetadata`, and `updationTime`.
   - You must own the target collection, even when contributing to a shared
     folder (see §7.2).
   - The server checks each object's real size with S3 HEAD requests, then
     applies the size cap and the quota.
   - Only object keys issued by step 1 are accepted, and only within their
     expiry window.

### 6.2 Limits and errors

| Limit | Value |
|---|---|
| Max file size (Drive) | **5 000 GiB** of encrypted size. |
| Max file size (public-link uploads into a Drive folder) | 10 GiB |
| Single PUT | ≤ 5 GiB |
| Parts | ≤ 10 000; 5 MiB–5 GiB each |
| Request body (JSON) | 4 MiB |
| Signed URL validity | 7 days |
| Upload-URL rate limit | 500 requests/min per user per route |
| Max folder depth | 64 (a top-level folder is depth 1) |
| Folder moves rate limit | 500 requests/min per user |
| Folder deletes (v4) rate limit | 500 requests/min per user |
| Copy jobs | 60 async copy enqueues and 200 job polls per minute per user; at most 10 unfinished jobs per user |
| Pending-uploads listing | 500 requests/min per user |

The cap applies to the **encrypted** file object (the thumbnail isn't
counted). Encryption adds 17 bytes per 4 MiB chunk plus the header, so the
largest plaintext is about `cap − 17 × ceil(cap / 4 MiB)`, roughly 20 MiB
under the cap. Check the encrypted size before starting, so you don't find out
after uploading terabytes.

| HTTP | Meaning |
|---|---|
| 426 | Storage quota exceeded (`ErrStorageLimitExceeded`). For Drive, also returned when **requesting** upload URLs. |
| 503 `QUOTA_CHECK_BUSY` | Drive upload-URL requests and async copy enqueues: too many concurrent quota checks for your account (more than 2 at once per subscription, or a busy server). Retry with backoff. |
| 503 `COLLECTION_TREE_BUSY` | Folder create-with-parent, move or delete: too many concurrent folder-tree changes. Retry with backoff. |
| 410 `UPLOAD_GONE` | Drive multipart-URL request: the upload was cancelled (`DELETE`, §6.4) while it was starting. |
| 413 | File too large (at create/update) |
| 400 | Bad request, including "contentLength exceeds max file size" at the URL step and "too many parts" (the message gives the minimum part length) |
| 402 | No active subscription |
| 409 | Magic-metadata version mismatch. Folder delete: `COLLECTION_NOT_EMPTY` or `HAS_CHILDREN` (§5.1) |
| 404 `COLLECTION_DELETED`, or plain 404 `{}` | `POST /files` into a deleted Drive folder returns `COLLECTION_DELETED`. `add-files`, `move-files` and `restore-files` into a folder that was already deleted return a plain 404; they return `COLLECTION_DELETED` only when the delete commits while the request runs. Treat both the same: drop the target, pick a live folder and retry there. |

### 6.3 Quota

- Quota is shared with the Photos subscription (plus storage bonus; family
  plans pool usage). Read it from `GET /users/details/v2` **[existing]**.
- For Drive, bytes of uploads **in progress** count against quota until they
  are committed, aborted or expire. Abort uploads you
  abandon (§6.4) so the space comes back right away. `GET /users/details/v2`
  shows committed usage only, not these reservations.
- **List your pending uploads:** `GET /files/uploads?after=<objectKey>` →
  `{uploads: [{objectKey, contentLength, isMultipart, createdAt, expiresAt}], hasMore}`
  (times in µs; your live Drive upload reservations; ordered by `objectKey`,
  up to 1 000 per page; pass the last key as `after` for the next page; 500
  requests/min). Reservations held by copies aren't listed: the copy manages
  them, and resume/cancel on their keys answer 404. Use it to find and cancel
  reservations you lost track of, e.g. after a lost upload-URL response or a
  crash before you saved the key; otherwise they hold quota for up to 14
  days. Another device's in-progress uploads are listed too.
- An upload whose quota was reserved at start isn't blocked at create by other
  in-flight Drive reservations. It's still rejected with **426** if committed
  usage plus the file exceeds the quota (plus a 50 MiB tolerance and any
  bonus), e.g. because the user uploaded to Photos meanwhile. Admission is
  per object: if only the file was reserved, the thumbnail gets the normal
  check. An upload rejected at create with **426** is kept: let the user
  upgrade, then retry `POST /files` with the same object key (within the
  upload's expiry window).
- `POST /files` is safe to retry with the same body after a lost response:
  it returns the file that was already created (also when the account is now
  over quota).
- Uploads through a public link into a Drive folder don't reserve the
  owner's quota; they're checked at start and again at create.
- An upload rejected at create with **413** (too large) is deleted within
  minutes. Start over (with a smaller file). Uploads through a public link
  aren't deleted early; they expire normally.

### 6.4 Resuming and aborting

Multipart uploads can be resumed:

- **Persist across restarts:** `objectKey`, `completeURL`, `partLength`,
  `contentLength`, the ETag of each finished part, and what you need to
  reproduce identical ciphertext (file key, stream header, and either the
  encrypted temp file or a way to re-encrypt deterministically from chunk 0
  while skipping finished parts).
- **Resume** after a restart, or on an S3 **403** ("Request has expired"):
  `POST /files/multipart-upload-url/resume` `{objectKey}` →
  `{completedParts: [{partNumber, eTag, size}], partURLs: {"<n>": url}, completeURL}`.
  Trust `completedParts` from the server over your local ETags. New part URLs
  aren't MD5-signed.
  - `{completed: true}`: S3 already assembled the object (for example, your
    `CompleteMultipartUpload` succeeded but the response was lost). Go
    straight to `POST /files`. Don't re-upload.
  - **410** `UPLOAD_GONE`: the upload no longer exists (cancelled, expired,
    already committed, or S3 has neither the upload nor a correctly sized
    object), also when it was cancelled or committed while the resume ran,
    or when the upload made no progress and has less than about an hour left
    before it expires (the server then cancels it). Start over.
  - **409** `UPLOAD_BUSY`: another request is working on the same upload (a
    concurrent resume or cancel, a `POST /files` in progress, or the cleanup
    job). Retry shortly; don't start over.
  - **404**: the object key isn't yours. **400**: not a Drive multipart
    upload, or it was started before the server supported resume
    (`upload is not resumable`).
  - Parts S3 lists with the wrong size are treated as missing: they get a new
    URL, and re-uploading overwrites them.
  - ⚠️ `CompleteMultipartUpload` for thousands of parts can take a long time.
    Use a generous timeout, and on a timeout or connection loss call resume
    instead of assuming it failed.
- **Stay alive:** a resume extends the deadline only if more parts are done
  than at the previous resume. Part URLs from a resume never outlive the
  upload: they're valid for up to 7 days, but no longer than the upload's
  deadline minus an hour (at least 5 minutes, if that much is left). When one
  expires (S3 **403**), resume again: the parts you finished meanwhile count
  as progress and extend the deadline. Hard cap: 90 days after the upload started
  (5 000 GiB at 20 Mbit/s takes about 25 days). A stalled upload expires,
  about 14 days after its last progress-showing resume.
- **Cancel:** `DELETE /files/multipart-upload?objectKey=`. Works for any of
  your Drive uploads, including single PUTs and thumbnails.
  Frees the quota reservation at once; the server cleans up the S3 upload or
  object within minutes. 410 means it was already gone, which you can treat as
  success; 409 means retry.

### 6.5 Client resource gotchas (found in existing clients)

- ⚠️ **Web:** the Photos web uploader keeps every encrypted part in memory
  when it computes part MD5s up front. Only the "deferred checksum" path
  streams, and it's gated to internal builds. For Drive, stream: encrypt → PUT
  part by part, without `partMd5s` (or compute them per part just before each
  PUT using a resume-style flow).
- ⚠️ **Mobile:** the Photos app encrypts to a temp file first, which needs free
  space equal to the file size again, and it deletes stale temp files after a
  day of inactivity, which kills resumability. It also treats an expired-URL
  403 as a generic error. Don't copy those choices for Drive.
- Use part sizes that scale with the file. Photos mobile's fixed 20 MiB parts
  top out at about 195 GiB.

### 6.6 Downloading

- `GET /files/download/v3/:fileID` **[existing]** → `{url}` (signed S3 GET,
  valid 7 days). Missing objects return **400** with code `NotFoundError`
  (v3 uses 404 to mean the endpoint itself is unavailable). Same for
  thumbnails: `GET /files/thumbnail/v3/:fileID`.
- ⚠️ Avoid `GET /files/download/:fileID` (307 redirect). Some HTTP stacks
  forward your `X-Auth-Token` to the storage provider on the cross-origin
  redirect; the server logs this as a critical issue.
- The signed URL accepts `Range` requests, so parallel or resumed downloads
  work. But decryption is sequential (§3): download chunk-aligned ranges
  (4 MiB + 17 bytes after the header) in parallel if you like, and decrypt
  them in order.
- Downloaded responses are `Content-Disposition: attachment`,
  `application/octet-stream`. Infer the type from your encrypted metadata.
- ⚠️ **CLI gotcha:** requests whose `User-Agent` contains `go-resty` (the
  Ente CLI's HTTP library) get a URL for the Wasabi replica instead of the
  hot bucket, when a replica exists (`isCliRequest`,
  `pkg/controller/file.go` ~553). A Go CLI using resty inherits this. It's
  harmless, but it explains differences in download speed and URLs.

### 6.7 Copying files shared with you

- `POST /files/copy` **[existing]** = "save my own copy of files someone shared
  with me". The server rejects it for files you own.
- Large files (over about 5 GiB) are copied in parts. For Drive, size and
  quota are checked for the whole batch before anything is copied (413 / 426 /
  503 `QUOTA_CHECK_BUSY`).
- ⚠️ A synchronous batch isn't atomic: if one file fails, files already
  created stay and the request returns an error, so a blind retry duplicates
  them. Re-sync the destination folder before retrying. A large synchronous
  copy can also be cut off by proxy timeouts (60–100 s), and a server crash
  mid-copy keeps its quota reservation for up to 14 days; use async copy
  (below) instead.
- **Use async copy for every Drive copy.** Send
  `POST /files/copy?async=true` (exactly `true`) with the usual body plus
  `"requestID": "<1–64 chars, client-chosen>"` in the JSON body.
  - Before answering, the server runs the same checks as a synchronous copy
    and reserves quota for the whole batch, so errors come back at once
    (400 / 403 / 404 / 413 / 426 / 402 / 503 `QUOTA_CHECK_BUSY`, and 429
    `TOO_MANY_COPY_JOBS` when you already have 10 unfinished jobs: wait for
    one to finish, then retry; tell it apart from the rate-limit 429 by its
    `code`). On success
    it answers `202 {"jobID": <int>}`. The reservation holds your quota while
    the job waits.
  - Poll `GET /files/copy/:jobID` (e.g. every 2 s, backing off to ~30 s) →
    `{jobID, status, oldToNewFileIDMap?, error?: {code, message}}`. `status` is
    `pending`, `running`, `completed` or `failed`; it can go from `running`
    back to `pending` (server restart, or a transient error retried with
    backoff), so don't treat it as monotonic. 404 `NOT_FOUND` means not
    found, not yours, or deleted after retention.
  - `oldToNewFileIDMap` (source file ID → new file ID) lists exactly what was
    copied, **also on failure**: the batch isn't atomic.
  - **Idempotency:** the same `requestID` always returns the same job,
    whatever the rest of the body says (and even at the job cap), as long as
    the request is well-formed (non-empty `files`, `requestID` 1–64 chars). Reuse it only to retry the
    **enqueue** (lost 202, network error). After a `failed` job, copy the files
    missing from the map under a **new** `requestID`. Finished jobs are kept
    7 days; after that the same `requestID` creates a new job.
  - Job error codes (in the GET body, not HTTP statuses):

    | Code | Meaning | What to do |
    |---|---|---|
    | `PERMISSION_DENIED` | Access to the source or destination folder was lost | Don't retry; re-sync |
    | `NOT_FOUND` | A source file left the shared folder or was deleted | Don't retry; re-sync |
    | `COPY_SOURCE_CHANGED` | A source file's content changed after the enqueue | Re-sync, then copy again with a new `requestID` |
    | `STORAGE_LIMIT_EXCEEDED`, `NO_ACTIVE_SUBSCRIPTION`, `FILE_TOO_LARGE` | As the HTTP equivalents (rare at job time) | Show to the user |
    | `CROSS_APP_FILE`, `BAD_REQUEST` | Invalid request | Don't retry |
    | `COPY_RESERVATION_LOST` | The quota reservation expired (job queued for ~14 days) | Copy the missing files with a new `requestID` |
    | `COLLECTION_DELETED` | The destination folder was deleted during the copy | Re-sync; copy the missing files into a live folder with a new `requestID` |
    | `COPY_ATTEMPTS_EXCEEDED` | Transient failures persisted after several retries | Copy the missing files with a new `requestID`, with backoff |
    | `INTERNAL_ERROR` | The job crashed (not retried) | Same, with backoff |
  - Rate limits: 60 async enqueues and 200 polls per minute per user (429).
  - ⚠️ **Older servers** (and pods still on the old binary during a rolling
    deploy) ignore `async` and `requestID`: they copy synchronously and answer
    `200 {oldToNewFileIDMap}`. Treat a 200 as a finished copy. A poll that hits
    an old pod gets a plain 404; retry it for a short while after enqueueing.
  - A large multipart copy interrupted by a server restart starts again from
    the beginning of that file.

---

## 7. Sharing

### 7.1 Folder sharing **[existing]**

- Share a folder with another Ente user by sealing the folder key to their
  public key: `/collections/share` (one user), `/collections/share/batch`
  (one folder, ≤ 10 users), `/collections/share/bulk` (one user, ≤ 100
  folders; **not atomic**, so check the per-item results).
- Roles: **VIEWER** (read), **COLLABORATOR** (read + add own files),
  **ADMIN** (+ manage sharees), **OWNER**. Rename, magic metadata, public links
  and deleting the folder are owner-only.
- ⚠️ **Sharing a folder shares only the files directly inside it, not its
  subfolders** (D9, until plan phase 3). To share a tree today, share each
  subfolder explicitly, and remember to share subfolders created later. Make
  this visible in the UI.
- ⚠️ People you share with **don't see the folder's parent** (no `parentID`
  for non-owners in phase 2). Every folder shared with a user appears as a
  top-level folder in their "Shared with me".
- Unsharing (or the sharee leaving) also **removes the files the sharee added**
  to that folder. They still have them in their own folders.
- The folder key is **not rotated** on unshare. Revocation is enforced by the
  server, not by cryptography.

### 7.2 Contributing to a shared folder (collaborators)

1. Upload into a folder **you** own (e.g. your Drive root); see §6.1.
2. `POST /collections/add-files` to the shared folder, with the file key
   re-wrapped with the shared folder's key.

The file stays owned by, and billed to, you, and it also appears in your own
folder. Moving it within the shared tree isn't possible for anyone (moves
require owning both folders and every file).

### 7.3 Public links **[existing]**

- **Folder link:** `/collections/share-url` (expiry, device limit, password,
  allow downloads, allow uploads ("collect")). The folder key goes in the URL
  fragment, which the server never sees. A link covers **one folder only**, not
  its subfolders (subtree links are plan phase 3b).
- **Single-file link:** `/files/share-url` (expiry, device limit, password,
  download toggle). One active link per file; owner only.
- The public link host for Drive comes from server config `apps.public-drive`.

---

## 8. Things that will bite you (summary)

1. Missing `X-Client-Package` header → your data goes to Photos.
2. Names are encrypted: no server search or listing, and no name-uniqueness
   enforcement.
3. File sync is per folder; diff only folders whose `updationTime` changed.
4. Use `/collections/v2`, never `/v3`.
5. Thumbnails are mandatory: upload a placeholder and set `noThumb`.
6. Don't `add-files` your own files; move them. Trashing removes a file from
   every folder.
7. Collection magic metadata and renames are last-write-wins; file magic
   metadata returns 409 on version mismatch.
8. Deleting a folder tree is eventually consistent for the trash.
9. Sharing a folder doesn't share its subfolders (yet); sharees see shared
   folders as roots.
10. Single PUT ≤ 5 GiB; part count ≤ 10 000; size the parts accordingly. The
    cap applies to the **encrypted** size.
11. Stream uploads and downloads; never hold a whole file (or every part) in
    memory.
12. Persist multipart state and use resume, including after a lost or slow
    complete call; abort what you abandon. A quota rejection at create keeps
    the upload, so retry after the user upgrades.
13. Use `/files/download/v3`, not the redirecting download route.
14. Ciphertext supports range downloads, but decryption is sequential from
    the start.
15. No version history and no folder restore.

## 9. Not supported (by design or out of scope)

Server-side search · file version history · folder trash/restore · files
larger than 5 000 GiB · random access within an encrypted file · strong
consistency for concurrent renames/metadata edits · inherited (subtree)
sharing and subtree public links (until phase 3).

## 10. References

- libsodium secretstream (file/thumbnail encryption format, chunk framing,
  `TAG_FINAL`): https://doc.libsodium.org/secret-key_cryptography/secretstream
- libsodium secretbox (folder and file key wrapping):
  https://doc.libsodium.org/secret-key_cryptography/secretbox
- libsodium sealed boxes (sharing a folder key with another user):
  https://doc.libsodium.org/public-key_cryptography/sealed_boxes
- S3 `CompleteMultipartUpload` request body (what you POST to `completeURL`):
  https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html
- S3 `UploadPart` (what you PUT to each part URL; signed headers must match):
  https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPart.html
- HTTP range requests on the signed download URL:
  https://developer.mozilla.org/en-US/docs/Web/HTTP/Range_requests
- Existing client code worth reading before writing your own:
  - Locker mobile (generic files on this backend: placeholder thumbnail,
    `noThumb`, search): `mobile/apps/locker/lib/services/files/upload/file_upload_service.dart`,
    `mobile/apps/locker/lib/ui/mixins/search_mixin.dart`
  - Photos mobile multipart with persisted state (resume model to extend):
    `mobile/apps/photos/lib/module/upload/service/multipart.dart`,
    `mobile/apps/photos/lib/db/upload_locks_db.dart`
  - Web streaming upload (the "deferred checksum" path):
    `web/packages/gallery/services/upload/upload-service.ts`
  - Web collection and file sync: `web/packages/new/photos/services/collection.ts`

## 11. TODO: server capability check for Drive

Older museum servers map unknown client packages (including `io.ente.drive`)
to Photos. A Drive client connected to an un-upgraded server is silently
issued a Photos session, so its folders and files show up in the user's Photos
library. For now, only use the Drive client against servers that include the
Drive changes.

In a later iteration, add a Drive bit to `serverApiFlag` (e.g.
`Drive int64 = 1 << 9` in `server/ente/remotestore.go`, set in
`GetFeatureFlags`). The Drive client then calls `GET /remote-store/feature-flags`
right after login and refuses to write anything (and logs out) if the bit is
missing.
