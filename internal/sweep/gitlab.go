package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gitlabRevisionLookups = 8
	gitlabGroupWalkers    = 8
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
	// Discovery runs commit lookups and subgroup walks concurrently. Each project and
	// failure carries an order key, so results keep the sequential discovery order:
	// a group's own projects first, then each subgroup in listing order.
	var mu sync.Mutex
	seenIDs := map[int64]bool{}
	seenGroups := map[string]bool{}
	type candidate struct {
		order []int
		repo  Repository
		err   error
		at    time.Time
	}
	var candidates []*candidate
	var resolving, walking sync.WaitGroup
	slots := make(chan struct{}, gitlabRevisionLookups)
	walkers := make(chan struct{}, gitlabGroupWalkers)
	failure := func(order []int, repo Repository, err error) {
		mu.Lock()
		defer mu.Unlock()
		candidates = append(candidates, &candidate{order: order, repo: repo, err: err, at: time.Now().UTC()})
	}
	addProject := func(order []int, p gitlabProject) {
		repo := Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: p.Path}
		if p.ID <= 0 || p.Path == "" {
			failure(order, repo, errors.New("invalid GitLab repository identity"))
			return
		}
		mu.Lock()
		if seenIDs[p.ID] || excluded[p.Path] || excludedGroup(p.Path) || (p.Archived && !c.IncludeArchived) || (p.Fork != nil && !c.IncludeForks) {
			mu.Unlock()
			return
		}
		seenIDs[p.ID] = true
		repo.ID = strconv.FormatInt(p.ID, 10)
		repo.DefaultBranch = p.DefaultBranch
		repo.Empty = p.Empty
		repo.CloneURL = p.CloneURL
		found := &candidate{order: order, repo: repo}
		candidates = append(candidates, found)
		mu.Unlock()
		if repo.Empty {
			return
		}
		if repo.DefaultBranch == "" {
			found.err, found.at = errors.New("repository has no default branch and is not confirmed empty"), time.Now().UTC()
			return
		}
		// Commit lookups dominate discovery time, so they overlap with the walk.
		// The slot channel also bounds the request rate against the API.
		resolving.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; resolving.Done() }()
			var commit struct {
				ID string `json:"id"`
			}
			found.err = gitlabGet(ctx, c, "projects/"+found.repo.ID+"/repository/commits/"+url.PathEscape(found.repo.DefaultBranch), &commit)
			if found.err == nil {
				if !validRevision(commit.ID) {
					found.err = errors.New("invalid default-branch commit")
				} else {
					found.repo.Revision = commit.ID
				}
			}
			found.at = time.Now().UTC()
		}()
	}
	// listed numbers projects from one list in page order under a parent key.
	listed := func(parent []int) func(gitlabProject) {
		next := 0
		return func(p gitlabProject) {
			addProject(append(slices.Clip(parent), 0, next), p)
			next++
		}
	}
	// afterListed orders a listing failure after failures already found in that listing.
	afterListed := func(order []int) []int { return append(slices.Clip(order), 0, math.MaxInt) }
	top := 0
	nextTop := func() []int { top++; return []int{top} }
	seenPaths := map[string]bool{}
	for _, path := range c.Include.Repositories {
		if seenPaths[path] || excluded[path] || excludedGroup(path) {
			continue
		}
		seenPaths[path] = true
		order := nextTop()
		var p gitlabProject
		if err := gitlabGet(ctx, c, "projects/"+url.PathEscape(path), &p); err != nil {
			failure(order, Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: path}, err)
			continue
		}
		addProject(order, p)
	}
	// Each group first tries recursive listing unless walk mode was requested.
	// A fallback lists only this group's direct projects and children; children
	// make their own recursive attempt. Release the slot before starting children
	// so nested groups cannot exhaust the slots.
	var walk func(string, []int)
	walk = func(group string, order []int) {
		mu.Lock()
		if seenGroups[group] || excludedGroup(group) {
			mu.Unlock()
			return
		}
		seenGroups[group] = true
		mu.Unlock()
		walking.Add(1)
		go func() {
			defer walking.Done()
			groupFailure := afterListed(order)
			report := func(err error) {
				if err != nil {
					failure(groupFailure, Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: group}, err)
				}
			}
			walkers <- struct{}{}
			children, ok := func() ([]string, bool) {
				defer func() { <-walkers }()
				if err := ctx.Err(); err != nil {
					report(err)
					return nil, false
				}
				if c.GitLabGroupDiscovery != "walk" {
					err := gitlabList(ctx, c, "groups/"+url.PathEscape(group)+"/projects?include_subgroups=true&with_shared=false&per_page=100", listed(append(slices.Clip(order), 0)))
					if err == nil {
						return nil, false
					}
					var status *apiStatusError
					if c.GitLabGroupDiscovery == "recursive" || ctx.Err() != nil || !errors.As(err, &status) || !status.consecutive500 {
						report(err)
						return nil, false
					}
					switchedToWalk(group)
					// Keep successful recursive pages before fallback results.
					order = append(slices.Clip(order), 1)
					groupFailure = afterListed(order)
				}
				err := gitlabList(ctx, c, "groups/"+url.PathEscape(group)+"/projects?include_subgroups=false&with_shared=false&per_page=100", listed(order))
				report(err)
				if ctx.Err() != nil {
					if err == nil {
						report(ctx.Err())
					}
					return nil, false
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
				return children, true
			}()
			if !ok {
				return
			}
			for i, child := range children {
				walk(child, append(slices.Clip(order), 1+i))
			}
		}()
	}
	for _, group := range c.Include.Groups {
		walk(group, nextTop())
		walking.Wait()
	}
	if c.AllAccessible {
		order := nextTop()
		if err := gitlabList(ctx, c, "projects?per_page=100&order_by=id&sort=asc", listed(order)); err != nil {
			failure(afterListed(order), Repository{Connection: c.Name, Provider: c.Provider, ServerURL: c.ServerURL, Path: "all_accessible"}, err)
		}
	}
	resolving.Wait()
	slices.SortStableFunc(candidates, func(a, b *candidate) int { return slices.Compare(a.order, b.order) })
	for _, found := range candidates {
		if found.err != nil {
			failures = append(failures, Outcome{Repository: found.repo, Outcome: "failed", Error: found.err.Error(), CheckedAt: found.at})
			continue
		}
		repos = append(repos, found.repo)
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
