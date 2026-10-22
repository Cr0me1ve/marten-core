package mobile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/pprof/profile"
)

var energyProfilingStarted atomic.Bool

// StartEnergyProfiling is diagnostic-only, local and independent of VPN locks.
// Android calls this only in :core. It never opens a profiling HTTP listener.
func StartEnergyProfiling(directory string) error {
	if !filepath.IsAbs(directory) {
		return errors.New("invalid diagnostics directory")
	}
	if !energyProfilingStarted.CompareAndSwap(false, true) {
		return nil
	}
	dir := filepath.Join(directory, "energy-profiles")
	if err := os.MkdirAll(dir, 0700); err != nil {
		energyProfilingStarted.Store(false)
		return errors.New("diagnostics storage unavailable")
	}
	go runEnergyProfiling(dir)
	return nil
}

type energyBoundedWriter struct {
	bytes.Buffer
	overflow bool
	limit    int
}

func (w *energyBoundedWriter) Write(p []byte) (int, error) {
	if w.Len()+len(p) > w.limit {
		w.overflow = true
		return len(p), nil
	}
	if w.overflow {
		return len(p), nil
	}
	return w.Buffer.Write(p)
}

// The compatibility entry point also uses the bounded capture path. Android
// supplies an internal private spool through StartEnergyCapture instead.
func runEnergyProfiling(dir string) {
	runEnergyCapture(dir, filepath.Join(dir, "private-spool"))
}

//go:noinline
func energyProfileCalibration() {
	deadline := time.Now().Add(200 * time.Millisecond)
	var x uint64 = 1
	for time.Now().Before(deadline) {
		for i := 0; i < 4096; i++ {
			x = x*6364136223846793005 + 1
		}
	}
	energyCalibrationSink.Store(x)
}

var energyCalibrationSink atomic.Uint64

var energySymbolChars = regexp.MustCompile(`[^a-zA-Z0-9_./*()<>$:\[\]-]`)

func safeEnergySymbol(s string) string {
	for _, prefix := range []string{"github.com/", "golang.org/", "google.golang.org/"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			if i := strings.IndexByte(s, '/'); i >= 0 {
				s = s[i+1:]
			}
			break
		}
	}
	s = energySymbolChars.ReplaceAllString(s, "_")
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}

// CPU/heap/goroutine profiles contain code symbols only after this transform.
// Drop arbitrary pprof labels, paths, build identifiers and raw PC addresses.
func sanitizeEnergyProfile(p *profile.Profile) {
	p.Comments = nil
	p.DropFrames = ""
	p.KeepFrames = ""
	for _, s := range p.Sample {
		s.Label = nil
		s.NumLabel = nil
		s.NumUnit = nil
	}
	for _, m := range p.Mapping {
		m.File = "libmarten-core.so"
		m.BuildID = ""
		m.Start = 0
		m.Limit = 0
		m.Offset = 0
	}
	for _, l := range p.Location {
		l.Address = 0
	}
	for _, f := range p.Function {
		f.Name = safeEnergySymbol(f.Name)
		if f.Name == "" {
			f.Name = "unresolved_native_frame"
		}
		f.SystemName = f.Name
		f.Filename = safeEnergySymbol(filepath.Base(f.Filename))
		if f.Filename == "." || f.Filename == "" {
			f.Filename = "unknown_source"
		}
	}
}

type energyFunctionRow struct {
	name, file            string
	line, flat, inclusive int64
}

func summarizeEnergyProfile(p *profile.Profile, sampleIndex, maxRows int) string {
	if sampleIndex < 0 || sampleIndex >= len(p.SampleType) {
		return "PROFILE outcome=no_sample_type\n"
	}
	rows := map[string]*energyFunctionRow{}
	stacks := map[string]int64{}
	var total int64
	for _, sample := range p.Sample {
		if sampleIndex >= len(sample.Value) {
			continue
		}
		value := sample.Value[sampleIndex]
		total += value
		seen := map[string]bool{}
		var stack []string
		for _, loc := range sample.Location {
			for _, line := range loc.Line {
				f := line.Function
				if f == nil {
					continue
				}
				name := strings.ReplaceAll(safeEnergySymbol(f.Name), ".", "::")
				key := name + ":" + fmt.Sprint(line.Line)
				r := rows[key]
				if r == nil {
					r = &energyFunctionRow{name: name, file: safeEnergySymbol(filepath.Base(f.Filename)), line: line.Line}
					rows[key] = r
				}
				if len(stack) == 0 {
					r.flat += value
				}
				if !seen[key] {
					r.inclusive += value
					seen[key] = true
				}
				if len(stack) < 24 {
					stack = append(stack, fmt.Sprintf("%s:%d", name, line.Line))
				}
			}
		}
		if len(stack) > 0 {
			stacks[strings.Join(stack, " <- ")] += value
		}
	}
	var list []*energyFunctionRow
	for _, r := range rows {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].inclusive == list[j].inclusive {
			return list[i].name < list[j].name
		}
		return list[i].inclusive > list[j].inclusive
	})
	var out strings.Builder
	fmt.Fprintf(&out, "PROFILE unit=%s total=%d\n", safeEnergySymbol(p.SampleType[sampleIndex].Unit), total)
	for i, r := range list {
		if i >= maxRows {
			break
		}
		flatPct, inclusivePct := float64(0), float64(0)
		if total > 0 {
			flatPct = 100 * float64(r.flat) / float64(total)
			inclusivePct = 100 * float64(r.inclusive) / float64(total)
		}
		fmt.Fprintf(&out, "FUNCTION flat=%d inclusive=%d symbol=%s source=%s:%d flat_pct=%.2f inclusive_pct=%.2f\n", r.flat, r.inclusive, r.name, r.file, r.line, flatPct, inclusivePct)
	}
	type chain struct {
		name  string
		value int64
	}
	var chains []chain
	for name, value := range stacks {
		chains = append(chains, chain{name, value})
	}
	sort.Slice(chains, func(i, j int) bool {
		if chains[i].value == chains[j].value {
			return chains[i].name < chains[j].name
		}
		return chains[i].value > chains[j].value
	})
	for i, c := range chains {
		if i >= maxRows {
			break
		}
		fmt.Fprintf(&out, "STACK value=%d frames=%s\n", c.value, c.name)
	}
	return out.String()
}

func writeEnergyAtomic(dir, name string, data []byte) error {
	if len(data) > 8<<20 {
		return errors.New("diagnostic output size limit")
	}
	file := filepath.Join(dir, name)
	tmp := file + ".pending"
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, file)
}

var _ io.Writer = (*energyBoundedWriter)(nil)
