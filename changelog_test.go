package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// changelog.md is load-bearing: the release workflow reads the section for a tag
// and uses it as the release body, so a malformed file fails a release rather
// than a test. These checks are cheap and catch the two ways it goes wrong: an
// entry written in the wrong shape, and an entry that the extractor cannot find.

var (
	// `## [0.2.0] - 2026-11-27`, or `## [Unreleased]` with nothing after it.
	changelogVersionHeading = regexp.MustCompile(`^## \[([^\]]+)\](?: - (\d{4}-\d{2}-\d{2}))?$`)
	// semver, because the tag is the version and the tag drives the image tags.
	changelogSemver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// Only these, and the file's own preamble says so.
	changelogAllowedSections = map[string]bool{
		"Breaking": true, "Added": true, "Changed": true, "Fixed": true,
	}
)

func readChangelog(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}

	return strings.Split(string(body), "\n")
}

// changelogVersions returns every version heading in file order.
func changelogVersions(t *testing.T, lines []string) []string {
	t.Helper()

	var versions []string
	for _, line := range lines {
		if match := changelogVersionHeading.FindStringSubmatch(line); match != nil {
			versions = append(versions, match[1])
		}
	}

	if len(versions) == 0 {
		t.Fatal("CHANGELOG.md has no '## [x.y.z] - YYYY-MM-DD' heading")
	}

	return versions
}

func TestChangelogHeadingsAreWellFormed(t *testing.T) {
	lines := readChangelog(t)
	versions := changelogVersions(t, lines)

	for _, line := range lines {
		if !strings.HasPrefix(line, "## [") {
			continue
		}

		match := changelogVersionHeading.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("version heading is not '## [x.y.z] - YYYY-MM-DD': %q", line)
		}

		version, date := match[1], match[2]
		if version == "Unreleased" {
			// Unreleased is a date-less placeholder, and only that one may be.
			if date != "" {
				t.Fatalf("Unreleased must not carry a date: %q", line)
			}
			continue
		}

		if !changelogSemver.MatchString(version) {
			t.Fatalf("version %q is not x.y.z; the tag is the version", version)
		}
		if date == "" {
			t.Fatalf("version %q has no release date", version)
		}
	}

	// Newest first, and Unreleased before everything: the workflow reads top-down
	// and a human does too.
	if versions[0] != "Unreleased" {
		t.Fatalf("the first heading is %q, want Unreleased at the top", versions[0])
	}
}

func TestChangelogSectionsAreTheAgreedFour(t *testing.T) {
	// The point of a fixed, tiny set is that a reader knows where to look. A new
	// heading is a convention change, so it should fail here first.
	for _, line := range readChangelog(t) {
		if !strings.HasPrefix(line, "### ") {
			continue
		}

		name := strings.TrimPrefix(line, "### ")
		if !changelogAllowedSections[name] {
			t.Fatalf("'### %s' is not one of Breaking, Added, Changed, Fixed", name)
		}
	}
}

func TestChangelogEntriesAreNotEmpty(t *testing.T) {
	// A version heading followed straight by the next one is an entry that says
	// nothing, and it would be published as an empty release body.
	lines := readChangelog(t)

	inVersion, inSection, wrote := false, false, false
	for _, line := range lines {
		switch {
		case changelogVersionHeading.MatchString(line):
			if inVersion && !wrote {
				t.Fatalf("the entry above %q has no content", line)
			}
			// Unreleased may legitimately be empty between releases.
			inVersion = !strings.Contains(line, "Unreleased")
			inSection, wrote = false, false
		case strings.HasPrefix(line, "### "):
			inSection = true
			wrote = false
		default:
			if (inVersion || inSection) && strings.TrimSpace(line) != "" {
				wrote = true
			}
		}
	}
}

func TestChangelogNotesScriptFindsEveryVersion(t *testing.T) {
	// This is the half of the contract a test usually misses: the file can be
	// well formed and still not extractable, and that failure would surface at
	// tag time, after the image is already on GHCR.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}

	for _, version := range changelogVersions(t, readChangelog(t)) {
		if version == "Unreleased" {
			continue
		}

		cmd := exec.Command("sh", "scripts/changelog-notes.sh", "v"+version)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("scripts/changelog-notes.sh v%s failed: %v", version, err)
		}
		if strings.TrimSpace(string(out)) == "" {
			t.Fatalf("scripts/changelog-notes.sh v%s printed nothing", version)
		}
	}
}
