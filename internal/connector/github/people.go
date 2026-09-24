package github

import (
	"context"
	"fmt"
	"net/url"
	"slices"
)

// maxCollaboratorPages bounds the collaborator walk of one repository, at a
// hundred people a page. A repository with more than ten thousand
// collaborators is not a team `hearsay init` is seeding by hand.
const maxCollaboratorPages = 100

// Collaborator is one account with access to a repository, as the
// collaborators list names it: the login a person types and the node id that
// survives a rename.
type Collaborator struct {
	Login  string
	NodeID string
	// Bot is an account GitHub types as a bot, such as an App's.
	Bot bool
}

// Profile is what a GitHub account makes public about itself: the display
// name and the public email address, either of which may be empty.
type Profile struct {
	Login  string
	NodeID string
	Name   string
	Email  string
}

// Collaborators lists everyone with access to a repository — outside
// collaborators, and organization members through the organization's and
// their teams' permissions — in the order GitHub pages them. It is what
// `hearsay init` seeds principals from, and nothing else calls it. The token
// needs read access to the repository's Metadata.
func (r *Reader) Collaborators(ctx context.Context, repo string) ([]Collaborator, error) {
	if !slices.Contains(r.repos, repo) {
		return nil, fmt.Errorf("repository %s is not one the source names", repo)
	}
	var out []Collaborator
	for page := 1; page <= maxCollaboratorPages; page++ {
		var users []struct {
			Login  string `json:"login"`
			NodeID string `json:"node_id"`
			Type   string `json:"type"`
		}
		more, err := r.api.get(ctx, fmt.Sprintf("%s/collaborators?per_page=100&page=%d", repoPath(repo), page), &users)
		if err != nil {
			return nil, fmt.Errorf("listing the collaborators of %s: %w", repo, err)
		}
		for _, u := range users {
			out = append(out, Collaborator{Login: u.Login, NodeID: u.NodeID, Bot: u.Type == "Bot"})
		}
		if !more {
			return out, nil
		}
	}
	return nil, fmt.Errorf("listing the collaborators of %s: more than %d pages", repo, maxCollaboratorPages)
}

// Profile reads one account's public profile.
func (r *Reader) Profile(ctx context.Context, login string) (Profile, error) {
	var u struct {
		Login  string `json:"login"`
		NodeID string `json:"node_id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
	}
	if _, err := r.api.get(ctx, "/users/"+url.PathEscape(login), &u); err != nil {
		return Profile{}, fmt.Errorf("reading the profile of %s: %w", login, err)
	}
	return Profile(u), nil
}

// SocialAccounts returns the URLs of the accounts elsewhere that a GitHub
// account lists on its profile, as its owner wrote them.
func (r *Reader) SocialAccounts(ctx context.Context, login string) ([]string, error) {
	var accounts []struct {
		URL string `json:"url"`
	}
	if _, err := r.api.get(ctx, "/users/"+url.PathEscape(login)+"/social_accounts?per_page=100", &accounts); err != nil {
		return nil, fmt.Errorf("reading the social accounts of %s: %w", login, err)
	}
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.URL)
	}
	return out, nil
}
