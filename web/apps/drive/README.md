# Ente Drive

An end-to-end encrypted file manager on Ente's museum backend, served as a web app (like Locker). Every request it makes carries `X-Client-Package: io.ente.drive.web`, which museum uses to keep Drive's data apart from Photos.

## Development

Run against a local museum (from the `web` directory):

```sh
NEXT_PUBLIC_ENTE_ENDPOINT=http://localhost:8080 npm run dev:drive
```

This builds the WASM packages and serves the app on `http://localhost:3014`. Without the variable, the app talks to Ente's production servers. Tapping 7 times on an empty area of the login page also opens a dialog to pick a custom server.

## Build and test

```sh
npm run build:drive
npm run test --workspace drive
```

Building needs a Rust toolchain with the `wasm32-unknown-unknown` target, since `ente-drive-wasm` is built by `wasm-pack` from `rust/bindings/wasm/drive`.

## Architecture

- `src/services` is the data layer: it syncs Drive collections and files from museum into IndexedDB (names and metadata stay encrypted there), and decrypts them in memory with `ente-drive-wasm`. `services/drive-api.ts` is its interface to the UI.
- `src/components` is the UI, built on the shared Ente MUI theme and components.
- Account flows (signup, login, two-factor, passkeys) reuse the pages from `ente-accounts`.
- `src/services/transfers` is the contract for uploads and downloads that the UI renders.
