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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ServerURL+"/api/v4/"+path, nil)
	if err != nil {
		return errors.New("invalid GitLab request")
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := apiDo(ctx, client, req, "GitLab")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitLab API returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(value); err != nil {
		return errors.New("invalid GitLab API response")
	}
	return nil
}

func discoverGitLab(ctx context.Context, c Connection) ([]Repository, []Outcome) {
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
		var p gitlabProject
		err := gitlabGet(ctx, c, "projects/"+url.PathEscape(path), &p)
		if err != nil {
			failures = append(failures, Outcome{Repository: repo, Outcome: "failed", Error: err.Error(), CheckedAt: time.Now().UTC()})
			continue
		}
		if p.ID <= 0 || p.Path == "" {
			failures = append(failures, Outcome{Repository: repo, Outcome: "failed", Error: "invalid GitLab repository identity", CheckedAt: time.Now().UTC()})
			continue
		}
		if seenIDs[p.ID] || excluded[p.Path] || (p.Archived && !c.IncludeArchived) || (p.Fork != nil && !c.IncludeForks) {
			continue
		}
		seenIDs[p.ID] = true
		repo.ID = strconv.FormatInt(p.ID, 10)
		repo.Path = p.Path
		repo.DefaultBranch = p.DefaultBranch
		repo.Empty = p.Empty
		repo.CloneURL = p.CloneURL
		if !repo.Empty {
			if repo.DefaultBranch == "" {
				err = errors.New("repository has no default branch and is not confirmed empty")
			} else {
				var commit struct {
					ID string `json:"id"`
				}
				err = gitlabGet(ctx, c, "projects/"+repo.ID+"/repository/commits/"+url.PathEscape(repo.DefaultBranch), &commit)
				if err == nil {
					if len(commit.ID) != 40 && len(commit.ID) != 64 {
						err = errors.New("invalid default-branch commit")
					} else {
						repo.Revision = commit.ID
					}
				}
			}
		}
		if err != nil {
			failures = append(failures, Outcome{Repository: repo, Outcome: "failed", Error: err.Error(), CheckedAt: time.Now().UTC()})
			continue
		}
		repos = append(repos, repo)
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
