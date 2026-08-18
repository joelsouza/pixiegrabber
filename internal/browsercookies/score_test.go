package browsercookies

import (
	"testing"
	"time"
)

func TestClassifySortsCookieNames(t *testing.T) {
	tests := []struct {
		name string
		want cookieClass
	}{
		{name: "gallery_dashboard_session", want: classSession},
		{name: "PHPSESSID", want: classSession},
		{name: "wsid", want: classSession},
		{name: "gd_ca", want: classSession},
		{name: "IDP_SID", want: classSession},
		{name: "GD-XSRF-TOKEN", want: classToken},
		{name: "csrf_token", want: classToken},
		{name: "_ga", want: classOther},
		{name: "gd_ca_extra", want: classOther},
		// A renamed Pixieset cookie falls back to classOther. It still counts in
		// the total, so the tool keeps its selection.
		{name: "gd_gallery_login_v2", want: classOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.name); got != tt.want {
				t.Fatalf("classify(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestCookieScoreComparesFromLeftToRight(t *testing.T) {
	score := func(names ...string) cookieScore {
		var result cookieScore
		for _, name := range names {
			result = result.add(name)
		}
		return result
	}
	tests := []struct {
		name     string
		stronger cookieScore
		weaker   cookieScore
		equal    bool
	}{
		{
			name:     "a session cookie beats many other cookies",
			stronger: score("PHPSESSID"),
			weaker:   score("_ga", "_gid", "_fbp", "GD-XSRF-TOKEN"),
		},
		{
			name:     "a token beats a plain cookie",
			stronger: score("GD-XSRF-TOKEN"),
			weaker:   score("_ga", "_gid"),
		},
		{
			// Pixieset can rename every cookie. The group that holds cookies must
			// still beat the group that holds none.
			name:     "renamed cookies still beat an empty group",
			stronger: score("gd_gallery_login_v2"),
			weaker:   cookieScore{},
		},
		{
			name:     "the total separates two equal classes",
			stronger: score("PHPSESSID", "_ga"),
			weaker:   score("PHPSESSID"),
		},
		{
			name:     "two identical counts are equal",
			stronger: score("PHPSESSID", "GD-XSRF-TOKEN"),
			weaker:   score("gallery_dashboard_session", "csrf"),
			equal:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forward := tt.stronger.compare(tt.weaker)
			backward := tt.weaker.compare(tt.stronger)
			if tt.equal {
				if forward != 0 || backward != 0 {
					t.Fatalf("compare() = %d and %d, want both zero", forward, backward)
				}
				return
			}
			if forward <= 0 || backward >= 0 {
				t.Fatalf("compare() = %d and %d, want a positive and a negative number", forward, backward)
			}
		})
	}
}

func TestCandidateGroupsBestPrefersEvidenceThenNoContainer(t *testing.T) {
	tests := []struct {
		name   string
		groups candidateGroups
		want   string
	}{
		{name: "no group", groups: candidateGroups{}, want: ""},
		{
			name:   "the strongest group wins",
			groups: candidateGroups{"": {total: 4}, "3": {session: 1, total: 1}},
			want:   "3",
		},
		{
			name:   "an equal score keeps the group without a container",
			groups: candidateGroups{"": {session: 1, total: 1}, "3": {session: 1, total: 1}},
			want:   "",
		},
		{
			name:   "two equal containers keep the first name",
			groups: candidateGroups{"4": {session: 1, total: 1}, "3": {session: 1, total: 1}},
			want:   "3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The map order changes between runs, so the result must not.
			for range 8 {
				if got, _ := tt.groups.best(); got != tt.want {
					t.Fatalf("best() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestBetterEvidenceOrdersTiesInOneWay(t *testing.T) {
	older := time.Unix(1000, 0)
	newer := time.Unix(2000, 0)
	evidence := func(score cookieScore, modified time.Time, container string, isDefault bool, path string) candidateEvidence {
		return candidateEvidence{
			candidate: storeCandidate{path: path, isDefault: isDefault},
			container: container,
			score:     score,
			modified:  modified,
		}
	}
	session := cookieScore{session: 1, total: 1}
	tests := []struct {
		name   string
		better candidateEvidence
		worse  candidateEvidence
	}{
		{
			name:   "the score decides first",
			better: evidence(session, older, "", false, "z"),
			worse:  evidence(cookieScore{total: 9}, newer, "", true, "a"),
		},
		{
			name:   "an equal score keeps the newest store",
			better: evidence(session, newer, "3", false, "z"),
			worse:  evidence(session, older, "", true, "a"),
		},
		{
			// A store without cookies gives no useful time.
			name:   "no evidence keeps the stable order",
			better: evidence(cookieScore{}, older, "", true, "z"),
			worse:  evidence(cookieScore{}, newer, "", false, "a"),
		},
		{
			name:   "cookies without a container come first",
			better: evidence(session, older, "", false, "z"),
			worse:  evidence(session, older, "3", true, "a"),
		},
		{
			name:   "the default profile comes before the others",
			better: evidence(session, older, "", true, "z"),
			worse:  evidence(session, older, "", false, "a"),
		},
		{
			name:   "the first path in alphabetical order ends every tie",
			better: evidence(session, older, "", true, "a"),
			worse:  evidence(session, older, "", true, "z"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !betterEvidence(tt.better, tt.worse) {
				t.Fatal("betterEvidence() rejected the stronger candidate")
			}
			if betterEvidence(tt.worse, tt.better) {
				t.Fatal("betterEvidence() accepted the weaker candidate")
			}
		})
	}
}
