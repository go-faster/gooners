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

## `blob/s3` (issue #75)

A bucket-backed `Attacher`, for a deployment whose servers are not on one machine. It is a
subpackage rather than part of `blob` so that importers using `blob.HTTP` do not link minio-go.

- **`Open` is the ingest half of the package, not an implementation detail.** It is how one server
  consumes what another wrote: tgmcp stores a file, the agent passes the **id**, ssh-mcp calls
  `Open`. Two consumers, two references — the agent gets the presigned URL because it has no bucket
  credentials, another server gets the id because it does. Never build ingest as "fetch a URL the
  agent supplied": that is request forgery with extra steps, and it also breaks when the presign
  expires mid-transfer, which async uploads make likely.
- **The prefix is the tenant; the namespace is the server.** Reads span namespaces deliberately and
  cannot span prefixes at all. Do not widen a prefix to make two users' servers see each other.
- **`ParseID` is the gate between a model-supplied string and a bucket key.** An id arrives in tool
  *output*, so it is attacker-influenced; loosening that regex is a tenancy change, not a parsing
  change.
- **One tenant per process**, which is what these servers are anyway. A process serving several
  users derives a store per tenant with `WithTenant` rather than sharing one — see issue #71, where
  session-scoped state would move the boundary from the process to the session.
- **`Attach` uploads.** A bucket cannot serve bytes that are only on someone's disk, so unlike
  `blob.HTTP.Attach` this is a real copy; `MaxSize` is what bounds it. It is what makes an
  uncontrolled upstream usable from another machine.
- **The instance metadata service is not in the default credential chain.** It is a link-local
  address the egress policy blocks by default, and a store reaching for it silently is the thing
  that policy exists to prevent. An operator on an instance role passes `credentials.NewIAM("")`.
- S3 cannot set `nosniff` on a GET, so the stored `Content-Type` is downgraded at *put* time and
  `Content-Disposition: attachment` carries the rest. Do not drop either.
- Object lifetime is the bucket's job (a lifecycle rule); `ExpiresAt` is when the **URL** stops
  working. There is no sweep, and adding one would mean listing a bucket shared with every other
  server.

See also `internal/gateway/CLAUDE.md` for `blob_share`, the gateway tool built on `Attacher`.
