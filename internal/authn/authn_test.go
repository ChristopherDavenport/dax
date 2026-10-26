package authn

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errUnauth = errors.New("unauthenticated")

// Each reading of the header against a set with an empty token in it,
// which is never known: the strict one the libraries' hooks had (only
// "Bearer " exactly), and A2A's (the scheme in any case, the token
// trimmed).
func TestTokens(t *testing.T) {
	tokens := New(map[string]string{"tok-a": "alice", "tok-b": "bob", "": "nobody"})
	strict := tokens.Authenticator(errUnauth)
	for _, tc := range []struct {
		name, header    string
		strict, lenient string // "" is refused
	}{
		{"no header", "", "", ""},
		{"a token", "Bearer tok-a", "alice", "alice"},
		{"another", "Bearer tok-b", "bob", "bob"},
		{"an unknown token", "Bearer tok-c", "", ""},
		{"a prefix of a token", "Bearer tok", "", ""},
		{"an empty token", "Bearer ", "", ""},
		{"the scheme alone", "Bearer", "", ""},
		{"the token alone", "tok-a", "", ""},
		{"another scheme", "Basic tok-a", "", ""},
		{"the scheme in lower case", "bearer tok-a", "", "alice"},
		{"the scheme in upper case", "BEARER tok-a", "", "alice"},
		{"space around the token", "Bearer  tok-a ", "", "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			p, err := strict(r)
			if tc.strict == "" {
				if err != errUnauth || p != "" {
					t.Errorf("strict: %q, %v; want refused with the library's error", p, err)
				}
			} else if err != nil || p != tc.strict {
				t.Errorf("strict: %q, %v; want %s", p, err, tc.strict)
			}
			p, err = tokens.Principal(r)
			if tc.lenient == "" {
				if !errors.Is(err, ErrNoToken) || p != "" {
					t.Errorf("A2A's: %q, %v; want refused", p, err)
				}
			} else if err != nil || p != tc.lenient {
				t.Errorf("A2A's: %q, %v; want %s", p, err, tc.lenient)
			}
		})
	}
	if _, err := New(map[string]string{}).Authenticator(errUnauth)(httptest.NewRequest(http.MethodGet, "/", nil)); err != errUnauth {
		t.Errorf("no tokens: %v", err)
	}
}

// The client sends its token to its origin alone: not to another host,
// another port or another scheme, nor across a redirect off the origin;
// the request it was given is not changed, and the base's redirect
// rule is kept.
func TestClientHoldsItsTokenToItsOrigin(t *testing.T) {
	seen := map[string]string{}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen["other"] = r.Header.Get("Authorization")
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("Authorization")
		if r.URL.Path == "/away" {
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		}
	}))
	defer origin.Close()
	c, err := Client(nil, origin.URL+"/base/", "tok-a")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/here", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen["/here"] != "Bearer tok-a" {
		t.Errorf("the origin got %q", seen["/here"])
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("the request given was changed")
	}
	resp, err = c.Get(other.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen["other"] != "" {
		t.Errorf("another origin got %q", seen["other"])
	}
	seen["other"] = "unset"
	resp, err = c.Get(origin.URL + "/away")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen["other"] != "" {
		t.Errorf("a redirect off the origin carried %q", seen["other"])
	}
	// A base that follows no redirect is kept as it is.
	stay := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c, _ = Client(stay, origin.URL, "tok-a")
	resp, err = c.Get(origin.URL + "/away")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("the base's redirect rule was lost: %d", resp.StatusCode)
	}
	if stay.Transport != nil {
		t.Error("the base client was changed")
	}
	if _, err := Client(nil, "not a url", "t"); err == nil || strings.Contains(err.Error(), "not a url") {
		t.Errorf("a URL with no origin: %v", err)
	}
}
