package config

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func encodeURI(t *testing.T, opts option.TurncoatOutboundOptions, tag string, enc *base64.Encoding) string {
	t.Helper()
	payload, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("marshal opts: %v", err)
	}
	uri := "turncoat://" + enc.EncodeToString(payload)
	if tag != "" {
		uri += "#" + tag
	}
	return uri
}

func sampleOptions() option.TurncoatOutboundOptions {
	return option.TurncoatOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     "1.2.3.4",
			ServerPort: 51820,
		},
		CallURLs: []string{"https://example.com/call/abc"},
		Providers: []option.TurncoatProvider{{
			Hosts: []string{"provider.example"},
			Credentials: []option.TurncoatAppCredentials{
				{ClientID: "cid", ClientSecret: "csecret"},
			},
		}},
		Sessions: 2,
		UseUDP:   true,
	}
}

func TestParseTurncoatURI_RoundTrip(t *testing.T) {
	in := sampleOptions()
	uri := encodeURI(t, in, "MyServer", base64.RawURLEncoding)

	ob, err := ParseTurncoatURI(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ob.Type != C.TypeTurncoat {
		t.Fatalf("type = %q, want %q", ob.Type, C.TypeTurncoat)
	}
	if ob.Tag != "MyServer" {
		t.Fatalf("tag = %q, want %q", ob.Tag, "MyServer")
	}
	if ob.TurncoatOptions.Server != in.Server || ob.TurncoatOptions.ServerPort != in.ServerPort {
		t.Fatalf("server mismatch: got %s:%d", ob.TurncoatOptions.Server, ob.TurncoatOptions.ServerPort)
	}
	if len(ob.TurncoatOptions.CallURLs) != 1 || ob.TurncoatOptions.CallURLs[0] != in.CallURLs[0] {
		t.Fatalf("call_urls mismatch: %+v", ob.TurncoatOptions.CallURLs)
	}
	if len(ob.TurncoatOptions.Providers) != 1 || ob.TurncoatOptions.Providers[0].Hosts[0] != "provider.example" {
		t.Fatalf("providers mismatch: %+v", ob.TurncoatOptions.Providers)
	}
	if ob.TurncoatOptions.Sessions != 2 || !ob.TurncoatOptions.UseUDP {
		t.Fatalf("scalar fields lost: %+v", ob.TurncoatOptions)
	}
}

func TestParseTurncoatURI_DefaultTag(t *testing.T) {
	uri := encodeURI(t, sampleOptions(), "", base64.RawURLEncoding)
	ob, err := ParseTurncoatURI(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ob.Tag != "turncoat" {
		t.Fatalf("default tag = %q, want %q", ob.Tag, "turncoat")
	}
}

func TestParseTurncoatURI_TagURLEncoded(t *testing.T) {
	uri := encodeURI(t, sampleOptions(), "Berlin%20%231", base64.RawURLEncoding)
	ob, err := ParseTurncoatURI(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ob.Tag != "Berlin #1" {
		t.Fatalf("tag = %q, want %q", ob.Tag, "Berlin #1")
	}
}

func TestParseTurncoatURI_AcceptsAllBase64Variants(t *testing.T) {
	in := sampleOptions()
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		uri := encodeURI(t, in, "tag", enc)
		if _, err := ParseTurncoatURI(uri); err != nil {
			t.Errorf("encoding %v: parse failed: %v", enc, err)
		}
	}
}

func TestParseTurncoatURI_RejectsBadScheme(t *testing.T) {
	if _, err := ParseTurncoatURI("vless://abcd#tag"); err == nil {
		t.Fatal("expected error on non-turncoat scheme")
	}
}

func TestParseTurncoatURI_RejectsEmptyPayload(t *testing.T) {
	if _, err := ParseTurncoatURI("turncoat://#tag"); err == nil {
		t.Fatal("expected error on empty payload")
	}
}

func TestParseTurncoatURI_RejectsBadBase64(t *testing.T) {
	if _, err := ParseTurncoatURI("turncoat://!!!not-base64!!!"); err == nil {
		t.Fatal("expected error on invalid base64")
	}
}

func TestParseTurncoatURI_RejectsBadJSON(t *testing.T) {
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("not json at all"))
	if _, err := ParseTurncoatURI("turncoat://" + notJSON); err == nil {
		t.Fatal("expected error on payload that decodes to non-JSON")
	}
}

func TestTurncoat2Sing_SingleURI(t *testing.T) {
	uri := encodeURI(t, sampleOptions(), "tag1", base64.RawURLEncoding)
	out, err := Turncoat2Sing(uri)
	if err != nil {
		t.Fatalf("Turncoat2Sing: %v", err)
	}
	if !strings.Contains(out, `"type": "turncoat"`) {
		t.Fatalf("expected turncoat outbound in output, got:\n%s", out)
	}
	if !strings.Contains(out, `"tag": "tag1"`) {
		t.Fatalf("expected tag1 in output, got:\n%s", out)
	}
}

func TestTurncoat2Sing_MultipleURIsWithCommentsAndBlankLines(t *testing.T) {
	uri1 := encodeURI(t, sampleOptions(), "a", base64.RawURLEncoding)
	uri2 := encodeURI(t, sampleOptions(), "b", base64.RawURLEncoding)
	content := "# header comment\n" +
		"\n" +
		uri1 + "\n" +
		"// inline comment\n" +
		uri2 + "\n"

	out, err := Turncoat2Sing(content)
	if err != nil {
		t.Fatalf("Turncoat2Sing: %v", err)
	}
	if !strings.Contains(out, `"tag": "a"`) || !strings.Contains(out, `"tag": "b"`) {
		t.Fatalf("expected both tags, got:\n%s", out)
	}
}

func TestTurncoat2Sing_RejectsMixedContent(t *testing.T) {
	uri := encodeURI(t, sampleOptions(), "a", base64.RawURLEncoding)
	content := uri + "\nvless://something@example.com\n"
	if _, err := Turncoat2Sing(content); err == nil {
		t.Fatal("expected error on mixed-protocol content")
	}
}

func TestTurncoat2Sing_RejectsEmptyContent(t *testing.T) {
	if _, err := Turncoat2Sing("# only a comment\n\n"); err == nil {
		t.Fatal("expected error when no turncoat URIs are present")
	}
}

func TestParseConfigContent_TurncoatBeforeRay2Sing(t *testing.T) {
	uri := encodeURI(t, sampleOptions(), "edge", base64.RawURLEncoding)
	out, err := ParseConfigContent(uri, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	if !strings.Contains(string(out), `"type": "turncoat"`) {
		t.Fatalf("expected turncoat outbound in output, got:\n%s", out)
	}
	if !strings.Contains(string(out), `"tag": "edge"`) {
		t.Fatalf("expected edge tag in output, got:\n%s", out)
	}
}
