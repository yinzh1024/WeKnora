package docparser

import (
	"regexp"
	"strings"
)

// schemeLikeRef matches image targets that carry an explicit URI scheme
// (http:, https:, data:, blob:, local:, resource:, javascript:, ...).
var schemeLikeRef = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+\-.]*:`)

// CollectRelativeImageRefs returns the image targets in markdown that point at
// relative paths: no URI scheme, no leading slash, no fragment. Both
// ![alt](target) destinations and quoted <img src="..."> attributes are
// scanned. Each returned string is the exact target text the resolvers match
// against ImageRef.OriginalRef, so callers can inject sibling-file bytes for
// them and let ResolveAndStore rewrite the reference like any inline image.
//
// Fenced code blocks are deliberately not skipped: the resolvers themselves
// rewrite image references anywhere in the document (an accepted limitation
// documented by TestResolveRemoteImages_CodeExampleIsRewritten), and a
// reference that is never injected simply stays untouched.
func CollectRelativeImageRefs(markdown string) []string {
	if markdown == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var refs []string
	add := func(ref string) {
		if !isRelativeImageRef(ref) {
			return
		}
		if _, dup := seen[ref]; dup {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}

	for _, span := range scanMarkdownImageTargets(markdown) {
		add(extractMarkdownImagePath(markdown[span.TargetStart:span.TargetEnd]))
	}
	for _, m := range imgHTMLSrc.FindAllStringSubmatch(markdown, -1) {
		if len(m) > 2 {
			add(strings.TrimSpace(m[2]))
		}
	}
	return refs
}

// isRelativeImageRef reports whether ref can point at a file next to the
// document. Scheme-qualified, absolute, and fragment-only targets are handled
// by other resolver passes or are not file references at all.
func isRelativeImageRef(ref string) bool {
	if ref == "" || strings.ContainsAny(ref, "\r\n\t") {
		return false
	}
	if schemeLikeRef.MatchString(ref) {
		return false
	}
	return !strings.HasPrefix(ref, "/") && !strings.HasPrefix(ref, "#")
}

// extractMarkdownImagePath pulls the path out of a raw ![alt](...) destination
// with the same rules splitMarkdownImageTarget applies, minus the refMap
// lookups: an angle-bracket destination yields its inner text verbatim, a
// title suffix is stripped, and anything else is the trimmed destination.
func extractMarkdownImagePath(raw string) string {
	start, end := trimMarkdownSpaceBounds(raw, 0, len(raw))
	if start >= end {
		return ""
	}
	if raw[start] == '<' {
		for i := start + 1; i < end; i++ {
			if raw[i] == '>' && !isEscaped(raw, i) {
				return raw[start+1 : i]
			}
		}
		return ""
	}
	trimmed := raw[start:end]
	if titleStart, ok := parseMarkdownImageTitleSuffix(trimmed); ok {
		path := trimmed[:titleStart]
		for len(path) > 0 && isMarkdownSpace(path[len(path)-1]) {
			path = path[:len(path)-1]
		}
		return path
	}
	return trimmed
}
