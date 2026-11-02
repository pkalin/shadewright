/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github

import (
	"crypto/sha1" // Git object IDs are SHA-1; used to check content against its ID, not for secrecy.
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// HeadBranchPrefix starts the name of every branch the provider writes to.
const HeadBranchPrefix = "shadewright/"

const (
	maxPathBytes  = 1024
	maxPathDepth  = 32
	maxTitleRunes = 256
	// maxBodyRunes is GitHub's limit for a pull request body.
	maxBodyRunes    = 65536
	maxMessageBytes = 64 << 10
)

var (
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// headNamePattern is the part after shadewright/: one path segment, as a request UID is.
	headNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	// targetPattern is a conservative subset of Git branch names.
	targetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

func validSHA(s string) bool { return shaPattern.MatchString(s) }

// isHeadBranch reports whether b is shadewright/<name> with one safe segment.
func isHeadBranch(b string) bool {
	name, ok := strings.CutPrefix(b, HeadBranchPrefix)
	return ok && headNamePattern.MatchString(name) && !strings.Contains(name, "..") &&
		!strings.HasSuffix(name, ".lock") && !strings.HasSuffix(name, ".")
}

func validTargetBranch(b string) bool {
	if !targetPattern.MatchString(b) || strings.HasPrefix(b, HeadBranchPrefix) {
		return false
	}
	if strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "/.") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") {
		return false
	}
	return true
}

// validPath accepts a relative, clean path of regular segments: no leading or trailing slash, no empty, "."
// or ".." segment, no ".git" directory, no control characters, valid UTF-8 and bounded.
func validPath(p string) bool {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) {
		return false
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxPathDepth {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.EqualFold(s, ".git") {
			return false
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

// refPath is the escaped path of a branch under git/ref(s)/heads.
func refPath(branch string) string {
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "heads/" + strings.Join(segs, "/")
}

// blobSHA is the Git object ID of a blob with this content.
func blobSHA(data []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
