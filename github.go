package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

const maxJSONResponse = 12 << 20
const maxArtifactArchive = 32 << 20

type ghClient struct {
	hostname string
	repo     string
	apiFunc  func(path string, maxBytes int) ([]byte, error)
}

func (c *ghClient) resolveContext() error {
	if c.repo != "" {
		parts := strings.Split(c.repo, "/")
		if len(parts) == 3 && c.hostname == "" {
			c.hostname = parts[0]
			c.repo = parts[1] + "/" + parts[2]
		} else if len(parts) != 2 {
			return fmt.Errorf("--repo must be OWNER/REPO or HOST/OWNER/REPO")
		}
		if c.hostname == "" {
			c.hostname = "github.com"
		}
		return nil
	}

	args := []string{"repo", "view", "--json", "nameWithOwner,url"}
	if c.hostname != "" {
		args = append(args, "--hostname", c.hostname)
	}
	out, err := runCommand(maxJSONResponse, "gh", args...)
	if err != nil {
		return fmt.Errorf("cannot determine repository from the current directory; use --repo OWNER/REPO: %w", err)
	}
	var info struct {
		NameWithOwner string `json:"nameWithOwner"`
		URL           string `json:"url"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return fmt.Errorf("decode gh repository context: %w", err)
	}
	if !strings.Contains(info.NameWithOwner, "/") {
		return errors.New("gh did not return a repository in OWNER/REPO form")
	}
	c.repo = info.NameWithOwner
	parsed, err := url.Parse(info.URL)
	if err == nil && parsed.Hostname() != "" {
		c.hostname = parsed.Hostname()
	}
	if c.hostname == "" {
		c.hostname = "github.com"
	}
	return nil
}

func (c ghClient) api(path string, maxBytes int) ([]byte, error) {
	if c.apiFunc != nil {
		return c.apiFunc(path, maxBytes)
	}
	args := []string{"api", "--hostname", c.hostname, path}
	out, err := runCommand(maxBytes, "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed for %s: %w", path, err)
	}
	return out, nil
}

func (c ghClient) repoPath(suffix string) string {
	return "/repos/" + c.repo + suffix
}

func runCommand(limit int, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stdout limitedBuffer
	stdout.limit = limit
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if stdout.exceeded {
			return nil, fmt.Errorf("response exceeded the %d byte safety limit", limit)
		}
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if stdout.exceeded {
		return nil, fmt.Errorf("response exceeded the %d byte safety limit", limit)
	}
	return stdout.buf, nil
}

type limitedBuffer struct {
	buf      []byte
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - len(b.buf)
	if remaining <= 0 || len(p) > remaining {
		if remaining > 0 {
			b.buf = append(b.buf, p[:remaining]...)
		}
		b.exceeded = true
		return 0, errors.New("output limit exceeded")
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func apiItemPath(path string) string {
	return path
}

func artifactArchivePath(repo string, id int64) string {
	return "/repos/" + repo + "/actions/artifacts/" + fmt.Sprint(id) + "/zip"
}

func runPath(repo string, runID int64) string {
	return "/repos/" + repo + "/actions/runs/" + fmt.Sprint(runID)
}

func runArtifactsPath(repo string, runID int64) string {
	return "/repos/" + repo + "/actions/runs/" + fmt.Sprint(runID) + "/artifacts?per_page=100"
}

func repoEventsPath(repo string, page int) string {
	return "/repos/" + repo + "/events?per_page=100&page=" + fmt.Sprint(page)
}

func pullRequestPath(repo string, number int64) string {
	return "/repos/" + repo + "/pulls/" + fmt.Sprint(number)
}

func commitPath(repo, sha string) string {
	return "/repos/" + repo + "/commits/" + url.PathEscape(sha)
}

func commitPullsPath(repo, sha string) string {
	return "/repos/" + repo + "/commits/" + url.PathEscape(sha) + "/pulls?per_page=100"
}
