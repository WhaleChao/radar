package mcp

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/skyhook-io/radar/internal/search"
	"github.com/skyhook-io/radar/pkg/readreason"
)

// maxCoverageSentences bounds coverageHint; further reasons are only counted.
const maxCoverageSentences = 3

// namespaceNotFound is hint-only: namespace_excluded as seen by a caller who
// can see every namespace. It never reaches the wire.
const namespaceNotFound readreason.Code = "namespace_not_found"

// coverageTemplate turns the unsearched entries for one or more reasons into
// one sentence. Reasons sharing a template share the sentence. all is every
// entry of the result, for sentences another reason qualifies.
type coverageTemplate struct {
	reasons  []readreason.Code
	sentence func(entries, all []search.UnsearchedKind) string
}

// catalogUnreadable reports a "*" read_error with no count: API discovery
// failed for some groups, so any kind could have been in them.
func catalogUnreadable(all []search.UnsearchedKind) bool {
	return slices.ContainsFunc(all, func(e search.UnsearchedKind) bool {
		return e.Kind == "*" && e.Reason == readreason.ReadError && e.Count == 0
	})
}

// filterErrors marks where CEL evaluation failures rank among the templates.
var filterErrors = coverageTemplate{}

// coverageTemplates is ordered most actionable first: what the agent can fix
// or retry now, then what it can only report.
var coverageTemplates = []coverageTemplate{
	{[]readreason.Code{readreason.UnknownKind}, func(e, all []search.UnsearchedKind) string {
		names := joinKinds(e, "and", func(kind string) string { return "'" + kind + "'" })
		if catalogUnreadable(all) {
			return fmt.Sprintf("%s %s recognized, but some API groups couldn't be discovered: %s may be served by one of them; retry.",
				pluralWord(len(e), "Kind", "Kinds")+" "+names, pluralWord(len(e), "wasn't", "weren't"), pluralWord(len(e), "it", "they"))
		}
		if len(e) == 1 {
			return fmt.Sprintf("Kind %s isn't recognized on this cluster: it may not be installed here, or the name may be misspelled.", names)
		}
		return fmt.Sprintf("Kinds %s aren't recognized on this cluster: they may not be installed here, or the names may be misspelled.", names)
	}},
	{[]readreason.Code{readreason.CollectionNotSupported}, func(e, _ []search.UnsearchedKind) string {
		if len(e) == 1 {
			return fmt.Sprintf("%s aren't searchable; use list_resources kind=%s.", kindPlural(e[0].Kind), e[0].Kind)
		}
		return fmt.Sprintf("%s aren't searchable; use list_resources for them.", joinKinds(e, "and", kindPlural))
	}},
	filterErrors,
	{[]readreason.Code{readreason.KindSyncPending}, func(e, _ []search.UnsearchedKind) string {
		label, n := kindsLabel(e)
		return fmt.Sprintf("%s %s still loading; retry in a few seconds before concluding absence.", capitalize(label), pluralWord(n, "was", "were"))
	}},
	{[]readreason.Code{readreason.PermissionCheckFailed}, func(e, _ []search.UnsearchedKind) string {
		label, _ := kindsLabel(e)
		return fmt.Sprintf("Radar couldn't check your access to %s; retry.", label)
	}},
	{[]readreason.Code{readreason.KindSyncFailed, readreason.ReadError, readreason.CollectorUnauthorized}, func(e, all []search.UnsearchedKind) string {
		if slices.ContainsFunc(all, func(u search.UnsearchedKind) bool { return u.Reason == readreason.UnknownKind }) {
			// The unknown-kind sentence already explains undiscovered groups.
			e = slices.DeleteFunc(slices.Clone(e), func(u search.UnsearchedKind) bool { return u.Kind == "*" && u.Count == 0 })
			if len(e) == 0 {
				return ""
			}
		}
		label, n := kindsLabel(e)
		return fmt.Sprintf("Couldn't read %s; matches of %s may be missing.", label, pluralWord(n, "that kind", "those kinds"))
	}},
	{[]readreason.Code{readreason.RBACDenied}, func(e, _ []search.UnsearchedKind) string {
		return fmt.Sprintf("Your access doesn't include all %s; this result says nothing about the rest.", joinKinds(e, "or", kindPlural))
	}},
	{[]readreason.Code{readreason.NamespaceExcluded}, func(e, _ []search.UnsearchedKind) string {
		return namespaceSentence(e, "it isn't among the namespaces you can see, or doesn't exist", "they aren't among the namespaces you can see, or don't exist")
	}},
	{[]readreason.Code{namespaceNotFound}, func(e, _ []search.UnsearchedKind) string {
		var namespaces []string
		for _, entry := range e {
			namespaces = append(namespaces, entry.Namespaces...)
		}
		if len(namespaces) == 1 {
			return fmt.Sprintf("Namespace '%s' doesn't exist on this cluster.", namespaces[0])
		}
		return fmt.Sprintf("Namespaces %s don't exist on this cluster.", quoteJoin(namespaces))
	}},
	{[]readreason.Code{readreason.NamespaceScopeLimited, readreason.CollectorScopeLimited}, func(e, _ []search.UnsearchedKind) string {
		return namespaceSentence(e, "Radar here watches only some namespaces", "Radar here watches only some namespaces")
	}},
	{[]readreason.Code{readreason.CollectorAccessDenied}, func(e, _ []search.UnsearchedKind) string {
		return fmt.Sprintf("Radar isn't allowed to read %s on this cluster; granting you access won't help.", joinKinds(e, "or", kindPlural))
	}},
}

// coverageHint explains a partial search result to an agent: one sentence per
// reason present, most actionable first, at most maxCoverageSentences and then
// a count of the rest. Every reason yields a sentence, so it is non-empty
// whenever res.Partial is true.
//
// seesAllNamespaces says the caller's RBAC covers every namespace, so a
// namespace_excluded entry can only mean the named namespace doesn't exist.
func coverageHint(res search.Result, seesAllNamespaces bool, namedKinds bool) string {
	if seesAllNamespaces {
		res.Unsearched = slices.Clone(res.Unsearched)
		for i := range res.Unsearched {
			if res.Unsearched[i].Reason == readreason.NamespaceExcluded {
				res.Unsearched[i].Reason = namespaceNotFound
			}
		}
	}
	var sentences []string
	retryIndex := -1
	retry := ""
	used := map[readreason.Code]bool{}
	for _, tmpl := range coverageTemplates {
		if tmpl.sentence == nil {
			if res.FilterErrors > 0 {
				sentences = append(sentences, filterSentence(res.FilterErrors, res.FilterErrorSample))
			}
			continue
		}
		var entries []search.UnsearchedKind
		for _, gap := range res.Unsearched {
			if slices.Contains(tmpl.reasons, gap.Reason) {
				entries = append(entries, gap)
				used[gap.Reason] = true
			}
		}
		if len(entries) > 0 {
			if sentence := tmpl.sentence(entries, res.Unsearched); sentence != "" {
				sentences = append(sentences, sentence)
				if !namedKinds && slices.Contains(tmpl.reasons, readreason.ReadError) {
					for _, entry := range entries {
						if entry.Kind != "*" && (entry.Reason == readreason.ReadError || entry.RetryWithKind) {
							retryIndex = len(sentences) - 1
							_, n := kindsLabel(entries)
							if n == 1 {
								retry = fmt.Sprintf("Retry with kind:%s before concluding absence.", entry.Kind)
							} else {
								retry = fmt.Sprintf("Retry with kind:%s for a kind you need before concluding absence.", entry.Kind)
							}
							break
						}
					}
				}
			}
		}
	}
	var other []string
	for _, gap := range res.Unsearched {
		if !used[gap.Reason] && !slices.Contains(other, string(gap.Reason)) {
			other = append(other, string(gap.Reason))
		}
	}
	if len(other) > 0 {
		sentences = append(sentences, fmt.Sprintf("Some kinds weren't searched (%s); see unsearched.", strings.Join(other, ", ")))
	}
	if retryIndex >= 0 && retryIndex < maxCoverageSentences {
		sentenceCount := len(sentences)
		if res.FilterErrors > 0 {
			sentenceCount++
		}
		if sentenceCount < maxCoverageSentences {
			sentences[retryIndex] += " " + retry
		} else {
			sentences[retryIndex] = strings.TrimSuffix(sentences[retryIndex], ".") + "; " + strings.ToLower(retry[:1]) + retry[1:]
		}
	}
	if extra := len(sentences) - maxCoverageSentences; extra > 0 {
		sentences = append(sentences[:maxCoverageSentences], fmt.Sprintf("+%d more %s.", extra, pluralWord(extra, "reason", "reasons")))
	}
	return strings.Join(sentences, " ")
}

// kindsLabel names the kinds entries stand for and how many there are: one
// named kind with its group, several as a count and the first few names, and
// "some kinds" (n = 0) when a "*" entry carries no count.
func kindsLabel(entries []search.UnsearchedKind) (label string, n int) {
	var named []string
	for _, e := range entries {
		switch {
		case e.Kind != "*":
			n++
			named = append(named, e.Kind)
		case e.Count > 0:
			n += e.Count
		default:
			return "some kinds", 0
		}
	}
	if n == 1 && len(named) == 1 {
		return kindWithGroup(entries[0]), 1
	}
	label = fmt.Sprintf("%d %s", n, pluralWord(n, "kind", "kinds"))
	if len(named) > 0 {
		shown := named[:min(len(named), 3)]
		if n > len(shown) {
			shown = append(shown, "…")
		}
		label += " (" + strings.Join(shown, ", ") + ")"
	}
	return label, n
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func kindWithGroup(e search.UnsearchedKind) string {
	if e.Group == "" {
		return e.Kind
	}
	return fmt.Sprintf("%s (%s)", e.Kind, e.Group)
}

// joinKinds lists the named kinds of entries, at most five, in prose.
func joinKinds(entries []search.UnsearchedKind, conj string, format func(string) string) string {
	var names []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Kind == "*" {
			continue
		}
		name := format(e.Kind)
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(names) < 5 {
			names = append(names, name)
		}
	}
	if len(seen) > len(names) {
		return boundedCoverageNames(names, len(seen))
	}
	switch len(names) {
	case 0:
		return "some kinds"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " " + conj + " " + names[len(names)-1]
}

func quoteJoin(values []string) string {
	quoted := make([]string, min(len(values), 5))
	for i, v := range values[:len(quoted)] {
		quoted[i] = "'" + v + "'"
	}
	return boundedCoverageNames(quoted, len(values))
}

func boundedCoverageNames(shown []string, total int) string {
	text := strings.Join(shown, ", ")
	if total > len(shown) {
		text += fmt.Sprintf(" and %d more", total-len(shown))
	}
	return text
}

func namespaceSentence(entries []search.UnsearchedKind, oneWhy, manyWhy string) string {
	var namespaces []string
	seen := map[string]bool{}
	for _, e := range entries {
		for _, ns := range e.Namespaces {
			if !seen[ns] {
				seen[ns] = true
				namespaces = append(namespaces, ns)
			}
		}
	}
	slices.Sort(namespaces)
	if len(namespaces) == 1 {
		return fmt.Sprintf("Namespace %s wasn't searched: %s.", namespaces[0], oneWhy)
	}
	return fmt.Sprintf("Namespaces %s weren't searched: %s.", boundedCoverageNames(namespaces[:min(len(namespaces), 5)], len(namespaces)), manyWhy)
}

// celNoSuchKey matches CEL's missing-field message so the hint can name the field.
var celNoSuchKey = regexp.MustCompile(`no such key: [A-Za-z0-9_.-]+`)

func filterSentence(failed int, sample string) string {
	cause := sample
	if m := celNoSuchKey.FindString(sample); m != "" {
		cause = m
	}
	if len(cause) > 80 {
		cause = strings.ToValidUTF8(cause[:80], "") + "…"
	}
	objects := pluralWord(failed, "object", "objects")
	const advice = "Kubernetes omits fields that are 0 or empty, so guard them with has() and match a zero with kind == \"K\" && (!has(x) || x == 0)."
	if cause == "" {
		return fmt.Sprintf("%d %s failed the filter: %s", failed, objects, advice)
	}
	return fmt.Sprintf("%d %s failed the filter (%s): %s", failed, objects, cause, advice)
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// kindPlural applies regular English plural rules to a Kind name, leaving a
// name that already reads as a plural (Endpoints) alone.
func kindPlural(kind string) string {
	lower := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(lower, "ss"), strings.HasSuffix(lower, "x"), strings.HasSuffix(lower, "ch"), strings.HasSuffix(lower, "sh"):
		return kind + "es"
	case strings.HasSuffix(lower, "s"):
		return kind
	case len(lower) > 1 && strings.HasSuffix(lower, "y") && !strings.ContainsRune("aeiou", rune(lower[len(lower)-2])):
		return kind[:len(kind)-1] + "ies"
	}
	return kind + "s"
}
