package mobile

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

func TestDecodeEnergyCaptureWritesSanitizedSummaryAndRemovesPrivateInputs(t *testing.T) {
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	externalDir := filepath.Join(root, "external")
	if err := os.MkdirAll(privateDir, 0700); err != nil {
		t.Fatal(err)
	}
	function := &profile.Function{
		ID: 1, Name: "github.com/private-owner/pkg.(*Worker).run", SystemName: "private-system-symbol",
		Filename: "/Users/private-worktree/pkg/worker.go", StartLine: 9,
	}
	calibration := &profile.Function{
		ID: 2, Name: "marten-core/platform/mobile.energyProfileCalibration", Filename: "/private/build/energy_capture.go", StartLine: 40,
	}
	mapping := &profile.Mapping{ID: 1, Start: 0x12340000, Limit: 0x12350000, Offset: 4096, File: "/private/build/libmarten-core.so", BuildID: "private-build-id"}
	location := &profile.Location{ID: 1, Mapping: mapping, Address: 0x12345678, Line: []profile.Line{{Function: function, Line: 42}}}
	calibrationLocation := &profile.Location{ID: 2, Mapping: mapping, Address: 0x12345690, Line: []profile.Line{{Function: calibration, Line: 40}}}
	capture := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{
				Location: []*profile.Location{location}, Value: []int64{2, 10_000_000},
				Label:    map[string][]string{"user": {"private-user"}, "request": {"private-request"}},
				NumLabel: map[string][]int64{"object_id": {67890}}, NumUnit: map[string][]string{"object_id": {"private-unit"}},
			},
			{Location: []*profile.Location{calibrationLocation}, Value: []int64{1, 5_000_000}},
		},
		Mapping: []*profile.Mapping{mapping}, Location: []*profile.Location{location, calibrationLocation}, Function: []*profile.Function{function, calibration},
		Comments: []string{"private comment"}, DropFrames: "private-drop-regexp", KeepFrames: "private-keep-regexp",
	}
	var raw bytes.Buffer
	if err := capture.Write(&raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, "core-0-cpu.raw"), raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(energyCaptureMetadata{
		Segment: 0, StartMS: 1000, EndMS: 61_000, DurationMS: 60_000, HeapBytes: 1234, HeapObjects: 12,
		AllocBytes: 512, GC: 1, GCPauseNS: 100, Goroutines: 3, Snapshots: false, GapUS: 125, DroppedSegments: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, "core-0.job"), metadata, 0600); err != nil {
		t.Fatal(err)
	}

	if err := DecodeEnergyCapture(privateDir, externalDir, "core-0.job"); err != nil {
		t.Fatalf("DecodeEnergyCapture: %v", err)
	}
	profilePath := filepath.Join(externalDir, "energy-profiles-v3", "core-cpu-0.pprof")
	profileBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := profile.ParseData(profileBytes)
	if err != nil {
		t.Fatalf("sanitized profile does not parse: %v", err)
	}
	if len(decoded.Sample) != 2 {
		t.Fatalf("sanitized pprof sample count = %d, want user and calibration samples", len(decoded.Sample))
	}
	for _, sample := range decoded.Sample {
		if len(sample.Label) != 0 || len(sample.NumLabel) != 0 || len(sample.NumUnit) != 0 {
			t.Fatalf("sanitized pprof retained dynamic sample metadata: %+v", sample)
		}
	}
	if decoded.Location[0].Address != 0 || decoded.Mapping[0].BuildID != "" || decoded.Mapping[0].Start != 0 || decoded.Mapping[0].Limit != 0 || decoded.Mapping[0].Offset != 0 {
		t.Fatalf("sanitized pprof retained address/build metadata: location=%+v mapping=%+v", decoded.Location[0], decoded.Mapping[0])
	}
	if len(decoded.Function) != 2 || decoded.Function[0].Name != "pkg.(*Worker).run" || decoded.Function[0].SystemName != "pkg.(*Worker).run" || decoded.Function[0].Filename != "worker.go" {
		t.Fatalf("sanitized pprof function metadata is wrong: %+v", decoded.Function)
	}
	if decoded.Mapping[0].File != "libmarten-core.so" {
		t.Fatalf("sanitized pprof mapping path is wrong: %+v", decoded.Mapping[0])
	}
	report, err := os.ReadFile(filepath.Join(externalDir, "energy-profiles-v3", "core-report-0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(report)
	for _, expected := range []string{"schema=3", "segment=0", "calibration_window=true", "calibration_seen=true", "total_alloc_delta_bytes=512", "sampled_cpu_ms=15", "pkg::(*Worker)::run", "source=worker.go:42", "peak_window=false", "CORE_RECORDING rotation_gap_us=125 dropped_segments=2 continuous=true", "CORE_CPU_COVERAGE total_sampled_cpu_ms=15 unresolved_leaf_cpu_ms=0 explicit_profiler_cpu_ms=5 indirect_overhead_not_subtracted=true"} {
		if !strings.Contains(text, expected) {
			t.Errorf("report missing %q: %s", expected, text)
		}
	}
	applicationHeading := strings.Index(text, "CPU APPLICATION FUNCTION PROFILE")
	profilerHeading := strings.Index(text, "CPU PROFILER FUNCTION PROFILE")
	workerRow := strings.Index(text, "symbol=pkg::(*Worker)::run")
	calibrationRow := strings.Index(text, "symbol=marten-core/platform/mobile::energyProfileCalibration")
	if applicationHeading < 0 || profilerHeading <= applicationHeading || workerRow <= applicationHeading || workerRow >= profilerHeading || calibrationRow <= profilerHeading {
		t.Fatalf("application and calibration costs must be separated into labeled profile sections: %s", text)
	}
	if strings.Contains(text[applicationHeading:profilerHeading], "energyProfileCalibration") {
		t.Fatalf("calibration samples must not be presented as application CPU causes: %s", text)
	}
	if strings.Contains(text[profilerHeading:], "symbol=pkg::(*Worker)::run") {
		t.Fatalf("ordinary application CPU samples must not be attributed to the explicit profiler section: %s", text)
	}
	if !strings.Contains(text[applicationHeading:profilerHeading], "symbol=pkg::(*Worker)::run") {
		t.Fatalf("application section should retain the non-profiler CPU sample: %s", text)
	}
	for _, private := range []string{"private-owner", "private-system-symbol", "private-worktree", "/Users/", "/private/", "private-build-id", "12345678", "private-user", "private-request", "private-unit", "private comment", "object_id"} {
		if strings.Contains(text, private) {
			t.Errorf("report retained private value %q: %s", private, text)
		}
	}
	if _, err := os.Stat(filepath.Join(privateDir, "core-0-cpu.raw")); !os.IsNotExist(err) {
		t.Errorf("raw CPU input should be removed, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateDir, "core-0.job")); !os.IsNotExist(err) {
		t.Errorf("consumed job marker should be removed, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(externalDir, "energy-profiles-v3", "core-cpu-peak.pprof")); !os.IsNotExist(err) {
		t.Errorf("calibration window must be excluded from peak state, stat error = %v", err)
	}
	historyFile, err := os.Open(filepath.Join(externalDir, "energy-profiles-v3", "core-history.txt.gz"))
	if err != nil {
		t.Fatalf("sanitized full-history stream missing: %v", err)
	}
	reader, err := gzip.NewReader(historyFile)
	if err != nil {
		historyFile.Close()
		t.Fatalf("open report history: %v", err)
	}
	history, err := io.ReadAll(reader)
	reader.Close()
	historyFile.Close()
	if err != nil {
		t.Fatalf("read report history: %v", err)
	}
	if count := bytes.Count(history, []byte("MARTEN_PROFILE schema=3 process=core")); count != 1 {
		t.Fatalf("one decoded segment should add exactly one full report to history, got %d markers: %s", count, history)
	}
}

func TestDecodeEnergyCaptureKeepsRowsPastCompactLimitInFullHistory(t *testing.T) {
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	externalDir := filepath.Join(root, "external")
	if err := os.MkdirAll(privateDir, 0700); err != nil {
		t.Fatal(err)
	}
	capture := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
	}
	for index := 0; index < 41; index++ {
		id := uint64(index + 1)
		name := fmt.Sprintf("pkg.Function_%02d", index)
		if index == 40 {
			name = "zzRareFunction"
		}
		function := &profile.Function{ID: id, Name: name, Filename: "functions.go", StartLine: int64(index + 1)}
		location := &profile.Location{ID: id, Line: []profile.Line{{Function: function, Line: int64(index + 1)}}}
		capture.Function = append(capture.Function, function)
		capture.Location = append(capture.Location, location)
		capture.Sample = append(capture.Sample, &profile.Sample{Location: []*profile.Location{location}, Value: []int64{1, 1_000_000}})
	}
	var raw bytes.Buffer
	if err := capture.Write(&raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, "core-1-cpu.raw"), raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(energyCaptureMetadata{Segment: 1, StartMS: 60_000, EndMS: 120_000, DurationMS: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, "core-1.job"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := DecodeEnergyCapture(privateDir, externalDir, "core-1.job"); err != nil {
		t.Fatalf("DecodeEnergyCapture: %v", err)
	}

	profileDir := filepath.Join(externalDir, "energy-profiles-v3")
	compact, err := os.ReadFile(filepath.Join(profileDir, "core-report-1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	applicationEnd := bytes.Index(compact, []byte("CPU PROFILER FUNCTION PROFILE"))
	if applicationEnd < 0 {
		t.Fatalf("compact report is missing separated CPU sections: %s", compact)
	}
	application := compact[bytes.Index(compact, []byte("CPU APPLICATION FUNCTION PROFILE")):applicationEnd]
	if bytes.Contains(application, []byte("zzRareFunction")) {
		t.Fatalf("compact application summary should stay within its 40-row limit: %s", application)
	}

	historyFile, err := os.Open(filepath.Join(profileDir, "core-history.txt.gz"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(historyFile)
	if err != nil {
		historyFile.Close()
		t.Fatal(err)
	}
	history, err := io.ReadAll(reader)
	reader.Close()
	historyFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(history, []byte("symbol=zzRareFunction")) {
		t.Fatalf("full history should preserve function rows omitted from compact display: %s", history)
	}
}

func TestDecodeEnergyCaptureRejectsUntrustedJobPathsAndNames(t *testing.T) {
	for _, job := range []string{"../core-0.job", "core-6.job", "core-x.job", "core-00.job", "/tmp/core-0.job", "other-0.job", "core-0.job/../secret"} {
		t.Run(job, func(t *testing.T) {
			err := DecodeEnergyCapture(t.TempDir(), t.TempDir(), job)
			if err == nil || !strings.Contains(err.Error(), "invalid capture job") {
				t.Fatalf("DecodeEnergyCapture(%q) error = %v, want invalid capture job", job, err)
			}
		})
	}
}

func TestDecodeEnergyCaptureMalformedOrMissingInputDoesNotBlockNextValidJob(t *testing.T) {
	for _, test := range []struct {
		name         string
		metadata     []byte
		includeCPU   bool
		wantCategory string
	}{
		{name: "malformed metadata", metadata: []byte("{"), includeCPU: true, wantCategory: "invalid_metadata"},
		{name: "missing raw capture", metadata: []byte(`{"segment":0,"start_ms":1,"end_ms":2,"duration_ms":1}`), wantCategory: "capture_read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			privateDir := filepath.Join(root, "private")
			externalDir := filepath.Join(root, "external")
			if err := os.MkdirAll(privateDir, 0700); err != nil {
				t.Fatal(err)
			}
			writeTestEnergyCapture(t, privateDir, "core-0", test.metadata, test.includeCPU)

			started := time.Now()
			if err := DecodeEnergyCapture(privateDir, externalDir, "core-0.job"); err == nil {
				t.Fatal("malformed or incomplete capture unexpectedly decoded")
			}
			if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
				t.Fatalf("permanent malformed input incurred transient retries: %s", elapsed)
			}
			statusPath := filepath.Join(externalDir, "energy-profiles-v3", "core-status.txt")
			status, err := os.ReadFile(statusPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(status), "outcome=unavailable category="+test.wantCategory) {
				t.Fatalf("failure status = %q, want category %s", status, test.wantCategory)
			}
			if _, err := os.Stat(filepath.Join(privateDir, "core-0.job")); !os.IsNotExist(err) {
				t.Fatalf("failed job marker should be released, stat error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(privateDir, "core-0-cpu.raw")); !os.IsNotExist(err) {
				t.Fatalf("failed raw capture should be removed, stat error = %v", err)
			}

			metadata, err := json.Marshal(energyCaptureMetadata{Segment: 1, StartMS: 2, EndMS: 3, DurationMS: 1})
			if err != nil {
				t.Fatal(err)
			}
			writeTestEnergyCapture(t, privateDir, "core-1", metadata, true)
			if err := DecodeEnergyCapture(privateDir, externalDir, "core-1.job"); err != nil {
				t.Fatalf("next valid capture did not decode: %v", err)
			}
			status, err = os.ReadFile(statusPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(status), "outcome=ready") || !strings.Contains(string(status), "end_ms=3") {
				t.Fatalf("valid follow-up did not restore ready status: %q", status)
			}
			loss, err := os.ReadFile(filepath.Join(externalDir, "energy-profiles-v3", "core-history-status.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(loss), "outcome=loss") || !strings.Contains(string(loss), "lost_segments=1") {
				t.Fatalf("failed capture loss was not retained: %q", loss)
			}
		})
	}
}

func writeTestEnergyCapture(t *testing.T, privateDir, prefix string, metadata []byte, includeCPU bool) {
	t.Helper()
	if includeCPU {
		function := &profile.Function{ID: 1, Name: "pkg.Function", Filename: "capture.go"}
		location := &profile.Location{ID: 1, Line: []profile.Line{{Function: function, Line: 1}}}
		capture := &profile.Profile{
			SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
			Sample:     []*profile.Sample{{Location: []*profile.Location{location}, Value: []int64{1, 1_000_000}}},
			Location:   []*profile.Location{location},
			Function:   []*profile.Function{function},
		}
		var raw bytes.Buffer
		if err := capture.Write(&raw); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(privateDir, prefix+"-cpu.raw"), raw.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(privateDir, prefix+".job"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAppendEnergyHistoryRetainsEverySegmentAsConcatenatedGzipMembers(t *testing.T) {
	dir := t.TempDir()
	appendEnergyHistory(dir, "core", []byte("MARTEN_PROFILE schema=3 process=core segment=1 continuous=true\n"))
	appendEnergyHistory(dir, "core", []byte("MARTEN_PROFILE schema=3 process=core segment=2 continuous=true\n"))

	file, err := os.Open(filepath.Join(dir, "core-history.txt.gz"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		t.Fatalf("read concatenated history stream: %v", err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	file.Close()
	if err != nil {
		t.Fatalf("decompress full history: %v", err)
	}
	for _, segment := range []string{"segment=1", "segment=2"} {
		if count := bytes.Count(data, []byte(segment)); count != 1 {
			t.Errorf("history contains %s %d times, want once: %s", segment, count, data)
		}
	}
}

type faultingEnergyHistoryFile struct {
	file     *os.File
	short    bool
	closeErr error
}

func (f *faultingEnergyHistoryFile) Write(p []byte) (int, error) {
	if f.short && len(p) > 1 {
		return f.file.Write(p[:len(p)/2])
	}
	return f.file.Write(p)
}
func (f *faultingEnergyHistoryFile) Sync() error { return f.file.Sync() }
func (f *faultingEnergyHistoryFile) Close() error {
	err := f.file.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func TestAppendEnergyHistoryFaultsPreserveCommitAndRecoverWithValidGzip(t *testing.T) {
	for _, mode := range []string{"open", "short_write", "close", "commit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "core-history.txt.gz")
			seed := []byte("segment=seed\n")
			if err := appendEnergyHistory(dir, "core", seed); err != nil {
				t.Fatal(err)
			}
			initialInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			initialBytes := initialInfo.Size()
			ops := defaultEnergyHistoryOps()
			switch mode {
			case "open":
				ops.open = func(string) (energyHistoryFile, error) { return nil, errors.New("injected open error") }
			case "short_write":
				open := ops.open
				ops.open = func(path string) (energyHistoryFile, error) {
					file, err := open(path)
					if err != nil {
						return nil, err
					}
					return &faultingEnergyHistoryFile{file: file.(*os.File), short: true}, nil
				}
			case "close":
				open := ops.open
				ops.open = func(path string) (energyHistoryFile, error) {
					file, err := open(path)
					if err != nil {
						return nil, err
					}
					return &faultingEnergyHistoryFile{file: file.(*os.File), closeErr: errors.New("injected close error")}, nil
				}
			case "commit":
				atomic := ops.atomic
				lengthWrites := 0
				ops.atomic = func(dir, name string, data []byte) error {
					if name == "core-history-length.txt" {
						lengthWrites++
						if lengthWrites == 2 {
							return errors.New("injected commit error")
						}
					}
					return atomic(dir, name, data)
				}
			}

			if err := appendEnergyHistoryWithOps(dir, "core", []byte("segment=failed\n"), ops); err == nil {
				t.Fatal("injected history fault unexpectedly succeeded")
			}
			marker, err := os.ReadFile(filepath.Join(dir, "core-history-length.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(marker) != fmt.Sprint(initialBytes) {
				t.Fatalf("commit marker = %q, want previous committed length %d", marker, initialBytes)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != initialBytes {
				t.Fatalf("failed append left %d bytes, want exactly committed %d", info.Size(), initialBytes)
			}
			status, err := os.ReadFile(filepath.Join(dir, "core-history-status.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(status, []byte("outcome=loss")) || !bytes.Contains(status, []byte("lost_segments=1")) {
				t.Fatalf("failed history append did not record one loss: %q", status)
			}

			if err := appendEnergyHistory(dir, "core", []byte("segment=recovered\n")); err != nil {
				t.Fatalf("append after recovery failed: %v", err)
			}
			history := readEnergyHistory(t, path)
			for _, expected := range [][]byte{[]byte("segment=seed"), []byte("segment=recovered")} {
				if !bytes.Contains(history, expected) {
					t.Errorf("recovered history lost %q: %s", expected, history)
				}
			}
			if bytes.Contains(history, []byte("segment=failed")) {
				t.Fatalf("uncommitted failed segment leaked into history: %s", history)
			}
		})
	}
}

func TestAppendEnergyHistoryRotationFailureRecordsLossWithoutReplacingCurrentHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core-history.txt.gz")
	original := bytes.Repeat([]byte("x"), 8<<20)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "core-history-length.txt"), []byte(fmt.Sprint(len(original))), 0600); err != nil {
		t.Fatal(err)
	}
	ops := defaultEnergyHistoryOps()
	ops.remove = func(path string) error {
		if strings.HasSuffix(path, ".old") {
			return errors.New("injected rotation failure")
		}
		return os.Remove(path)
	}
	if err := appendEnergyHistoryWithOps(dir, "core", []byte("segment=next\n"), ops); err == nil || !strings.Contains(err.Error(), "history_rotation") {
		t.Fatalf("rotation failure = %v, want history_rotation", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, original) {
		t.Fatal("rotation fault replaced or truncated the existing committed history")
	}
	status, err := os.ReadFile(filepath.Join(dir, "core-history-status.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(status, []byte("outcome=loss")) || !bytes.Contains(status, []byte("lost_segments=1")) {
		t.Fatalf("rotation failure did not record loss: %q", status)
	}
}

func readEnergyHistory(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		t.Fatalf("open history gzip: %v", err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	fileErr := file.Close()
	if readErr != nil || closeErr != nil || fileErr != nil {
		t.Fatalf("read history gzip: read=%v gzip-close=%v file-close=%v", readErr, closeErr, fileErr)
	}
	return data
}

func TestEnergyCaptureUsesTwoMinuteWindowsAndThirtyMinuteSnapshots(t *testing.T) {
	if energyCaptureWindow != 2*time.Minute {
		t.Fatalf("energy capture window = %s, want 2m", energyCaptureWindow)
	}
	if energyCaptureSnapshotInterval != 30*time.Minute {
		t.Fatalf("snapshot interval = %s, want 30m", energyCaptureSnapshotInterval)
	}
	if energyCaptureSnapshotInterval%energyCaptureWindow != 0 {
		t.Fatalf("snapshot interval %s is not an integer number of capture windows %s", energyCaptureSnapshotInterval, energyCaptureWindow)
	}
	windowsPerSnapshot := int(energyCaptureSnapshotInterval / energyCaptureWindow)
	if windowsPerSnapshot != 15 {
		t.Fatalf("snapshot cadence = every %d windows, want every 15", windowsPerSnapshot)
	}

	source, err := os.ReadFile("energy_capture.go")
	if err != nil {
		t.Fatal(err)
	}
	compact := strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(string(source))
	if !strings.Contains(compact, "Snapshots:segment%int(energyCaptureSnapshotInterval/energyCaptureWindow)==0") {
		t.Fatal("segment snapshot metadata must derive its cadence from the 30-minute interval and 2-minute capture window")
	}
}

func TestEnergyCaptureKeepsContiguousBoundedSamplingAndDefersProfileProcessing(t *testing.T) {
	source, err := os.ReadFile("energy_capture.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "energy_capture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "runEnergyCapture" {
			run = fn
			break
		}
	}
	if run == nil {
		t.Fatal("runEnergyCapture function not found")
	}
	forbidden := map[string]bool{"ParseData": true, "summarizeEnergyProfile": true, "sanitizeEnergyProfile": true, "Write": true}
	ast.Inspect(run.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && forbidden[selector.Sel.Name] {
			t.Errorf("capture path must not call expensive profile parser/summarizer: %s", selector.Sel.Name)
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && forbidden[ident.Name] {
			t.Errorf("capture path must not call expensive profile parser/summarizer: %s", ident.Name)
		}
		return true
	})
	for _, required := range []string{"energyCaptureWindow = 120 * time.Second", "energyCaptureSnapshotInterval = 30 * time.Minute", "time.Sleep(time.Until(start.Add(energyCaptureWindow)))", "segment%int(energyCaptureSnapshotInterval/energyCaptureWindow) == 0", "energyCaptureLimit", "GapUS", "DroppedSegments", "continuous=true", "appendEnergyHistory(dir, \"core\""} {
		if !strings.Contains(string(source), required) {
			t.Errorf("continuous capture/history contract is missing %q", required)
		}
	}
	if strings.Contains(string(source), "budget-paused") || strings.Contains(string(source), "overhead_budget") {
		t.Fatal("continuous sampling must not have a permanent overhead-budget pause")
	}
	stop := bytes.Index(source, []byte("pprof.StopCPUProfile()"))
	if stop < 0 {
		t.Fatal("capture must stop the prior segment during rotation")
	}
	restartOffset := stop + len("pprof.StopCPUProfile()")
	start := bytes.Index(source[restartOffset:], []byte("pprof.StartCPUProfile(cpu)"))
	if start < 0 {
		t.Fatal("each segment must restart CPU sampling after stopping the previous segment")
	}
	start += restartOffset
	between := string(source[stop:start])
	for _, expensive := range []string{"runtime.ReadMemStats", "pprof.Lookup", "profile.ParseData", "summarizeEnergyProfile"} {
		if strings.Contains(between, expensive) {
			t.Errorf("rotation must restart sampling before %s", expensive)
		}
	}
}
