package browsercookies

import (
	"cmp"
	"strings"
)

// cookieClass ranks one cookie name by the evidence it gives of a signed-in
// Pixieset session.
type cookieClass int

const (
	classOther cookieClass = iota
	classToken
	classSession
)

// sessionCookieNames lists the session cookie names that hold no "sess" part.
var sessionCookieNames = map[string]bool{
	"wsid":    true,
	"gd_ca":   true,
	"idp_sid": true,
}

// classify sorts one cookie name. The rule is soft: a name that Pixieset
// renames becomes classOther and still counts in the total, so the tool keeps
// its selection when a name changes.
func classify(name string) cookieClass {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "sess") || sessionCookieNames[lower]:
		return classSession
	case strings.Contains(lower, "xsrf") || strings.Contains(lower, "csrf"):
		return classToken
	default:
		return classOther
	}
}

// cookieScore counts the evidence that one group of Pixieset cookies holds.
type cookieScore struct {
	session int
	token   int
	total   int
}

// add counts one more cookie name.
func (s cookieScore) add(name string) cookieScore {
	switch classify(name) {
	case classSession:
		s.session++
	case classToken:
		s.token++
	}
	s.total++
	return s
}

func (s cookieScore) empty() bool { return s.total == 0 }

// compare reads the counts from left to right: session cookies first, then
// token cookies, then the total. It gives a negative number when s is weaker
// than other, and a positive number when s is stronger.
func (s cookieScore) compare(other cookieScore) int {
	if result := cmp.Compare(s.session, other.session); result != 0 {
		return result
	}
	if result := cmp.Compare(s.token, other.token); result != 0 {
		return result
	}
	return cmp.Compare(s.total, other.total)
}

// candidateGroups holds the score of each cookie group of one candidate. The
// key is empty for the cookies that have no container.
type candidateGroups map[string]cookieScore

// best selects the group with the most evidence. A tie keeps the group with no
// container, then the first container in alphabetical order, so the result is
// always the same for the same input.
func (g candidateGroups) best() (string, cookieScore) {
	var name string
	var best cookieScore
	found := false
	for candidate, score := range g {
		if !found || betterGroup(candidate, score, name, best) {
			name, best, found = candidate, score, true
		}
	}
	return name, best
}

func betterGroup(name string, score cookieScore, otherName string, other cookieScore) bool {
	if result := score.compare(other); result != 0 {
		return result > 0
	}
	if (name == "") != (otherName == "") {
		return name == ""
	}
	return name < otherName
}
