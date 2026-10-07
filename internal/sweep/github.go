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

func githubAPIRoot(c Connection) string {
	root := c.ServerURL + "/api/v3"
	if c.ServerURL == "https://github.com" {
		root = "https://api.github.com"
	}
	return root
}

func githubGet(ctx context.Context, c Connection, path string, value any) error {
	_, err := githubGetURL(ctx, c, githubAPIRoot(c)+"/"+path, value)
	return err
}

func githubGetURL(ctx context.Context, c Connection, rawURL string, value any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("invalid GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := apiClient()
	resp, err := apiDo(ctx, client, req, "GitHub")
	if err != nil {
		return nil, err
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
		return nil, apiErr
	}
	if err := json.NewDecoder(resp.Body).Decode(value); err != nil {
		return nil, errors.New("invalid GitHub API response")
	}
	return resp.Header, nil
}

func githubNextPage(current string, header http.Header, c Connection) (string, error) {
	var next string
	for _, line := range header.Values("Link") {
		for _, part := range regexp.MustCompile(`<([^>]*)>([^<]*)`).FindAllStringSubmatch(line, -1) {
			for _, parameter := range strings.Split(part[2], ";") {
				key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if ok && key == "rel" {
					for _, relation := range strings.Fields(strings.Trim(value, "\", \t")) {
						if relation == "next" {
							if strings.TrimSpace(part[1]) == "" {
								return "", errors.New("invalid GitHub pagination URL")
							}
							next = part[1]
						}
					}
				}
			}
		}
	}
	if next == "" {
		return "", nil
	}
	currentURL, _ := url.Parse(current)
	nextURL, err := url.Parse(next)
	if err != nil {
		return "", errors.New("invalid GitHub pagination URL")
	}
	nextURL = currentURL.ResolveReference(nextURL)
	root, _ := url.Parse(githubAPIRoot(c))
	if nextURL.Scheme != root.Scheme || nextURL.Host != root.Host || nextURL.User != nil || nextURL.Fragment != "" || !strings.HasPrefix(nextURL.Path, strings.TrimRight(root.Path, "/")+"/") {
		return "", errors.New("GitHub pagination URL must remain on the configured API server without credentials")
	}
	return nextURL.String(), nil
}

func githubList(ctx context.Context, c Connection, path string, add func(githubRepository)) error {
	next := githubAPIRoot(c) + "/" + path
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return errors.New("GitHub pagination repeated a page")
		}
		seen[next] = true
		var repositories []githubRepository
		header, err := githubGetURL(ctx, c, next, &repositories)
		if err != nil {
			return err
		}
		if repositories == nil {
			return errors.New("invalid GitHub repository list")
		}
		for _, repository := range repositories {
			add(repository)
		}
		next, err = githubNextPage(next, header, c)
		if err != nil {
			return err
		}
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
	excludedOrganizations := map[string]bool{}
	for _, organization := range c.Exclude.Organizations {
		excludedOrganizations[organization] = true
	}
	excludedOwner := func(path string) bool { owner, _, _ := strings.Cut(path, "/"); return excludedOrganizations[owner] }
	seenIDs := map[int64]bool{}
	failure := func(repo Repository, err error) {
		failures = append(failures, Outcome{Repository: repo, Outcome: "failed", Error: err.Error(), CheckedAt: time.Now().UTC()})
	}
	addRepository := func(p githubRepository) {
		repo := Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: p.Path}
		parts := strings.Split(p.Path, "/")
		if p.ID <= 0 || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			failure(repo, errors.New("invalid GitHub repository identity"))
			return
		}
		if seenIDs[p.ID] || excluded[p.Path] || excludedOwner(p.Path) || (p.Archived && !c.IncludeArchived) || (p.Fork && !c.IncludeForks) {
			return
		}
		seenIDs[p.ID] = true
		repo.ID = strconv.FormatInt(p.ID, 10)
		repo.DefaultBranch = p.DefaultBranch
		repo.CloneURL = p.CloneURL
		if repo.DefaultBranch == "" {
			failure(repo, errors.New("repository has no default branch and is not confirmed empty"))
			return
		}
		var commit struct {
			SHA string `json:"sha"`
		}
		endpoint := "repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
		if err := githubGet(ctx, c, endpoint+"/commits/"+url.PathEscape(repo.DefaultBranch), &commit); err != nil {
			var apiErr *githubAPIError
			if errors.As(err, &apiErr) && apiErr.empty {
				repo.Empty = true
				repos = append(repos, repo)
			} else {
				failure(repo, err)
			}
			return
		}
		if !regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`).MatchString(commit.SHA) {
			failure(repo, errors.New("invalid default-branch commit"))
			return
		}
		repo.Revision = commit.SHA
		repos = append(repos, repo)
	}
	seenPaths := map[string]bool{}
	for _, path := range c.Include.Repositories {
		if seenPaths[path] || excluded[path] || excludedOwner(path) {
			continue
		}
		seenPaths[path] = true
		repo := Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: path}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			failure(repo, errors.New("GitHub repositories must use owner/repository paths"))
			continue
		}
		var p githubRepository
		if err := githubGet(ctx, c, "repos/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1]), &p); err != nil {
			failure(repo, err)
			continue
		}
		addRepository(p)
	}
	seenOrganizations := map[string]bool{}
	for _, organization := range c.Include.Organizations {
		if seenOrganizations[organization] || excludedOrganizations[organization] {
			continue
		}
		seenOrganizations[organization] = true
		if err := githubList(ctx, c, "orgs/"+url.PathEscape(organization)+"/repos?type=all&per_page=100", addRepository); err != nil {
			failure(Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: organization}, err)
		}
	}
	if c.AllAccessible {
		if err := githubList(ctx, c, "user/repos?visibility=all&affiliation=owner%2Ccollaborator%2Corganization_member&per_page=100&sort=full_name", addRepository); err != nil {
			failure(Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: "all_accessible"}, err)
		}
	}
	return repos, failures
}
