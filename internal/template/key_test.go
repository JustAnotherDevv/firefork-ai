package template

import "testing"

func TestParseKey(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantVer  string
		wantErr  bool
	}{
		{"python/v1", "python", "v1", false},
		{"llama-3.2-1b-q4/v1", "llama-3.2-1b-q4", "v1", false},
		// legacy firefork-CLI split-on-first vs fork-CLI
		// split-on-last disagreed on this input. ParseKey rejects it
		// outright now.
		{"foo/bar/v1", "", "", true},
		{"", "", "", true},
		{"noslash", "", "", true},
		{"/v1", "", "", true},
		{"name/", "", "", true},
		{"./v1", "", "", true},
		{"name/..", "", "", true},
		{"name\n/v1", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			n, v, err := ParseKey(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want err, got name=%q ver=%q", n, v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if n != c.wantName || v != c.wantVer {
				t.Fatalf("got %q/%q want %q/%q", n, v, c.wantName, c.wantVer)
			}
		})
	}
}
