package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Remote Git mirror of the OKF repository (GOVERNANCE.md §31 item 5, and
// "OKF Git history: indefinite"). A mirror run pushes every ref of the OKF
// repository to the configured remote with `git push --mirror`, then reads
// the remote's refs back with `git ls-remote` and requires every local ref
// to be on the remote at the same commit. The outcome is recorded; coverage
// is derived from the newest verified run, not from configuration.

type GitMirrorRun struct {
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	Actor       string `json:"actor"`
	Repository  string `json:"repository"`
	Remote      string `json:"remote"`
	Status      string `json:"status"` // verified, failed
	Refs        int    `json:"refs"`
	Head        string `json:"head,omitempty"`
	Detail      string `json:"detail"`
}

func (s *backupServer) gitMirrorStatePath() string {
	return filepath.Join(s.backupDir, "git-mirror", "last-run.json")
}

func (s *backupServer) lastGitMirrorRun() (*GitMirrorRun, error) {
	data, err := os.ReadFile(s.gitMirrorStatePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var run GitMirrorRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// remoteIsOffHost reports whether a git remote names another host: a path
// or file:// URL is on this one.
func remoteIsOffHost(remote string) (bool, string) {
	r := strings.TrimSpace(remote)
	switch {
	case r == "":
		return false, "no remote is configured"
	case strings.HasPrefix(r, "file://"), strings.HasPrefix(r, "/"), strings.HasPrefix(r, "."), strings.HasPrefix(r, "~"):
		return false, fmt.Sprintf("remote %q is a path on this host", r)
	case strings.Contains(r, "://"):
		return true, "remote " + r
	case strings.Contains(r, ":") && !strings.Contains(strings.SplitN(r, ":", 2)[0], "/"):
		// scp-like syntax, user@host:path
		return true, "remote " + r
	}
	return false, fmt.Sprintf("remote %q is not a recognisable remote URL", r)
}

func gitCommand(ctx context.Context, repo string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "safe.directory=*", "-C", repo}, args...)
	return commandContext(ctx, "git", full...).CombinedOutput()
}

// runGitMirror pushes and verifies the mirror, records the run, and
// returns it. Callers hold s.opMu.
func (s *backupServer) runGitMirror(ctx context.Context, actor string) (*GitMirrorRun, error) {
	run := &GitMirrorRun{StartedAt: time.Now().UTC().Format(time.RFC3339), Actor: actor, Repository: s.config.gitRepo, Remote: s.config.gitMirrorRemote, Status: "failed"}
	finish := func(detail string) (*GitMirrorRun, error) {
		run.Detail = detail
		run.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		data, err := json.Marshal(run)
		if err != nil {
			return run, err
		}
		if err := os.MkdirAll(filepath.Dir(s.gitMirrorStatePath()), 0o750); err != nil {
			return run, err
		}
		return run, writeAtomic(s.gitMirrorStatePath(), data, 0o600)
	}
	if run.Repository == "" {
		return finish("SCOS_OKF_GIT_REPO is not configured: there is no OKF repository to mirror")
	}
	if run.Remote == "" {
		return finish("SCOS_OKF_GIT_MIRROR_REMOTE is not configured")
	}
	if _, err := lookPath("git"); err != nil {
		return finish("git is unavailable: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	localRaw, err := gitCommand(ctx, run.Repository, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return finish(fmt.Sprintf("read local refs: %v: %s", err, strings.TrimSpace(string(localRaw))))
	}
	local := parseRefs(string(localRaw))
	if len(local) == 0 {
		return finish("the OKF repository has no refs to mirror")
	}
	if out, err := gitCommand(ctx, run.Repository, "push", "--mirror", run.Remote); err != nil {
		return finish(fmt.Sprintf("git push --mirror failed: %v: %s", err, strings.TrimSpace(string(out))))
	}
	remoteRaw, err := gitCommand(ctx, run.Repository, "ls-remote", run.Remote)
	if err != nil {
		return finish(fmt.Sprintf("git ls-remote failed: %v: %s", err, strings.TrimSpace(string(remoteRaw))))
	}
	remote := parseLsRemote(string(remoteRaw))
	var mismatched []string
	for ref, sha := range local {
		if remote[ref] != sha {
			mismatched = append(mismatched, fmt.Sprintf("%s (local %s, remote %q)", ref, shortHash(sha), shortHash(remote[ref])))
		}
	}
	sort.Strings(mismatched)
	if len(mismatched) > 0 {
		return finish("remote does not hold the pushed refs: " + strings.Join(mismatched, ", "))
	}
	if head, err := gitCommand(ctx, run.Repository, "rev-parse", "HEAD"); err == nil {
		run.Head = strings.TrimSpace(string(head))
	}
	run.Status, run.Refs = "verified", len(local)
	return finish(fmt.Sprintf("pushed and verified %d refs on the remote", len(local)))
}

// parseRefs reads `git for-each-ref --format='%(objectname) %(refname)'`.
func parseRefs(out string) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[1]] = fields[0]
		}
	}
	return refs
}

// parseLsRemote reads `git ls-remote` output (sha<TAB>ref).
func parseLsRemote(out string) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] != "HEAD" {
			refs[fields[1]] = fields[0]
		}
	}
	return refs
}
