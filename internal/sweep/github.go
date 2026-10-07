package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type githubRepository struct {
	ID            int64  `json:"id"`
	Path          string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
}

type githubAPIError struct {
	status int
	empty  bool
}

func (e *githubAPIError) Error() string { return fmt.Sprintf("GitHub API returned HTTP %d", e.status) }

func githubGet(ctx context.Context, c Connection, path string, value any) error {
	root := c.ServerURL + "/api/v3"
	if c.ServerURL == "https://github.com" {
		root = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/"+path, nil)
	if err != nil {
		return errors.New("invalid GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := apiClient()
	resp, err := apiDo(ctx, client, req, "GitHub")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		apiErr := &githubAPIError{status: resp.StatusCode}
		if resp.StatusCode == http.StatusConflict {
			var body struct {
				Message string `json:"message"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil {
				apiErr.empty = body.Message == "Git Repository is empty."
			}
		}
		return apiErr
	}
	if err := json.NewDecoder(resp.Body).Decode(value); err != nil {
		return errors.New("invalid GitHub API response")
	}
	return nil
}

func discoverGitHub(ctx context.Context, c Connection) ([]Repository, []Outcome) {
	var repos []Repository
	var failures []Outcome
	excluded := map[string]bool{}
	for _, p := range c.Exclude.Repositories {
		excluded[p] = true
	}
	seenPaths := map[string]bool{}
	seenIDs := map[int64]bool{}
	for _, path := range c.Include.Repositories {
		if seenPaths[path] || excluded[path] {
			continue
		}
		seenPaths[path] = true
		repo := Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: path}
		fail := func(err error) {
			failures = append(failures, Outcome{Repository: repo, Outcome: "failed", Error: err.Error(), CheckedAt: time.Now().UTC()})
		}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			fail(errors.New("GitHub repositories must use owner/repository paths"))
			continue
		}
		endpoint := "repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
		var p githubRepository
		if err := githubGet(ctx, c, endpoint, &p); err != nil {
			fail(err)
			continue
		}
		if p.ID <= 0 || p.Path == "" {
			fail(errors.New("invalid GitHub repository identity"))
			continue
		}
		if seenIDs[p.ID] || excluded[p.Path] || (p.Archived && !c.IncludeArchived) || (p.Fork && !c.IncludeForks) {
			continue
		}
		seenIDs[p.ID] = true
		repo.ID = strconv.FormatInt(p.ID, 10)
		repo.Path = p.Path
		repo.DefaultBranch = p.DefaultBranch
		repo.CloneURL = p.CloneURL
		if repo.DefaultBranch == "" {
			fail(errors.New("repository has no default branch and is not confirmed empty"))
			continue
		}
		var commit struct {
			SHA string `json:"sha"`
		}
		if err := githubGet(ctx, c, endpoint+"/commits/"+url.PathEscape(repo.DefaultBranch), &commit); err != nil {
			var apiErr *githubAPIError
			if errors.As(err, &apiErr) && apiErr.empty {
				repo.Empty = true
				repos = append(repos, repo)
			} else {
				fail(err)
			}
			continue
		}
		if !regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`).MatchString(commit.SHA) {
			fail(errors.New("invalid default-branch commit"))
			continue
		}
		repo.Revision = commit.SHA
		repos = append(repos, repo)
	}
	return repos, failures
}
