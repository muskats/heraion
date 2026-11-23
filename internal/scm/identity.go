package scm

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/diff"
)

// BranchForPaths names the branch after what changed, so the model's words
// have no influence and a rerun finds the same PR:
//
//	BranchForPaths("trivy-remediate", "acme/web", ["Dockerfile"])
//	  = "heraion/trivy-remediate/dockerfile-<8 hex>"
//
// The hash covers the repo and every path, the slug only the first path.
func BranchForPaths(playbook, repo string, paths []string) string {
	sorted := slices.Sorted(slices.Values(paths))
	h := sha256.Sum256([]byte(repo + "\x00" + strings.Join(sorted, "\x00")))
	s := "change"
	if len(sorted) > 0 {
		if v := slug(sorted[0], 30); v != "" {
			s = v
		}
	}
	return BranchPrefix + playbook + "/" + s + "-" + hex.EncodeToString(h[:])[:8]
}

// ChangeID is a hash of the added and removed lines per path, so the same
// edit gives the same ID on every run, whatever the commit or PR text.
func ChangeID(patch diff.Patch) string {
	type file struct {
		path  string
		lines []string
	}
	var files []file
	for _, fp := range patch.FilePatches() {
		from, to := fp.Files()
		f := file{}
		if to != nil {
			f.path = to.Path()
		} else if from != nil {
			f.path = from.Path()
		}
		for _, c := range fp.Chunks() {
			prefix := ""
			switch c.Type() {
			case diff.Add:
				prefix = "+"
			case diff.Delete:
				prefix = "-"
			default:
				continue
			}
			for _, l := range strings.Split(strings.TrimSuffix(c.Content(), "\n"), "\n") {
				f.lines = append(f.lines, prefix+strings.TrimRight(l, " \t\r"))
			}
		}
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.path, b.path) })
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.path + "\x00" + strings.Join(f.lines, "\n") + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

var markerRE = regexp.MustCompile(`<!-- heraion:change=([0-9a-f]{64}) -->`)

// changeMarker is appended to the PR body, e.g. "<!-- heraion:change=3f9a... -->".
func changeMarker(change string) string { return "<!-- heraion:change=" + change + " -->" }

// ChangeOf returns the change ID in a PR body, or "" (e.g. a PR from 2.x).
func ChangeOf(body string) string {
	if m := markerRE.FindAllStringSubmatch(body, -1); len(m) > 0 {
		return m[len(m)-1][1]
	}
	return ""
}

var trailerRE = regexp.MustCompile(`(?m)^Heraion-Change: ([0-9a-f]{64})$`)

// changeTrailer ends the commit message, e.g. "Heraion-Change: 3f9a...". It
// records the change on the branch itself, so a PR whose body could not be
// updated still shows what its branch holds.
func changeTrailer(change string) string { return "Heraion-Change: " + change }

// changeOfCommit returns the change ID in a commit message, or "".
func changeOfCommit(msg string) string {
	if m := trailerRE.FindAllStringSubmatch(msg, -1); len(m) > 0 {
		return m[len(m)-1][1]
	}
	return ""
}
