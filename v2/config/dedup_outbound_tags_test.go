package config

import (
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestBuildConfigDropsDuplicateOutboundTags(t *testing.T) {
	input := option.Options{Outbounds: []option.Outbound{
		{Type: C.TypeDirect, Tag: "node-a"},
		{Type: C.TypeDirect, Tag: "node-a"},
		{Type: C.TypeDirect, Tag: "node-b"},
	}}

	out, err := BuildConfig(*DefaultMartenOptions(), input)
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}

	var nodeACount int
	var selector *option.Outbound
	for i := range out.Outbounds {
		if out.Outbounds[i].Tag == "node-a" {
			nodeACount++
		}
		if out.Outbounds[i].Tag == OutboundSelectTag {
			selector = &out.Outbounds[i]
		}
	}
	if nodeACount != 1 {
		t.Fatalf("node-a outbound count = %d, want 1", nodeACount)
	}
	if selector == nil {
		t.Fatal("selector outbound not found")
	}
	if got := selector.SelectorOptions.Outbounds; len(got) != 2 || got[0] != "node-a" || got[1] != "node-b" {
		t.Fatalf("selector outbounds = %#v, want [node-a node-b]", got)
	}
}
