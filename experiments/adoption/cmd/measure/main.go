// Command measure records a local, in-process comparison. It deliberately does
// not infer human development time or claim representative production latency.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mujhtech/trest/experiments/adoption"
)

type measurement struct {
	Name              string  `json:"name"`
	Samples           int     `json:"samples"`
	P50NS             int64   `json:"p50_ns"`
	P95NS             int64   `json:"p95_ns"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	BenchmarkNS       int64   `json:"benchmark_ns_per_op"`
	Allocations       int64   `json:"allocations_per_op"`
	Bytes             int64   `json:"bytes_per_op"`
	StartupP50NS      int64   `json:"startup_p50_ns"`
	StartupP95NS      int64   `json:"startup_p95_ns"`
	ContractP50NS     int64   `json:"contract_export_p50_ns,omitempty"`
	ContractBytes     int     `json:"contract_bytes,omitempty"`
}

func request(handler http.Handler) {
	r := httptest.NewRequest("POST", "/invoices", strings.NewReader(adoption.Body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer acme-test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 201 {
		panic(fmt.Sprintf("unexpected response: %d", w.Code))
	}
}
func percentile(samples []int64, fraction float64) int64 {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	index := int(float64(len(samples)-1) * fraction)
	return samples[index]
}

func main() {
	samples := flag.Int("samples", 3000, "latency sample count per implementation")
	startup := flag.Int("startup-samples", 30, "construction sample count per implementation")
	flag.Parse()
	if *samples < 100 || *samples > 100000 || *startup < 5 || *startup > 1000 {
		fmt.Fprintln(os.Stderr, "sample counts outside allowed range")
		os.Exit(2)
	}
	type report struct {
		SchemaVersion string        `json:"schema_version"`
		MeasuredAt    string        `json:"measured_at"`
		GoVersion     string        `json:"go_version"`
		OS            string        `json:"os"`
		Architecture  string        `json:"architecture"`
		GOMAXPROCS    int           `json:"gomaxprocs"`
		Workload      string        `json:"workload"`
		Modules       []string      `json:"compiled_modules"`
		Measurements  []measurement `json:"measurements"`
	}
	out := report{SchemaVersion: "trest.adoption.measurement.v1", MeasuredAt: time.Now().UTC().Format(time.RFC3339), GoVersion: runtime.Version(), OS: runtime.GOOS, Architecture: runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0), Workload: "serial httptest POST /invoices, repeated scoped idempotency key; request construction and response recording included; no network, persistence, tracing exporter, or body capture", Modules: []string{}}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			value := dependency.Path + "@" + dependency.Version
			if dependency.Replace != nil {
				value += " => local replacement"
			}
			out.Modules = append(out.Modules, value)
		}
	}
	for _, factory := range adoption.Factories() {
		result := measurement{Name: factory.Name, Samples: *samples}
		construction := make([]int64, *startup)
		for i := range construction {
			started := time.Now()
			_, err := factory.New()
			if err != nil {
				panic(err)
			}
			construction[i] = time.Since(started).Nanoseconds()
		}
		result.StartupP50NS = percentile(construction, .50)
		result.StartupP95NS = percentile(construction, .95)
		fixture, err := factory.New()
		if err != nil {
			panic(err)
		}
		for i := 0; i < 100; i++ {
			request(fixture.Handler)
		}
		latencies := make([]int64, *samples)
		totalStart := time.Now()
		for i := range latencies {
			started := time.Now()
			request(fixture.Handler)
			latencies[i] = time.Since(started).Nanoseconds()
		}
		result.RequestsPerSecond = float64(*samples) / time.Since(totalStart).Seconds()
		result.P50NS = percentile(latencies, .50)
		result.P95NS = percentile(latencies, .95)
		benchmark := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				request(fixture.Handler)
			}
		})
		result.BenchmarkNS = benchmark.NsPerOp()
		result.Allocations = benchmark.AllocsPerOp()
		result.Bytes = benchmark.AllocedBytesPerOp()
		if fixture.Export != nil {
			timings := make([]int64, 30)
			for i := range timings {
				start := time.Now()
				data, err := fixture.Export()
				if err != nil {
					panic(err)
				}
				result.ContractBytes = len(data)
				timings[i] = time.Since(start).Nanoseconds()
			}
			result.ContractP50NS = percentile(timings, .50)
		}
		out.Measurements = append(out.Measurements, result)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
