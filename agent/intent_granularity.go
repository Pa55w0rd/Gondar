package agent

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var seedListItem = regexp.MustCompile(`(?m)^\s*(?:[-*+]\s+|[0-9]+[.)、]\s*|#{1,6}\s+)`)

// SeedIntentSummary offers a short, single-direction fast path. A full scenario
// stays in the task root for planning, with all original scope and constraints.
// These are routing heuristics, not a claim to infer intent from natural language.
func SeedIntentSummary(description, goal string) (string, bool) {
	goal, description = strings.TrimSpace(goal), strings.TrimSpace(description)
	if goal == "" {
		return "", false
	}
	for _, text := range []string{goal, description} {
		if utf8.RuneCountInString(text) > 240 || strings.Count(text, "\n") > 1 || seedListItem.MatchString(text) || strings.Count(text, "；")+strings.Count(text, ";") > 1 {
			return "", false
		}
	}
	summary := "完成任务目标：" + goal
	if description != "" && description != goal {
		summary += fmt.Sprintf("（任务：%s）", description)
	}
	if utf8.RuneCountInString(summary) > 400 {
		return "", false
	}
	return summary, true
}

func repeatsComplexTask(summary, description, goal string) bool {
	if _, direct := SeedIntentSummary(description, goal); direct {
		return false
	}
	normalize := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	summary = normalize(summary)
	if summary == "" {
		return false
	}
	for _, original := range []string{goal, description, "完成任务目标：" + goal, fmt.Sprintf("完成任务目标：%s（任务：%s）", goal, description)} {
		if strings.TrimSpace(original) != "" && summary == normalize(original) {
			return true
		}
	}
	return false
}
