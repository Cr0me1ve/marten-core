package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

// Marten extension: parser for the `turncoat://` share-link URI scheme.
//
// Format
//
//	turncoat://<base64url-json>[#<tag>]
//
// The opaque payload is base64url-encoded (padded or unpadded; std-base64 is
// also accepted as a fallback) JSON whose shape matches
// option.TurncoatOutboundOptions — the same JSON the outbound consumes inside
// a sing-box config, minus the `type` discriminator. The fragment is the
// outbound tag; if absent or empty the tag defaults to "turncoat".
//
// We deliberately keep the payload opaque rather than splitting fields into
// query parameters: the outbound's `providers` list (per-outbound auth-backend
// descriptors) is itself nested JSON, and stuffing it into URL params would
// require either re-inventing a JSON-in-URL grammar or losing fidelity. One
// base64-JSON blob is unambiguous, lossless, and trivially round-trippable.
//
// Subscription form: a content blob whose every non-blank, non-comment line is
// a `turncoat://` URI is treated as a Marten-native subscription and turned
// into a sing-box config containing one outbound per URI. Comment lines start
// with `#` or `//`. Mixed-protocol subscriptions are not supported here — the
// caller's existing fall-through to ray2sing handles those.

// Turncoat2Sing converts a subscription whose lines are all `turncoat://` URIs
// (with comments and blank lines tolerated) into a sing-box config JSON
// string. Returns an error if any non-blank, non-comment line is not a
// turncoat:// URI, or if no URI is found — both cases signal "not a
// turncoat-only subscription" so the caller can try other parsers.
func Turncoat2Sing(content string) (string, error) {
	lines := strings.Split(content, "\n")
	var outbounds []option.Outbound
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if !strings.HasPrefix(line, "turncoat://") {
			return "", fmt.Errorf("[TurncoatParser] line %d: not a turncoat:// URI", i+1)
		}
		ob, err := ParseTurncoatURI(line)
		if err != nil {
			return "", fmt.Errorf("[TurncoatParser] line %d: %w", i+1, err)
		}
		outbounds = append(outbounds, *ob)
	}
	if len(outbounds) == 0 {
		return "", fmt.Errorf("[TurncoatParser] no turncoat:// URIs found")
	}
	out := map[string]interface{}{"outbounds": outbounds}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("[TurncoatParser] marshal: %w", err)
	}
	return string(b), nil
}

// ParseTurncoatURI parses a single `turncoat://<base64url-json>[#tag]` URI
// into a sing-box outbound. Exposed for callers that want to splice a single
// turncoat outbound into an otherwise heterogeneous subscription.
func ParseTurncoatURI(rawURI string) (*option.Outbound, error) {
	if !strings.HasPrefix(rawURI, "turncoat://") {
		return nil, fmt.Errorf("scheme is not turncoat://")
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return nil, fmt.Errorf("parse URI: %w", err)
	}
	// `turncoat://payload` puts the payload in u.Host; `turncoat://payload/`
	// or anything trailing puts the rest into u.Path. Concatenate to recover
	// the full opaque token (base64url has no `/` so this is unambiguous —
	// but if a future format gains a path component, redesign here).
	payload := u.Host
	if u.Path != "" && u.Path != "/" {
		payload += strings.TrimSuffix(u.Path, "/")
	}
	if payload == "" {
		return nil, fmt.Errorf("empty payload")
	}
	decoded, err := decodeBase64Lenient(payload)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	var opts option.TurncoatOutboundOptions
	if err := json.Unmarshal(decoded, &opts); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
	}
	tag := u.Fragment
	if dec, errUnesc := url.QueryUnescape(tag); errUnesc == nil {
		tag = dec
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "turncoat"
	}
	return &option.Outbound{
		Type:            C.TypeTurncoat,
		Tag:             tag,
		TurncoatOptions: opts,
	}, nil
}

// decodeBase64Lenient tries the four common base64 variants users pick when
// hand-crafting share links: URL-safe and standard, each with and without
// padding. Returns the first successful decode; otherwise the last error.
func decodeBase64Lenient(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64 (tried url/std, padded/unpadded)")
}
