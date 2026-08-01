package provider

import (
	"fmt"
	"sort"
	"strings"
)

// What each provider will accept as an attachment. An entry ending in "/"
// matches a whole family.
//
// This is deliberately per-provider and not per-model. A model-by-model
// capability table is the kind of configuration this program exists
// without, and it would be stale the day it shipped. Provider level is
// statically known and catches the error worth catching early — sending
// audio to a provider that has never accepted audio. A model that is
// merely the wrong model for the job is the provider's 400 to report, in
// its own words.
var accepts = map[string][]string{
	"anthropic":    {"image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf"},
	"openai":       {"image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf"},
	"openai-codex": {"image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf"},
	"gemini":       {"image/", "audio/", "video/", "application/pdf"},
	"openrouter":   {"image/", "application/pdf", "audio/wav", "audio/mpeg"},
}

// Accepts reports whether the provider named by a model spec takes this
// media type, and says so in the error when it does not. It is called
// before anything is written to the log: an attachment this provider can
// never carry must not become a user event, or every later fold replays a
// request that cannot be sent.
func Accepts(spec, mediaType string) error {
	name, _, _ := strings.Cut(spec, "/")
	list, ok := accepts[name]
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	for _, pat := range list {
		if pat == mediaType || (strings.HasSuffix(pat, "/") && strings.HasPrefix(mediaType, pat)) {
			return nil
		}
	}
	return fmt.Errorf("%s does not accept %s (it takes %s)", name, mediaType, strings.Join(families(list), ", "))
}

// families renders an accepts list the way a person would say it.
func families(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, pat := range list {
		f := pat
		if !strings.HasSuffix(f, "/") {
			if i := strings.IndexByte(f, '/'); i >= 0 && strings.HasPrefix(f, "image/") {
				f = "image/*"
			}
		} else {
			f += "*"
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
