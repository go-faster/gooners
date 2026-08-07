// Package blobutil holds the naming and identity rules every blob store
// shares.
//
// They live here rather than in one store because they are security-relevant
// and there is now more than one store: [CleanName] is what keeps a
// caller-supplied name from breaking out of a Content-Disposition header, and
// [NewID] is what makes an object id unguessable. A second copy of either is a
// second chance to get it wrong in only one of them.
package blobutil

import (
	"mime"
	"path"
	"strings"

	"github.com/go-faster/errors"
	"github.com/google/uuid"
)

// NewID returns an object id.
//
// It is a random (version 4) UUID, so it is unique without coordinating with
// anything — which matters once several servers write into one bucket. Access
// control is not its job: see the "Reaching an object" section of the blob
// package documentation for what actually guards the bytes.
//
// Version 4 rather than 7 on two counts. Its 122 bits come from crypto/rand,
// so an id stays unguessable as a free second layer, where v7 spends most of
// its bits on a timestamp it then leaks. And v7 ids sort by creation time,
// which concentrates writes on one S3 partition; random ones spread over the
// keyspace, which is what object stores want from a prefix.
func NewID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", errors.Wrap(err, "generate blob id")
	}
	return id.String(), nil
}

// CleanName reduces a caller-supplied name to a base name safe to put in a URL
// path and a Content-Disposition header. fallback is used when nothing usable
// is left.
func CleanName(name, fallback string) string {
	// Reduce to a base name first, so a path only loses its directories rather
	// than being flattened into one long name.
	name = path.Base(strings.TrimSpace(strings.ReplaceAll(name, `\`, "/")))
	// What is left cannot break out of a quoted Content-Disposition or inject
	// a header.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return fallback + ".bin"
	}
	return name
}

// ContentType picks the declared type, guessing from the extension when the
// caller had none.
func ContentType(declared, name string) string {
	if declared != "" {
		return declared
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// ServeType downgrades the types a browser would execute on the origin serving
// them. The declared type still reaches the agent on the ResourceLink, which is
// where it is useful; what goes on the wire is only what a fetcher needs.
func ServeType(t string) string {
	base, _, err := mime.ParseMediaType(t)
	if err != nil {
		return "application/octet-stream"
	}
	switch base {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/xml", "text/xml":
		return "application/octet-stream"
	}
	return t
}

// ContentDisposition renders the header that makes a fetcher save the bytes
// rather than display them.
func ContentDisposition(name string) string {
	// name is already stripped of quotes, backslashes and control characters,
	// so the quoted form cannot be broken out of. The RFC 5987 form carries
	// non-ASCII names that the quoted one cannot.
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
