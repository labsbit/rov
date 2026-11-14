package config

import "strings"

// aliases maps keys people reach for from other tools to rov's names.
var aliases = map[string]string{
	"command":     "cmd",
	"run":         "cmd",
	"dir":         "cwd",
	"workdir":     "cwd",
	"working_dir": "cwd",
	"environment": "env",
	"desc":        "description",
	"compose":     "docker",
	"logdir":      "log_dir",
	"log-dir":     "log_dir",
	"logmax":      "log_max",
	"log-max":     "log_max",
	"process":     "processes",
	"services":    "processes",
}

// suggest returns the key of valid that key is most likely a typo of, or "".
func suggest(key string, valid []string) string {
	lk := strings.ToLower(key)
	if a, ok := aliases[lk]; ok {
		for _, v := range valid {
			if v == a {
				return a
			}
		}
	}
	best, bestDist := "", 3 // at most two edits away
	for _, v := range valid {
		if d := editDistance(lk, v); d < bestDist {
			best, bestDist = v, d
		}
	}
	if best != "" {
		return best
	}
	// "comand" is three edits from "cmd" but still obviously meant it: accept
	// a valid key whose letters appear in order in the typo, if only one does.
	var hits []string
	for _, v := range valid {
		if subsequence(v, lk) {
			hits = append(hits, v)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

func subsequence(short, long string) bool {
	i := 0
	for j := 0; j < len(long) && i < len(short); j++ {
		if long[j] == short[i] {
			i++
		}
	}
	return i == len(short)
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
