package mobile

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
)

// Sampling stays enabled during each window and decoder work. Longer segments
// amortize pprof's repeated process-mapping reads, allocations and compression.
const energyCaptureWindow = 120 * time.Second
const energyCaptureInterval = energyCaptureWindow
const energyCaptureSnapshotInterval = 30 * time.Minute
const energyCaptureLimit = 2 << 20

// StartEnergyCapture records only. Raw code metadata stays in the application's
// internal spool; decoding, sanitizing and report construction run in :diagnostics.
func StartEnergyCapture(directory, privateDirectory string) error {
	if !filepath.IsAbs(directory) || !filepath.IsAbs(privateDirectory) {
		return errors.New("invalid diagnostics directory")
	}
	if !energyProfilingStarted.CompareAndSwap(false, true) {
		return nil
	}
	dir := filepath.Join(directory, "energy-profiles-v3")
	for _, path := range []string{dir, privateDirectory} {
		if os.MkdirAll(path, 0700) != nil {
			energyProfilingStarted.Store(false)
			return errors.New("diagnostics storage unavailable")
		}
	}
	go runEnergyCapture(dir, privateDirectory)
	return nil
}

type energyCaptureMetadata struct {
	Segment         int    `json:"segment"`
	StartMS         int64  `json:"start_ms"`
	EndMS           int64  `json:"end_ms"`
	DurationMS      int64  `json:"duration_ms"`
	HeapBytes       uint64 `json:"heap_bytes"`
	HeapObjects     uint64 `json:"heap_objects"`
	AllocBytes      uint64 `json:"alloc_bytes"`
	GC              uint32 `json:"gc"`
	GCPauseNS       uint64 `json:"gc_pause_ns"`
	Goroutines      int    `json:"goroutines"`
	Snapshots       bool   `json:"snapshots"`
	GapUS           int64  `json:"gap_us"`
	DroppedSegments int    `json:"dropped_segments"`
}

func runEnergyCapture(dir, spool string) {
	active := false
	oldMutex := runtime.SetMutexProfileFraction(100)
	runtime.SetBlockProfileRate(10_000_000)
	defer func() {
		if active {
			pprof.StopCPUProfile()
		}
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(oldMutex)
		writeEnergyAtomic(spool, "core-phase.txt", []byte("phase=idle sequence=0-0\n"))
		if recover() != nil {
			writeEnergyAtomic(dir, "core-status.txt", []byte("MARTEN_PROFILE schema=3 process=core outcome=failed category=panic\n"))
		}
	}()
	stopSignals := prepareEnergyProfileSignals()
	defer stopSignals()
	generation := time.Now().UnixMilli()
	writeEnergyAtomic(dir, "core-status.txt", []byte(fmt.Sprintf("MARTEN_PROFILE schema=3 process=core outcome=recording continuous=true rotation_seconds=%d\n", int(energyCaptureWindow/time.Second))))
	writeEnergyAtomic(spool, "core-phase.txt", []byte(fmt.Sprintf("phase=capture sequence=%d-1 pid=%d\n", generation, os.Getpid())))
	cpu := &energyBoundedWriter{limit: energyCaptureLimit}
	if pprof.StartCPUProfile(cpu) != nil {
		writeEnergyAtomic(dir, "core-status.txt", []byte("MARTEN_PROFILE schema=3 process=core outcome=failed category=cpu_start\n"))
		return
	}
	active = true
	start := time.Now()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	energyProfileCalibration()
	gapUS := int64(0)
	dropped := 0
	for segment := 0; ; segment++ {
		time.Sleep(time.Until(start.Add(energyCaptureWindow)))
		stop := time.Now()
		pprof.StopCPUProfile()
		active = false
		completed := cpu
		cpu = &energyBoundedWriter{limit: energyCaptureLimit}
		// Restart before snapshots, disk writes or decoder work. There is no
		// deliberate off interval; the runtime's unavoidable rotation gap is measured.
		if pprof.StartCPUProfile(cpu) != nil {
			writeEnergyAtomic(dir, "core-status.txt", []byte("MARTEN_PROFILE schema=3 process=core outcome=failed category=cpu_restart\n"))
			return
		}
		active = true
		nextStart := time.Now()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		m := energyCaptureMetadata{Segment: segment, StartMS: start.UnixMilli(), EndMS: stop.UnixMilli(), DurationMS: stop.Sub(start).Milliseconds(), HeapBytes: after.HeapAlloc, HeapObjects: after.HeapObjects, AllocBytes: after.TotalAlloc - before.TotalAlloc, GC: after.NumGC - before.NumGC, GCPauseNS: after.PauseTotalNs - before.PauseTotalNs, Goroutines: runtime.NumGoroutine(), Snapshots: segment%int(energyCaptureSnapshotInterval/energyCaptureWindow) == 0, GapUS: gapUS, DroppedSegments: dropped}
		prefix := fmt.Sprintf("core-%d", segment%6)
		_, pending := os.Stat(filepath.Join(spool, prefix+".job"))
		if !completed.overflow && os.IsNotExist(pending) {
			writeEnergyAtomic(spool, prefix+"-cpu.raw", completed.Bytes())
			if m.Snapshots {
				for _, kind := range []string{"heap", "allocs", "goroutine", "block", "mutex"} {
					buf := &energyBoundedWriter{limit: energyCaptureLimit}
					if p := pprof.Lookup(kind); p != nil && p.WriteTo(buf, 0) == nil && !buf.overflow {
						writeEnergyAtomic(spool, prefix+"-"+kind+".raw", buf.Bytes())
					}
				}
			}
			data, _ := json.Marshal(m)
			writeEnergyAtomic(spool, prefix+".job", data)
		} else {
			dropped++
			writeEnergyAtomic(dir, "core-status.txt", []byte(fmt.Sprintf("MARTEN_PROFILE schema=3 process=core outcome=recording continuous=true dropped_segments=%d reason=spool_or_size_limit\n", dropped)))
		}
		gapUS = nextStart.Sub(stop).Microseconds()
		start = nextStart
		before = after
	}
}

// DecodeEnergyCapture must be called only by the diagnostic decoder process.
// Only producer-owned filenames are accepted; raw inputs never leave the spool.
var energyDecodeMu sync.Mutex

type energyCaptureFailure struct {
	category  string
	permanent bool
}

func (f *energyCaptureFailure) Error() string { return f.category }

func captureReadFailure(category string, err error) error {
	return &energyCaptureFailure{category, os.IsNotExist(err) || errors.Is(err, errInvalidEnergyCapture)}
}

func DecodeEnergyCapture(privateDirectory, directory, job string) error {
	if !validEnergyJob(privateDirectory, directory, job) {
		return errors.New("invalid capture job")
	}
	energyDecodeMu.Lock()
	defer energyDecodeMu.Unlock()
	snapshot, err := os.Lstat(filepath.Join(privateDirectory, job))
	if err != nil {
		return errors.New("capture metadata unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		err = decodeEnergyCapture(privateDirectory, directory, job)
		if err == nil {
			break
		}
		var failure *energyCaptureFailure
		if errors.As(err, &failure) && failure.permanent {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
		}
	}
	if err != nil {
		category := "decode_io"
		var failure *energyCaptureFailure
		if errors.As(err, &failure) {
			category = failure.category
		}
		dir := filepath.Join(directory, "energy-profiles-v3")
		_ = os.MkdirAll(dir, 0700)
		_ = recordEnergyHistoryLoss(dir, "core", category)
		_ = writeEnergyAtomic(dir, "core-status.txt", []byte("MARTEN_PROFILE schema=3 process=core outcome=unavailable category="+category+" dropped_segments=1\n"))
	}
	// The producer keeps this marker until we finish; remove raw inputs first,
	// then release the exact slot. Never clean a replaced or live generation.
	cleanupEnergyCapture(privateDirectory, job, snapshot)
	return err
}

func validEnergyJob(privateDirectory, directory, job string) bool {
	return filepath.IsAbs(privateDirectory) && filepath.IsAbs(directory) && len(job) == len("core-0.job") && strings.HasPrefix(job, "core-") && strings.HasSuffix(job, ".job") && job[5] >= '0' && job[5] <= '5'
}

func cleanupEnergyCapture(spool, job string, expected os.FileInfo) {
	current, err := os.Lstat(filepath.Join(spool, job))
	if err != nil || !os.SameFile(current, expected) || current.Size() != expected.Size() || !current.ModTime().Equal(expected.ModTime()) {
		return
	}
	prefix := strings.TrimSuffix(job, ".job")
	for _, kind := range []string{"cpu", "heap", "allocs", "goroutine", "block", "mutex"} {
		_ = os.Remove(filepath.Join(spool, prefix+"-"+kind+".raw"))
	}
	_ = os.Remove(filepath.Join(spool, job))
}

func decodeEnergyCapture(privateDirectory, directory, job string) error {
	if !filepath.IsAbs(privateDirectory) || !filepath.IsAbs(directory) || len(job) != len("core-0.job") || !strings.HasPrefix(job, "core-") || !strings.HasSuffix(job, ".job") || job[5] < '0' || job[5] > '5' {
		return errors.New("invalid capture job")
	}
	prefix := strings.TrimSuffix(job, ".job")
	data, err := readEnergyBounded(filepath.Join(privateDirectory, job), 2048)
	if err != nil {
		return captureReadFailure("metadata_read", err)
	}
	var m energyCaptureMetadata
	if json.Unmarshal(data, &m) != nil || m.DurationMS <= 0 || m.DurationMS > 86_400_000 || m.Segment < 0 {
		return &energyCaptureFailure{"invalid_metadata", true}
	}
	dir := filepath.Join(directory, "energy-profiles-v3")
	if os.MkdirAll(dir, 0700) != nil {
		return errors.New("diagnostics storage unavailable")
	}
	raw, err := readEnergyBounded(filepath.Join(privateDirectory, prefix+"-cpu.raw"), energyCaptureLimit)
	if err != nil {
		return captureReadFailure("capture_read", err)
	}
	p, err := profile.ParseData(raw)
	if err != nil {
		return &energyCaptureFailure{"invalid_profile", true}
	}
	sanitizeEnergyProfile(p)
	var cpuNS, samples, unknownNS, profilerNS int64
	applicationProfile := *p
	applicationProfile.Sample = nil
	profilerProfile := *p
	profilerProfile.Sample = nil
	calibrated := false
	for _, s := range p.Sample {
		if len(s.Value) < 2 {
			continue
		}
		cpuNS += s.Value[1]
		samples += s.Value[0]
		if len(s.Location) == 0 || len(s.Location[0].Line) == 0 || s.Location[0].Line[0].Function == nil || s.Location[0].Line[0].Function.Name == "unresolved_native_frame" {
			unknownNS += s.Value[1]
		}
		isProfiler := false
		for _, l := range s.Location {
			for _, line := range l.Line {
				if line.Function == nil {
					continue
				}
				name := line.Function.Name
				if strings.Contains(name, "energyProfileCalibration") {
					calibrated = true
					isProfiler = true
				}
				if strings.Contains(name, "runEnergyCapture") || strings.HasPrefix(name, "runtime/pprof.") {
					isProfiler = true
				}
			}
		}
		if isProfiler {
			profilerNS += s.Value[1]
			profilerProfile.Sample = append(profilerProfile.Sample, s)
		} else {
			applicationProfile.Sample = append(applicationProfile.Sample, s)
		}
	}
	rate := float64(cpuNS) / float64(m.DurationMS*1e6)
	var encoded bytes.Buffer
	if p.Write(&encoded) != nil {
		return errors.New("capture encoding failed")
	}
	if err := writeEnergyAtomic(dir, fmt.Sprintf("core-cpu-%d.pprof", m.Segment%6), encoded.Bytes()); err != nil {
		return errors.New("capture output unavailable")
	}
	// Peak state belongs to the serialized decoder, not the core process. Persist
	// the numeric score so restarting the decoder does not discard the peak.
	peak := readEnergyScore(dir, "core-peak-score.txt", -1)
	idle := readEnergyScore(dir, "core-idle-score.txt", 1e12)
	isPeak := samples > 0 && rate > peak && m.Segment > 0
	isIdle := samples > 0 && rate < idle && m.Segment > 0
	for _, save := range []struct {
		yes  bool
		name string
	}{{isPeak, "peak"}, {isIdle, "idle"}} {
		if save.yes {
			writeEnergyAtomic(dir, "core-cpu-"+save.name+".pprof", encoded.Bytes())
			writeEnergyAtomic(dir, "core-"+save.name+"-score.txt", []byte(fmt.Sprintf("%g", rate)))
		}
	}
	var report strings.Builder
	fmt.Fprintf(&report, "MARTEN_PROFILE schema=3 process=core outcome=complete start_ms=%d end_ms=%d segment=%d duration_ms=%d samples=%d sampled_cpu_ms=%d calibration_window=%t calibration_seen=%t continuous=true rotation_seconds=%d decoder_process=diagnostics\n", m.StartMS, m.EndMS, m.Segment, m.DurationMS, samples, cpuNS/1e6, m.Segment == 0, calibrated, int(energyCaptureWindow/time.Second))
	fmt.Fprintf(&report, "CORE_RECORDING rotation_gap_us=%d dropped_segments=%d continuous=true\n", m.GapUS, m.DroppedSegments)
	fmt.Fprintf(&report, "MARTEN_GO_MEMORY heap_bytes=%d heap_objects=%d total_alloc_delta_bytes=%d gc_delta=%d gc_pause_delta_ns=%d goroutines=%d\n", m.HeapBytes, m.HeapObjects, m.AllocBytes, m.GC, m.GCPauseNS, m.Goroutines)
	fmt.Fprintf(&report, "CORE_CPU sampled_one_cpu_pct=%.2f peak_window=%t lowest_sampled_window=%t\n", rate*100, isPeak, isIdle)
	fmt.Fprintf(&report, "CORE_CPU_COVERAGE total_sampled_cpu_ms=%d unresolved_leaf_cpu_ms=%d explicit_profiler_cpu_ms=%d indirect_overhead_not_subtracted=true scope=sampled_go_and_recognized_native_frames\n", cpuNS/1e6, unknownNS/1e6, profilerNS/1e6)
	report.WriteString("CPU APPLICATION FUNCTION PROFILE (flat and inclusive nanoseconds; explicit profiler stacks excluded; indirect overhead not subtracted)\n")
	report.WriteString(summarizeEnergyProfile(&applicationProfile, 1, 40))
	report.WriteString("CPU PROFILER FUNCTION PROFILE — CPU FUNCTION PROFILE of explicit profiler stacks including self-check; not an application cause\n")
	report.WriteString(summarizeEnergyProfile(&profilerProfile, 1, 40))
	if m.Snapshots {
		for _, kind := range []string{"heap", "allocs", "goroutine", "block", "mutex"} {
			raw, err := readEnergyBounded(filepath.Join(privateDirectory, prefix+"-"+kind+".raw"), energyCaptureLimit)
			if err != nil {
				fmt.Fprintf(&report, "PROFILE kind=%s outcome=unavailable\n", kind)
				continue
			}
			q, err := profile.ParseData(raw)
			if err != nil {
				fmt.Fprintf(&report, "PROFILE kind=%s outcome=unavailable\n", kind)
				continue
			}
			sanitizeEnergyProfile(q)
			var out bytes.Buffer
			if q.Write(&out) == nil {
				writeEnergyAtomic(dir, "core-"+kind+".pprof", out.Bytes())
			}
			fmt.Fprintf(&report, "PROFILE kind=%s scope=current_heap_or_cumulative_recorded_windows snapshots_interval_seconds=1800 forced_gc=false\n", kind)
			report.WriteString(summarizeEnergyProfile(q, len(q.SampleType)-1, 15))
		}
	}
	if err := writeEnergyAtomic(dir, fmt.Sprintf("core-report-%d.txt", m.Segment%6), []byte(report.String())); err != nil {
		return errors.New("capture report unavailable")
	}
	history := report.String()[:strings.Index(report.String(), "CPU APPLICATION FUNCTION PROFILE")]
	history += "CPU APPLICATION FUNCTION PROFILE\n" + summarizeEnergyProfile(&applicationProfile, 1, 100_000)
	history += "CPU PROFILER FUNCTION PROFILE including all observed stacks\n" + summarizeEnergyProfile(&profilerProfile, 1, 100_000)
	historyErr := appendEnergyHistory(dir, "core", []byte(history))
	historyOutcome := "ready"
	if historyErr != nil {
		historyOutcome = "unavailable"
	}
	fmt.Fprintf(&report, "HISTORY source=core outcome=%s\n", historyOutcome)
	writeEnergyAtomic(dir, fmt.Sprintf("core-report-%d.txt", m.Segment%6), []byte(report.String()))
	if isPeak {
		writeEnergyAtomic(dir, "core-report-peak.txt", []byte(report.String()))
	}
	if isIdle {
		writeEnergyAtomic(dir, "core-report-idle.txt", []byte(report.String()))
	}
	outcome := "ready"
	if samples == 0 {
		outcome = "no_samples"
	}
	writeEnergyAtomic(dir, "core-status.txt", []byte(fmt.Sprintf("MARTEN_PROFILE schema=3 process=core outcome=%s end_ms=%d samples=%d rotation_gap_us=%d dropped_segments=%d continuous=true decoder_process=diagnostics history_outcome=%s\n", outcome, m.EndMS, samples, m.GapUS, m.DroppedSegments, historyOutcome)))
	if m.Segment == 0 {
		check := "no_calibration_samples"
		if calibrated {
			check = "passed"
		}
		writeEnergyAtomic(dir, "core-self-check.txt", []byte(fmt.Sprintf("MARTEN_PROFILE_SELF_CHECK outcome=%s samples=%d calibration_cpu_ms=200\n", check, samples)))
	}
	return nil
}

var errInvalidEnergyCapture = errors.New("invalid capture input")

func readEnergyBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errInvalidEnergyCapture
	}
	return os.ReadFile(path)
}

func readEnergyScore(dir, name string, fallback float64) float64 {
	data, err := readEnergyBounded(filepath.Join(dir, name), 128)
	var value float64
	if err != nil {
		return fallback
	}
	if _, err := fmt.Sscan(string(data), &value); err != nil {
		return fallback
	}
	return value
}

// Append compressed, sanitized history in the decoder only. Rotation is bounded;
// retained generations and any truncation remain explicit in the exported status.
type energyHistoryFile interface {
	io.Writer
	Sync() error
	Close() error
}
type energyHistoryOps struct {
	open     func(string) (energyHistoryFile, error)
	stat     func(string) (os.FileInfo, error)
	rename   func(string, string) error
	remove   func(string) error
	truncate func(string, int64) error
	atomic   func(string, string, []byte) error
}

func defaultEnergyHistoryOps() energyHistoryOps {
	return energyHistoryOps{
		open: func(path string) (energyHistoryFile, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		},
		stat: os.Stat, rename: os.Rename, remove: os.Remove, truncate: os.Truncate, atomic: writeEnergyAtomic,
	}
}

func appendEnergyHistory(dir, source string, data []byte) error {
	return appendEnergyHistoryWithOps(dir, source, data, defaultEnergyHistoryOps())
}

func appendEnergyHistoryWithOps(dir, source string, data []byte, ops energyHistoryOps) (failure error) {
	defer func() {
		if failure != nil {
			_ = recordEnergyHistoryLoss(dir, source, failure.Error())
		}
	}()
	var encoded bytes.Buffer
	z := gzip.NewWriter(&encoded)
	if _, err := z.Write(data); err != nil {
		return errors.New("history_encode")
	}
	if z.Close() != nil {
		return errors.New("history_encode")
	}
	if encoded.Len() > 8<<20 {
		return errors.New("history_segment_size")
	}
	path := filepath.Join(dir, source+"-history.txt.gz")
	size := int64(0)
	if st, err := ops.stat(path); err == nil {
		size = st.Size()
	} else if !os.IsNotExist(err) {
		return errors.New("history_stat")
	}
	committed := int64(0)
	marker, markerErr := os.ReadFile(filepath.Join(dir, source+"-history-length.txt"))
	if markerErr == nil {
		var err error
		committed, err = strconv.ParseInt(string(marker), 10, 64)
		if err != nil || committed < 0 || committed > size {
			_ = recordEnergyHistoryLoss(dir, source, "history_uncommitted")
			committed = 0
		}
	} else if !os.IsNotExist(markerErr) {
		return errors.New("history_length")
	} else if size > 0 {
		// Older/incomplete stores without a commit marker are verified once, never
		// blindly appended. Streaming validation bounds memory and decompression.
		f, err := os.Open(path)
		if err != nil {
			return errors.New("history_open")
		}
		reader, err := gzip.NewReader(f)
		if err == nil {
			var n int64
			n, err = io.Copy(io.Discard, io.LimitReader(reader, 128<<20+1))
			if n > 128<<20 {
				err = errors.New("history_limit")
			}
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
		}
		_ = f.Close()
		if err == nil {
			committed = size
		} else {
			_ = recordEnergyHistoryLoss(dir, source, "history_uncommitted")
		}
	}
	// A short write or interrupted append leaves bytes beyond this marker. Drop
	// only that uncommitted tail before starting the next complete gzip member.
	if size != committed {
		if err := ops.truncate(path, committed); err != nil {
			return errors.New("history_truncate")
		}
		size = committed
	}
	if err := ops.atomic(dir, source+"-history-length.txt", []byte(strconv.FormatInt(committed, 10))); err != nil {
		return errors.New("history_commit")
	}
	if size+int64(encoded.Len()) > 8<<20 {
		if err := ops.remove(path + ".old"); err != nil && !os.IsNotExist(err) {
			return errors.New("history_rotation")
		}
		if err := ops.rename(path, path+".old"); err != nil {
			return errors.New("history_rotation")
		}
		size, committed = 0, 0
		if err := ops.atomic(dir, source+"-history-length.txt", []byte("0")); err != nil {
			return errors.New("history_commit")
		}
		previous, _ := os.ReadFile(filepath.Join(dir, source+"-history-status.txt"))
		_ = ops.atomic(dir, source+"-history-status.txt", []byte(fmt.Sprintf("MARTEN_PROFILE schema=3 outcome=retention_rotated history_limit_bytes=16777216 lost_segments=%d\n", historyLostCount(previous))))
	}
	f, err := ops.open(path)
	if err != nil {
		return errors.New("history_open")
	}
	n, writeErr := f.Write(encoded.Bytes())
	if writeErr == nil && n != encoded.Len() {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = ops.truncate(path, committed)
		if writeErr != nil {
			return errors.New("history_write")
		}
		return errors.New("history_close")
	}
	if err := ops.atomic(dir, source+"-history-length.txt", []byte(strconv.FormatInt(size+int64(n), 10))); err != nil {
		_ = ops.truncate(path, committed)
		return errors.New("history_commit")
	}
	return nil
}

func historyLostCount(data []byte) int {
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, "lost_segments=") {
			n, _ := strconv.Atoi(strings.TrimPrefix(field, "lost_segments="))
			return n
		}
	}
	return 0
}

func recordEnergyHistoryLoss(dir, source, category string) error {
	previous, _ := os.ReadFile(filepath.Join(dir, source+"-history-status.txt"))
	return writeEnergyAtomic(dir, source+"-history-status.txt", []byte(fmt.Sprintf("MARTEN_PROFILE schema=3 outcome=loss category=%s lost_segments=%d\n", category, historyLostCount(previous)+1)))
}
