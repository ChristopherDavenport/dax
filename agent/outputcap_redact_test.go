package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentredact"
	"github.com/ChristopherDavenport/agentredact/publish"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// keptSecret is a fake AWS access key id, the shape agentpolicy's
// default secrets name.
const keptSecret = "AKIAFAKEFAKEFAKEFAKE"

// leaky is an extension whose tool returns a megabyte with a secret in
// the middle, where the cut leaves it out of the result and keeps it in
// the whole.
func leaky() extension.Extension {
	return extension.Extension{Name: "leaky", Tools: func(extension.ToolEnv) []agenttool.Tool {
		return []agenttool.Tool{agenttool.New("dump", "dump a lot", func(context.Context, struct{}) (string, error) {
			half := strings.Repeat("x", 1<<19)
			return half + "\naws_key " + keptSecret + "\n" + half, nil
		})}
	}}
}

// keptBlob is the hash the session's dax.output_kept entry names, "" for
// none.
func keptBlob(t *testing.T, s *agentsession.Session) string {
	t.Helper()
	for _, e := range s.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != outputKeptNS {
			continue
		}
		var k outputKept
		if err := json.Unmarshal(c.Data, &k); err != nil {
			t.Fatal(err)
		}
		hash, ok := strings.CutPrefix(k.Blob, "sidecar:")
		if !ok || !agentsession.ValidHash(hash) {
			t.Fatalf("dax.output_kept names %q", k.Blob)
		}
		return hash
	}
	return ""
}

// A kept output (#172) holds the whole of what a tool returned, a
// secret the cut left out included. A redaction pass over dax's store,
// as agentredact's command runs it, redacts the kept blob through the
// profile's string rules, and a publish carries the redacted blob alone,
// never the original (agentredact#23).
func TestAKeptOutputIsPublishedOnlyRedacted(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"dump", `{}`}}})
	o.Extensions = append(o.Extensions, leaky())
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "dump it", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	for _, it := range s.ag.State().Transcript {
		if out, ok := it.(*openresponses.FunctionCallOutput); ok && strings.Contains(out.Output.Text, keptSecret) {
			t.Fatal("the cut result holds the secret; it must be in the kept whole alone")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := cas.Open(o.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	orig, err := st.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	origHash := keptBlob(t, orig)
	if origHash == "" {
		t.Fatal("the session kept no output")
	}
	if whole, _, err := agentsession.ReadSessionBlob(ctx, st, id, origHash); err != nil || !bytes.Contains(whole, []byte(keptSecret)) {
		t.Fatalf("the kept output does not hold the secret: %v", err)
	}

	key, err := agentredact.NewKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	p := agentredact.Profile{Name: "dax.test@1", Strings: []agentredact.StringRedactor{agentredact.DefaultPatterns()}}
	rep, err := agentredact.RedactStore(ctx, st, p, key, agentredact.StoreOptions{})
	if err != nil {
		t.Fatalf("RedactStore: %v", err)
	}
	var sr *agentredact.SessionReport
	for i := range rep.Sessions {
		if rep.Sessions[i].Original == id {
			sr = &rep.Sessions[i]
		}
	}
	if sr == nil || sr.Err != nil {
		t.Fatalf("the session's redaction: %+v", sr)
	}
	if sr.CarriesBlob(origHash) {
		t.Error("the report lets the original kept output leave")
	}
	red, err := st.Read(ctx, sr.Redaction)
	if err != nil {
		t.Fatal(err)
	}
	redHash := keptBlob(t, red)
	if redHash == "" || redHash == origHash {
		t.Fatalf("the redaction's dax.output_kept names %q, the original %q", redHash, origHash)
	}
	whole, _, err := agentsession.ReadSessionBlob(ctx, st, sr.Redaction, redHash)
	if err != nil || bytes.Contains(whole, []byte(keptSecret)) || !bytes.Contains(whole, []byte("aws_key [REDACTED")) {
		t.Fatalf("the redacted kept output (%d bytes, %v) still holds the secret or no placeholder", len(whole), err)
	}

	to := agentsession.NewMemoryStore(agentsession.MemoryRedactedOnly())
	res, err := publish.Publish(ctx, st, to, publish.Options{Profile: p.Name, Report: rep, Unjudged: true})
	if err != nil {
		t.Fatalf("Publish: %v (held %+v)", err, res)
	}
	published := false
	for _, pb := range res.Published {
		published = published || pb.Session == sr.Redaction
	}
	if !published {
		t.Fatalf("the session was not published: %+v", res.Held)
	}
	if _, err := to.Blob(ctx, origHash); err == nil {
		t.Error("the publish carried the original kept output")
	}
	if got, err := to.Blob(ctx, redHash); err != nil || bytes.Contains(got, []byte(keptSecret)) {
		t.Errorf("the published kept output: %v, holds the secret %v", err, bytes.Contains(got, []byte(keptSecret)))
	}
}
