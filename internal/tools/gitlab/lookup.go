package gitlab

import (
	"context"
	"strings"

	"github.com/go-faster/errors"
	gl "gitlab.com/gitlab-org/api/client-go/v2"
)

// splitCSV parses a comma-separated argument, dropping blanks and surrounding
// space so "bug, ux" and "bug,ux" mean the same thing.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// userIDs resolves usernames to the numeric IDs the API wants for assignment.
// The tools take usernames throughout because that is what an agent reading an
// issue or a merge request actually has.
func (c *Client) userIDs(ctx context.Context, usernames []string) ([]int64, error) {
	ids := make([]int64, 0, len(usernames))
	for _, name := range usernames {
		name = strings.TrimPrefix(strings.TrimSpace(name), "@")
		if name == "" {
			continue
		}
		users, _, err := c.gl.Users.ListUsers(&gl.ListUsersOptions{
			Username: new(name),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrapf(err, "look up user %q", name)
		}
		if len(users) == 0 {
			return nil, errors.Errorf("no such user: %q", name)
		}
		ids = append(ids, users[0].ID)
	}
	return ids, nil
}

// milestoneID resolves a milestone title within a project. Titles are what
// appear in the UI and in an issue's JSON; IDs are not.
func (c *Client) milestoneID(ctx context.Context, pid, title string) (int64, error) {
	milestones, _, err := c.gl.Milestones.ListMilestones(pid, &gl.ListMilestonesOptions{
		Title:            new(title),
		IncludeAncestors: new(true),
	}, gl.WithContext(ctx))
	if err != nil {
		return 0, errors.Wrapf(err, "look up milestone %q in %s", title, pid)
	}
	if len(milestones) == 0 {
		return 0, errors.Errorf("no such milestone in %s: %q", pid, title)
	}
	return milestones[0].ID, nil
}
