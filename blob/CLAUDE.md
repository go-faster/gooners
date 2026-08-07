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
- **The object id is the only credential.** 128 bits from `crypto/rand`, short TTL, and unknown /
  expired / malformed ids all return the same 404 — distinguishing them is an oracle. A URL also
  outlives the call in the transcript and the logs, so lengthening the default TTL needs a reason.
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
