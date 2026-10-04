# Ente Drive — client plan (web app)

> **Implementation status** (kept up to date by the orchestrator)
>
> - **Current:** Stage 4 implemented (in review); Stage 5 fixed (verification
>   review). Stages 4 and 5 will be committed together (shared files).
> - **Done (committed):** Stages 1+2 (a9ca80416b).
> - Work happens on branch `drive-desktop`, one commit per stage (never
>   pushed). The owner reviews each stage commit.

Companion to [`drive-plan.md`](./drive-plan.md) (server) and
[`drive-client-guide.md`](./drive-client-guide.md) (the client contract, which
is authoritative for every API interaction). Scope: the Drive **web app**
(like Locker web). Mobile comes later.

> **Direction change (owner, during Stage 5):** "Remove Drive's Tauri shell and
> keep it as just a web-app, like Locker." The Tauri shell (`rust/apps/drive`),
> the native Rust transfer engine (`rust/crates/drive`, Stage 3) and its
> `ente-core` additions were deleted (owner chose "delete", not "keep on a side
> branch"), together with the desktop bridge in `ente-base`/`ente-accounts`.
> Decisions C1, C3, C4, C6, C7 and C13 below were revised accordingly; the
> Stage 3 reviews' protocol lessons carry over to the browser transfer layer.

---

## Orchestration

Instructions from the project owner to the orchestrating agent:

> Act as an orchestrator for the CLIENT APPS part of this effort. Use the same
> strategy and guidelines as the backend orchestrator, and split the work
> across subagents. For the UI, take inspiration from existing Ente apps and
> Google Drive UX. Work autonomously to deliver a full desktop client app
> (mobile will come later). Be proactive when taking decisions to improve the
> final product.

How this works in practice (same as the backend, adapted to autonomy):

1. **Per stage:** one implementation agent → at least two adversarial review
   agents with different focuses → the orchestrator verifies each finding
   against the code → a fix agent applies the confirmed ones → the
   orchestrator runs the checks and commits the stage on `drive-desktop`.
   Independent stages may run in parallel when they touch disjoint
   directories.
2. **Compatibility guideline** (included in every prompt; reviewers check it):
   - Never break or change the behaviour of existing apps: Photos (web,
     desktop/Electron, mobile), Locker, Auth, Ensu, Accounts, Share, Paste,
     Cast, Space. Edits to shared packages (`web/packages/*`, `rust/crates/*`,
     `rust/bindings/*`) must be additive and behaviour-preserving for every
     existing consumer; say why when that's impossible.
   - **No changes under `server/`.** A server gap is reported to the
     orchestrator, who records it in "Server follow-ups" below.
   - The whole repo must keep passing its existing checks (web lint/tsc/tests,
     `cargo fmt/clippy/test` with `-D warnings`).
3. **Comments rule** (backend §0 rule 9): keep comments to a minimum; prefer
   self-documenting code; comment only a non-obvious *why*. No doc comments
   that restate a signature, no per-case narration in tests.
4. Agents must not edit this file or the server docs; the orchestrator
   updates the status block.

Reference notes gathered for agents (orchestrator scratchpad, mirrored into
prompts): museum API reference verified against code, Locker web
architecture, desktop shells/Rust crates, Ente design system.

---

## Decisions

| # | Decision | Choice |
|---|---|---|
| C1 | Platform | **Web app only**, like Locker (revised: originally a Tauri desktop shell). No desktop wrapper for now. |
| C2 | UI | Next.js static export at `web/apps/drive`, MUI + Ente theme, reusing `ente-base` / `ente-accounts`. Deployed like the other Ente web apps. |
| C3 | Logic split | Everything in **TypeScript** (`web/apps/drive/src/services`): auth/session, sync engine, local model, operations, sharing and transfers — crypto via Rust WASM (`packages/wasm/drive` ← `rust/bindings/wasm/drive`), Locker-style. Transfers run in a Web Worker so encryption stays off the main thread. |
| C4 | App identity | `X-Client-Package: io.ente.drive.web`. Dev port 3014. Every request (incl. login) carries it; a test asserts it. |
| C5 | Server capability | After login and before any write, probe `GET /files/uploads`: a plain-text 404 means the server predates Drive (guide §11) → explain and log out. Replace with the `serverApiFlag` bit when the server adds it. |
| C6 | Uploads (browser) | The transfer worker owns a whole upload: generates the file key, streams secretstream encryption (WASM) from `File.slice`, uploads, commits `POST /files` (idempotent retry with the same object keys and pinned ciphertext), and hands the created file to `ingestRemoteFiles`. Placeholder thumbnail + `noThumb` (real thumbnails later). On `COLLECTION_DELETED` the task waits for a new target. |
| C7 | Streaming & resume (browser) | Never buffer whole files. Parts are a whole number of encrypted 4 MiB chunks, ≤ 10 000 parts; an encrypted part is kept in memory only until its PUT succeeds (memory bounded by part size × parallel parts), so retries never re-encrypt with a reused stream state. Within a session: retries, expired-URL resume via `/files/multipart-upload-url/resume` (trust `completedParts`; `{completed:true}`, 410, 409). Across reloads the browser can't re-read the file, so unfinished uploads are cancelled (`DELETE /files/multipart-upload`) and listed for the user to re-add; orphan reservations are reconciled via `GET /files/uploads`. Downloads stream-decrypt to a file (File System Access API where available, otherwise a fallback) with sequential decryption. |
| C8 | Metadata | Encrypted metadata JSON `{title, fileType, creationTime, modificationTime, hash?, mimeType?}`; times in **µs** (server convention); `fileType` 3 = generic file (Locker's value), 0/1 image/video when detected. `hash` = BLAKE2b of the plaintext computed while streaming (client-side dedupe). Renames: `editedName` in pubMagicMetadata with 409 refetch-merge-retry. Placeholder thumbnail + `noThumb: true` unless a real thumbnail was generated. |
| C9 | Local state | IndexedDB (encrypted records only, Locker pattern), hydrated on start for an instant UI; incremental sync on start, on window focus and every 30 s while visible. File diffs only for folders whose `updationTime` changed. |
| C10 | Folder tree | Server `parentID`; verify every owned child's `parentEncryptedKey` against the claimed parent (guide §5.1) — mismatch → show at root with a warning badge; repair by moving to the same parent. Orphans (parent deleted/unknown) at root. Files at root live in Drive `uncategorized` (created on first use). One owned folder per file (move, never add). Name conflicts resolved client-side ("name (1).ext"). |
| C11 | Deletes | Folder delete via v4 only, one at a time, ≥ 60 s timeout, backoff on 503; UX maps `HAS_CHILDREN`/`COLLECTION_NOT_EMPTY` into a single "delete folder and contents" confirmation (recursive + keepFiles=false), `SUBTREE_TOO_LARGE` → explanatory error. Show a "deleting…" state while files trickle into trash. |
| C12 | Sharing v1 | Per folder; roles Viewer/Collaborator/Admin. UI states clearly that subfolders aren't included (D9). "Shared with me" lists shared folders as roots. Collaborators contribute by uploading to their own root then `add-files`. Public links for folders and single files; copies of shared files via async `POST /files/copy`. |
| C13 | Login | `ente-accounts` pages reused exactly as Locker does (standard web passkey redirect). |
| C14 | UX direction | Google Drive information architecture (left nav with **New** button, My Drive tree, Shared with me, Recent, Starred, Trash, storage meter; search bar; breadcrumbs; list/grid toggle; multi-select; context menus; drag-to-move; details panel; bottom-right transfers panel) rendered in Ente's visual language (Locker/share palette with the blue accent, Inter, rounded surfaces, Hugeicons, `ente-base` dialogs/menus/notifications). Light/dark/system. |
| C15 | Starred | Private magic metadata key `starred` (files: versioned file magic metadata; folders: collection private magic metadata, last-write-wins). |

---

## Stages

| Stage | Scope | Depends on | Status |
|---|---|---|---|
| 1 | Foundations: `web/apps/drive` scaffold + registration (app name, client package, theme, redirect, auth pages, i18n), Drive WASM package, capability probe, session service, dev docs. (Tauri shell and desktop bridge removed by the direction change.) | – | Committed (a9ca80416b) |
| 2 | TS data layer: sync engine (collections/diff/trash), IndexedDB store, decryption, model (tree, verification, orphans, shared, root), operations (folders create/rename/move/delete v4; files move/rename/trash; trash restore/delete/empty; starred), error mapping, name conflicts | 1 | Committed (a9ca80416b) |
| 3 | ~~Rust transfer engine `ente-drive`~~ — built and reviewed, then deleted by the direction change. | – | Dropped |
| 4 | Browser transfers: transfer worker (streaming encrypt/upload, multipart, resume within session, commit, ingest), upload queue (files and folder trees via picker and drag-drop), downloads (streaming decrypt; files and folders), transfers panel, reload handling | 2 | In review |
| 5 | Main UI: app shell, navigation, list/grid views, sorting, selection, context menus, keyboard shortcuts, drag-to-move, dialogs (new folder, rename, move picker, delete), details panel, transfers panel, trash view, search, settings drawer (account, storage, theme, language, logout), empty states | 2 (contract from 4) | Fixed; verification review |
| 6 | Sharing: share dialog (users, roles), manage/leave, Shared with me, contribute to shared folders, public links (folder and file), save a copy (async copy jobs) | 4, 5 | – |
| 7 | Previews & polish: previews (images, PDF, text, audio/video), real image thumbnails, onboarding/empty states, accessibility, deployment (Dockerfile/web-deploy entries), icons and login art, user-facing docs | 6 | – |
| 8 | Full audit: adversarial reviews (crypto/security, protocol vs client guide, existing-app compatibility, UX, simplification), fix groups, end-to-end smoke test against a local museum | 7 | – |

---

## Deferred items (carried into later stages)

- **Stage 7:** a Drive login illustration (Stage 1 reuses Locker's auth art);
  deployment wiring (`web/Dockerfile`, `web-deploy.yml`); Hugeicons throughout.
- **Stage 8:** CSP/headers for the deployed web app (`_headers` exists;
  previews must be sandboxed).

## Server follow-ups (found by the client work; not implemented here)

- Drive capability bit in `serverApiFlag` (guide §11); the client uses the
  `GET /files/uploads` probe meanwhile (C5).
- App mismatch on `POST /files`, `add-files`, `move-files` returns 500 `{}`
  (`ErrInvalidApp` unmapped); should be a typed 4xx.
- `/files/share-url` requires `app` in the body — undocumented in the guide.
- `POST /files` HEADs both objects before its duplicate check, so an expired
  reservation looks like a transient 503 `OBJECT_SIZE_FETCH_FAILED` and can't
  be told apart from "already committed, storage flaky".
- The `POST /files` duplicate check ignores `collectionID` and echoes the
  request's collection.
- Upload-URL requests take no idempotency key, so a lost response leaks a
  reservation the client can't reliably identify.
- Single-PUT and thumbnail reservations have no keep-alive/extension.
  (These `POST /files`/reservation findings came from the deleted Stage 3
  engine's reviews; they still apply to any Drive client.)
- `updation_time` is taken before the transaction commits, so two commits can
  land out of timestamp order and a client cursor can skip the later one
  (affects every client; Drive re-requests with a small overlap).
- Upload-URL routes are limited to 500/min per user and Drive has no batch
  route; with the mandatory placeholder thumbnail every file costs two calls,
  capping small-file uploads at ~250 files/min. Options: a batch upload-URL
  route for Drive, and optional thumbnails for Drive files.
- A repeated `POST /files` echoes the request's `encryptedKey` and
  `pubMagicMetadata` instead of the stored row.
- `GET /collections/:id` doesn't return the caller's sharee magic metadata.
- `move-files` doesn't check that the files are in `fromCollectionID`;
  restore of a stale trash batch returns an untyped `400 {}`.
