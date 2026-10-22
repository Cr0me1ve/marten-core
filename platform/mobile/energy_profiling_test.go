package mobile

import (
	"strings"
	"testing"

	"github.com/google/pprof/profile"
)

func TestSanitizeEnergyProfileDropsDynamicMetadataAndPrivatePaths(t *testing.T) {
	function := &profile.Function{
		ID:         1,
		Name:       "github.com/private-owner/pkg.(*Worker).run",
		SystemName: "private-system-symbol",
		Filename:   "/Users/example/private-worktree/pkg/worker.go",
		StartLine:  9,
	}
	location := &profile.Location{
		ID:      1,
		Mapping: &profile.Mapping{ID: 1, Start: 0x12340000, Limit: 0x12350000, Offset: 4096, File: "/private/build/libmarten-core.so", BuildID: "private-build-id"},
		Address: 0x12345678,
		Line:    []profile.Line{{Function: function, Line: 42}},
	}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{{
			Location: []*profile.Location{location},
			Value:    []int64{5},
			Label:    map[string][]string{"user": {"private-user"}, "request": {"private-request"}},
			NumLabel: map[string][]int64{"object_id": {67890}},
			NumUnit:  map[string][]string{"object_id": {"private-unit"}},
		}},
		Mapping:  []*profile.Mapping{location.Mapping},
		Location: []*profile.Location{location},
		Function: []*profile.Function{function},
		Comments: []string{"private comment"}, DropFrames: "private-drop-regexp", KeepFrames: "private-keep-regexp",
	}

	sanitizeEnergyProfile(p)

	if len(p.Comments) != 0 || p.DropFrames != "" || p.KeepFrames != "" {
		t.Fatalf("profile comments and frame filters must be removed: %+v", p)
	}
	if len(p.Sample[0].Label) != 0 || len(p.Sample[0].NumLabel) != 0 || len(p.Sample[0].NumUnit) != 0 {
		t.Fatalf("dynamic sample labels must be removed: %+v", p.Sample[0])
	}
	if function.Name != "pkg.(*Worker).run" || function.SystemName != function.Name || function.Filename != "worker.go" {
		t.Fatalf("function metadata was not sanitized: %+v", function)
	}
	if location.Address != 0 || location.Mapping.File != "libmarten-core.so" || location.Mapping.BuildID != "" || location.Mapping.Start != 0 || location.Mapping.Limit != 0 || location.Mapping.Offset != 0 {
		t.Fatalf("address mapping metadata was not sanitized: %+v", location)
	}
	serialized := strings.Join([]string{function.Name, function.SystemName, function.Filename, location.Mapping.File}, " ")
	for _, private := range []string{"private-owner", "private-module", "/Users/", "private-worktree", "private-build-id", "12345678"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("sanitized profile retained %q: %s", private, serialized)
		}
	}
}

func TestSummarizeEnergyProfileReportsSortedFlatAndInclusiveRowsWithLimit(t *testing.T) {
	root := &profile.Function{Name: "pkg.root", Filename: "/private/tree/root.go"}
	leaf := &profile.Function{Name: "pkg.leaf", Filename: "/private/tree/leaf.go"}
	other := &profile.Function{Name: "pkg.other", Filename: "/private/tree/other.go"}
	rootLocation := func(id uint64, f *profile.Function, line int64) *profile.Location {
		return &profile.Location{ID: id, Line: []profile.Line{{Function: f, Line: line}}}
	}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{rootLocation(1, root, 10), rootLocation(2, leaf, 20)}, Value: []int64{100}},
			{Location: []*profile.Location{rootLocation(3, root, 10), rootLocation(4, leaf, 20)}, Value: []int64{50}},
			{Location: []*profile.Location{rootLocation(5, other, 30)}, Value: []int64{70}},
		},
	}

	got := summarizeEnergyProfile(p, 0, 2)
	if !strings.Contains(got, "PROFILE unit=nanoseconds total=220") {
		t.Fatalf("summary total missing or incorrect: %s", got)
	}
	if !strings.Contains(got, "FUNCTION flat=150 inclusive=150 symbol=pkg::root source=root.go:10 flat_pct=68.18 inclusive_pct=68.18") {
		t.Fatalf("root flat and inclusive values missing: %s", got)
	}
	if !strings.Contains(got, "FUNCTION flat=0 inclusive=150 symbol=pkg::leaf source=leaf.go:20 flat_pct=0.00 inclusive_pct=68.18") {
		t.Fatalf("leaf inclusive value missing: %s", got)
	}
	if strings.Contains(got, "symbol=pkg::other") {
		t.Fatalf("maxRows must bound function rows: %s", got)
	}
	if strings.Contains(got, "/private/tree") {
		t.Fatalf("summary must show file basenames only: %s", got)
	}
	if !strings.Contains(got, "STACK value=150 frames=pkg::root:10 <- pkg::leaf:20") {
		t.Fatalf("summary should include the sampled call chain: %s", got)
	}
}

func TestSummarizeEnergyProfileHandlesMissingSampleType(t *testing.T) {
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}}
	if got := summarizeEnergyProfile(p, -1, 10); got != "PROFILE outcome=no_sample_type\n" {
		t.Fatalf("unexpected missing sample type report: %q", got)
	}
	if got := summarizeEnergyProfile(p, 1, 10); got != "PROFILE outcome=no_sample_type\n" {
		t.Fatalf("unexpected out-of-range sample type report: %q", got)
	}
}

func TestSanitizeEnergyProfilePreservesCPUForBlankNativeFramesAsUnresolved(t *testing.T) {
	function := &profile.Function{ID: 1, Name: "", SystemName: "", Filename: ""}
	location := &profile.Location{ID: 1, Line: []profile.Line{{Function: function}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Sample:     []*profile.Sample{{Location: []*profile.Location{location}, Value: []int64{6, 7_500_000}}},
		Location:   []*profile.Location{location},
		Function:   []*profile.Function{function},
	}

	sanitizeEnergyProfile(p)

	if function.Name != "unresolved_native_frame" || function.Filename != "unknown_source" {
		t.Fatalf("blank native frame should receive fixed safe markers: name=%q source=%q", function.Name, function.Filename)
	}
	if got := p.Sample[0].Value[1]; got != 7_500_000 {
		t.Fatalf("sanitization must preserve unknown native CPU cost: got %d ns", got)
	}
	got := summarizeEnergyProfile(p, 1, 10)
	if !strings.Contains(got, "PROFILE unit=nanoseconds total=7500000") {
		t.Fatalf("unknown frame cost must remain in the profile total: %s", got)
	}
	if !strings.Contains(got, "symbol=unresolved_native_frame source=unknown_source") {
		t.Fatalf("summary should attribute cost to the fixed unresolved marker: %s", got)
	}
	if strings.Contains(got, "symbol= source=") || strings.Contains(got, "symbol=unknown_source") {
		t.Fatalf("summary must not emit a blank function row: %s", got)
	}
}

func TestEnergyBoundedWriterCapsDataAndRecordsOverflow(t *testing.T) {
	w := &energyBoundedWriter{limit: 5}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("first write = (%d, %v), want (3, nil)", n, err)
	}
	if n, err := w.Write([]byte("def")); n != 3 || err != nil {
		t.Fatalf("overflow write = (%d, %v), want (3, nil)", n, err)
	}
	if !w.overflow || w.String() != "abc" {
		t.Fatalf("writer should retain only bounded bytes and report overflow: overflow=%t data=%q", w.overflow, w.String())
	}
	if n, err := w.Write([]byte("g")); n != 1 || err != nil || w.String() != "abc" {
		t.Fatalf("writes after overflow must be discarded with full write counts: (%d, %v), data=%q", n, err, w.String())
	}
}
