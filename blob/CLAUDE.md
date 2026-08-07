# blob

Guidance for `github.com/go-faster/gooners/blob`. Repo-wide rules are in the root `CLAUDE.md`.

## Public API (issue #67)

`blob` is the module's **only exported package**, so every change to it is a public API change that
`gotd/tgmcp` and other outside servers depend on. Keep it that way:

- **Nothing in `blob`'s exported API may name an `internal/` type.** `blob.FS` is a blob-owned
  interface, not `effect.FS`, and `blob.Dir` returns it — an outside importer cannot import
  `internal/effect` to construct one. `effect.FS` does *not* satisfy `blob.FS` automatically: Go
  interface types are identical only when they are the same declaration, which is why `effectFS` in
  `blob/dir.go` exists. Widen `blob.FS` and that adapter must grow with it.
- **`blob` stays dependency-light** — stdlib, `go-faster/errors`, the MCP SDK, and `internal/effect`.
  Every dependency it takes lands in the go.mod of everyone who imports it.
- **A store that cannot advertise a reachable URL is `blob.Deny`, never a store minting URLs.**
  `HTTPOptions.BaseURL` is required for exactly this reason. Returning an unreachable URL is the
  plausible-wrong-answer failure the package exists to remove, so it must be an error naming the flag.
- **The object id is not an access control mechanism** — see "Reaching an object" in the package
  doc. What guards the bytes is the deployment: `blob.HTTP` on loopback or behind a firewall, or
  `blob.HTTP` behind authentication the operator put in front of it, or bucket credentials for
  `blob/s3`. Only a presigned URL is a credential in its own right.
  An id is a UUIDv4 anyway, because 122 bits of `crypto/rand` cost nothing and unguessability is a
  fine second layer — just never the boundary. Do not switch to v7: it spends most of its bits on a
  timestamp it then leaks, and its sequential keys hot-spot one S3 partition.
  Keep the rest of the hygiene regardless. Unknown / expired / malformed ids return the same 404,
  and a URL outlives the call in the transcript and the logs, so lengthening the default TTL still
  needs a reason.
- **The store serves attacker-influenced bytes on the operator's origin.** `attachment`, `nosniff`,
  and a downgrade of types a browser executes are not optional; `Range` support is, and it stays,
  because resuming a large fetch is half the point.
- **Do not add a `ReadDir` to `effect.FS` for the sweep.** The index is in memory and `objectsDir` is
  cleared at startup, which is what lets the store stay inside the existing filesystem interface.
- A tool that produces bytes calls `blob.Content`, which decides inline vs link. Do not reimplement
  that threshold in a handler, and do not put it inside a `Store`: storage is not content policy.
- **`Attach` borrows bytes; `Put` owns them.** An attached object's file belongs to whoever wrote it,
  so expiry, `Delete` and shutdown drop the reference only. A sweep that removes an attached file
  would empty the volume a gateway was serving — `object.attached` is what prevents that.

See also `internal/gateway/CLAUDE.md` for `blob_share`, the gateway tool built on `Attacher`.
