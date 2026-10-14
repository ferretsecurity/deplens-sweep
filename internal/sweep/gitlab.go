package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Repository struct {
	Connection    string `json:"connection"`
	Provider      string `json:"provider"`
	ServerURL     string `json:"server_url"`
	ID            string `json:"repository_id"`
	Path          string `json:"path"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Revision      string `json:"revision,omitempty"`
	Empty         bool   `json:"-"`
	CloneURL      string `json:"-"`
}
type gitlabProject struct {
	ID            int64            `json:"id"`
	Path          string           `json:"path_with_namespace"`
	DefaultBranch string           `json:"default_branch"`
	CloneURL      string           `json:"http_url_to_repo"`
	Empty         bool             `json:"empty_repo"`
	Archived      bool             `json:"archived"`
	Fork          *json.RawMessage `json:"forked_from_project"`
}

func gitlabGet(ctx context.Context, c Connection, path string, value any) error {
	_, err := gitlabGetURL(ctx, c, c.ServerURL+"/api/v4/"+path, value)
	return err
}

func gitlabGetURL(ctx context.Context, c Connection, rawURL string, value any, recursiveGroup ...bool) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("invalid GitLab request")
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	client := apiClient()
	resp, err := apiDo(ctx, client, req, "GitLab", recursiveGroup...)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitLab API returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(value); err != nil {
		return nil, errors.New("invalid GitLab API response")
	}
	return resp.Header, nil
}

func gitlabNextPage(current string, header http.Header, c Connection) (string, error) {
	var next string
	for _, line := range header.Values("Link") {
		for _, part := range strings.Split(line, ",") {
			close := strings.Index(part, ">")
			open := strings.Index(part, "<")
			if open < 0 || close < open {
				continue
			}
			for _, parameter := range strings.Split(part[close+1:], ";") {
				key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if ok && key == "rel" && strings.Trim(value, "\" ") == "next" {
					next = part[open+1 : close]
				}
			}
		}
	}
	currentURL, _ := url.Parse(current)
	if next == "" && header.Get("X-Next-Page") != "" {
		page, err := strconv.Atoi(header.Get("X-Next-Page"))
		if err != nil || page <= 0 {
			return "", errors.New("invalid GitLab pagination page")
		}
		query := currentURL.Query()
		query.Set("page", strconv.Itoa(page))
		currentURL.RawQuery = query.Encode()
		next = currentURL.String()
	}
	if next == "" {
		return "", nil
	}
	nextURL, err := url.Parse(next)
	if err != nil {
		return "", errors.New("invalid GitLab pagination URL")
	}
	nextURL = currentURL.ResolveReference(nextURL)
	server, _ := url.Parse(c.ServerURL)
	if nextURL.Scheme != server.Scheme || nextURL.Host != server.Host || nextURL.User != nil || nextURL.Fragment != "" || !strings.HasPrefix(nextURL.Path, strings.TrimRight(server.Path, "/")+"/api/v4/") {
		return "", errors.New("GitLab pagination URL must remain on the configured API server without credentials")
	}
	return nextURL.String(), nil
}

func gitlabList[T any](ctx context.Context, c Connection, path string, add func(T)) error {
	next := c.ServerURL + "/api/v4/" + path
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return errors.New("GitLab pagination repeated a page")
		}
		seen[next] = true
		var projects []T
		header, err := gitlabGetURL(ctx, c, next, &projects, strings.HasPrefix(path, "groups/") && strings.Contains(path, "include_subgroups=true"))
		if err != nil {
			return err
		}
		if projects == nil {
			return errors.New("invalid GitLab list")
		}
		for _, project := range projects {
			add(project)
		}
		next, err = gitlabNextPage(next, header, c)
		if err != nil {
			return err
		}
	}
	return nil
}

func discoverGitLab(ctx context.Context, c Connection, switchedToWalk func(string)) ([]Repository, []Outcome) {
	var repos []Repository
	var failures []Outcome
	excluded := map[string]bool{}
	for _, p := range c.Exclude.Repositories {
		excluded[p] = true
	}
	excludedGroup := func(path string) bool {
		for _, group := range c.Exclude.Groups {
			if path == group || strings.HasPrefix(path, group+"/") {
				return true
			}
		}
		return false
	}
	seenIDs := map[int64]bool{}
	failure := func(repo Repository, err error) {
		failures = append(failures, Outcome{Repository: repo, Outcome: "failed", Error: err.Error(), CheckedAt: time.Now().UTC()})
	}
	addProject := func(p gitlabProject) {
		repo := Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: p.Path}
		if p.ID <= 0 || p.Path == "" {
			failure(repo, errors.New("invalid GitLab repository identity"))
			return
		}
		if seenIDs[p.ID] || excluded[p.Path] || excludedGroup(p.Path) || (p.Archived && !c.IncludeArchived) || (p.Fork != nil && !c.IncludeForks) {
			return
		}
		seenIDs[p.ID] = true
		repo.ID = strconv.FormatInt(p.ID, 10)
		repo.DefaultBranch = p.DefaultBranch
		repo.Empty = p.Empty
		repo.CloneURL = p.CloneURL
		var err error
		if !repo.Empty {
			if repo.DefaultBranch == "" {
				err = errors.New("repository has no default branch and is not confirmed empty")
			} else {
				var commit struct {
					ID string `json:"id"`
				}
				err = gitlabGet(ctx, c, "projects/"+repo.ID+"/repository/commits/"+url.PathEscape(repo.DefaultBranch), &commit)
				if err == nil {
					if !validRevision(commit.ID) {
						err = errors.New("invalid default-branch commit")
					} else {
						repo.Revision = commit.ID
					}
				}
			}
		}
		if err != nil {
			failure(repo, err)
			return
		}
		repos = append(repos, repo)
	}
	seenPaths := map[string]bool{}
	for _, path := range c.Include.Repositories {
		if seenPaths[path] || excluded[path] || excludedGroup(path) {
			continue
		}
		seenPaths[path] = true
		var p gitlabProject
		if err := gitlabGet(ctx, c, "projects/"+url.PathEscape(path), &p); err != nil {
			failure(Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: path}, err)
			continue
		}
		addProject(p)
	}
	seenGroups := map[string]bool{}
	var walk func(string)
	walk = func(group string) {
		if ctx.Err() != nil || seenGroups[group] || excludedGroup(group) {
			return
		}
		seenGroups[group] = true
		report := func(err error) {
			if err != nil {
				failure(Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: group}, err)
			}
		}
		report(gitlabList(ctx, c, "groups/"+url.PathEscape(group)+"/projects?include_subgroups=false&with_shared=false&per_page=100", addProject))
		if ctx.Err() != nil {
			return
		}
		type subgroup struct {
			ID   int64  `json:"id"`
			Path string `json:"full_path"`
		}
		var children []string
		report(gitlabList(ctx, c, "groups/"+url.PathEscape(group)+"/subgroups?per_page=100", func(child subgroup) {
			if child.ID <= 0 || child.Path == "" {
				report(errors.New("invalid GitLab subgroup identity"))
				return
			}
			children = append(children, child.Path)
		}))
		for _, child := range children {
			walk(child)
		}
	}
	walking := c.GitLabGroupDiscovery == "walk"
	for _, group := range c.Include.Groups {
		if seenGroups[group] || excludedGroup(group) {
			continue
		}
		if walking {
			walk(group)
			continue
		}
		if err := gitlabList(ctx, c, "groups/"+url.PathEscape(group)+"/projects?include_subgroups=true&with_shared=false&per_page=100", addProject); err != nil {
			var status *apiStatusError
			if c.GitLabGroupDiscovery != "recursive" && ctx.Err() == nil && errors.As(err, &status) && status.consecutive500 {
				walking = true
				switchedToWalk(group)
				walk(group)
				continue
			}
			failure(Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: group}, err)
		}
		seenGroups[group] = true
	}
	if c.AllAccessible {
		if err := gitlabList(ctx, c, "projects?per_page=100&order_by=id&sort=asc", addProject); err != nil {
			failure(Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: "all_accessible"}, err)
		}
	}
	return repos, failures
}

func validateCloneURL(c Connection, raw string) error {
	u, err := url.Parse(raw)
	server, e := url.Parse(c.ServerURL)
	if err != nil || e != nil || u.Scheme != "https" || u.Host != server.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, strings.TrimRight(server.Path, "/")+"/") {
		return errors.New("clone URL must use HTTPS on the configured server without credentials")
	}
	return nil
}
