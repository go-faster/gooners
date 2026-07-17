package gitlab

import (
	"time"

	gl "gitlab.com/gitlab-org/api/client-go/v2"
)

// defaultPerPage keeps a list call's result small enough to read. GitLab's own
// default is 20; the tools stay there rather than dumping a page of 100 into
// the context.
const defaultPerPage = 20

// maxPerPage is GitLab's server-side ceiling. Asking for more is silently
// clamped by the API, so the tools clamp it themselves and say so.
const maxPerPage = 100

// ListArgs is the pagination every list tool accepts.
type ListArgs struct {
	Page    int `json:"page,omitempty" jsonschema:"1-based page number; defaults to 1"`
	PerPage int `json:"per_page,omitempty" jsonschema:"results per page, 1-100; defaults to 20"`
}

// listOptions converts to the client's pagination, applying defaults.
func (a ListArgs) listOptions() gl.ListOptions {
	perPage := a.PerPage
	switch {
	case perPage <= 0:
		perPage = defaultPerPage
	case perPage > maxPerPage:
		perPage = maxPerPage
	}
	page := a.Page
	if page <= 0 {
		page = 1
	}
	return gl.ListOptions{Page: int64(page), PerPage: int64(perPage)}
}

// UserSummary identifies a user without the avatar URLs, web URLs and IDs that
// the full API representation carries.
type UserSummary struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
}

func userSummary(u *gl.BasicUser) *UserSummary {
	if u == nil || u.Username == "" {
		return nil
	}
	return &UserSummary{Username: u.Username, Name: u.Name}
}

func userSummaries(users []*gl.BasicUser) []UserSummary {
	out := make([]UserSummary, 0, len(users))
	for _, u := range users {
		if s := userSummary(u); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// NoteSummary is a comment on an issue or merge request.
type NoteSummary struct {
	ID        int64        `json:"id"`
	Author    *UserSummary `json:"author,omitempty"`
	Body      string       `json:"body"`
	System    bool         `json:"system,omitempty" jsonschema:"true for GitLab-generated activity notes rather than human comments"`
	CreatedAt *time.Time   `json:"created_at,omitempty"`
}

func noteSummary(n *gl.Note) NoteSummary {
	return NoteSummary{
		ID:        n.ID,
		Author:    &UserSummary{Username: n.Author.Username, Name: n.Author.Name},
		Body:      n.Body,
		System:    n.System,
		CreatedAt: n.CreatedAt,
	}
}

// truncate caps a body at limit runes, marking that it did. Descriptions and
// diffs are the two places a single API response can dwarf everything else in
// the context.
func truncate(s string, limit int) (string, bool) {
	r := []rune(s)
	if len(r) <= limit {
		return s, false
	}
	return string(r[:limit]), true
}
