// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/workload/histogram/exporter"
	"github.com/codahale/hdrhistogram"
)

var (
	inputFile    = flag.String("input", "", "Path to input JSON file (reads from standard in if not provided)")
	outputFile   = flag.String("output", "metrics.html", "Path to output HTML file")
	title        = flag.String("title", "Workload Metrics", "Title for the HTML page")
	metric       = flag.String("metric", "", "Filter to specific metric name (optional)")
	vlineOffsets = flag.String("vline", "", "Vertical line time offsets as 'duration:label,...' (e.g., '5m:Start,10m:Event')")
	hlineOps     = flag.String("hline-ops", "", "Horizontal lines for ops/sec as 'value:label,...' (e.g., '1000:Target,2000:Max')")
	hlineLatency = flag.String("hline-latency", "", "Horizontal lines for latency as 'value:label,...' (e.g., '100:SLA,200:Limit')")
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
	os.Exit(1)
}

func main() {
	flag.Parse()
	markerConfig := MarkerConfig{
		VLineOffsets: *vlineOffsets,
		HLineOps:     *hlineOps,
		HLineLatency: *hlineLatency,
	}

	var input io.Reader
	if *inputFile != "" {
		f, err := os.Open(*inputFile)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		input = f
	} else {
		input = os.Stdin
	}

	data, err := readMetrics(input)
	if err != nil {
		fatal(fmt.Errorf("failed to read metrics: %w", err))
	}

	timeSeries, err := processMetrics(data, *metric)
	if err != nil {
		fatal(fmt.Errorf("failed to process metrics: %w", err))
	}

	markers, err := parseMarkers(markerConfig, timeSeries)
	if err != nil {
		fatal(fmt.Errorf("failed to parse markers: %w", err))
	}

	if err := generateHTML(timeSeries, markers, *outputFile, *title); err != nil {
		fatal(fmt.Errorf("failed to generate HTML: %w", err))
	}

	fmt.Printf("Successfully generated %s\n", *outputFile)
}

// readMetrics reads a newline-delimited JSON containing SnapshotTick records
func readMetrics(r io.Reader) ([]exporter.SnapshotTick, error) {
	var snapshots []exporter.SnapshotTick
	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var snapshot exporter.SnapshotTick
		if err := json.Unmarshal(line, &snapshot); err != nil {
			return nil, fmt.Errorf("failed to parse JSON on line %d: %w", lineNum, err)
		}
		snapshots = append(snapshots, snapshot)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	if len(snapshots) == 0 {
		return nil, fmt.Errorf("no valid snapshots found in file")
	}

	return snapshots, nil
}

// MetricTimeSeries holds time series data for a single metric
type MetricTimeSeries struct {
	Name       string
	Timestamps []string  // ISO 8601 timestamps
	OpsPerSec  []float64 // Operations per second
	P50        []float64 // 50th percentile latency in ms
	P95        []float64 // 95th percentile latency in ms
	P99        []float64 // 99th percentile latency in ms
	PMax       []float64 // Max latency in ms
}

// processMetrics converts raw snapshots into time series data grouped by metric name
func processMetrics(
	snapshots []exporter.SnapshotTick, metricFilter string,
) (map[string]*MetricTimeSeries, error) {
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("no snapshots to process")
	}

	// Group snapshots by metric name
	grouped := make(map[string][]exporter.SnapshotTick)
	for _, snapshot := range snapshots {
		// Apply metric filter if specified
		if metricFilter != "" && snapshot.Name != metricFilter {
			continue
		}
		grouped[snapshot.Name] = append(grouped[snapshot.Name], snapshot)
	}

	if len(grouped) == 0 {
		if metricFilter != "" {
			return nil, fmt.Errorf("no metrics found matching filter: %s", metricFilter)
		}
		return nil, fmt.Errorf("no metrics found in snapshots")
	}

	// Process each metric group
	result := make(map[string]*MetricTimeSeries)
	for name, snaps := range grouped {
		// Sort by timestamp
		sort.Slice(snaps, func(i, j int) bool {
			return snaps[i].Now.Before(snaps[j].Now)
		})

		ts := &MetricTimeSeries{
			Name:       name,
			Timestamps: make([]string, 0, len(snaps)),
			OpsPerSec:  make([]float64, 0, len(snaps)),
			P50:        make([]float64, 0, len(snaps)),
			P95:        make([]float64, 0, len(snaps)),
			P99:        make([]float64, 0, len(snaps)),
			PMax:       make([]float64, 0, len(snaps)),
		}

		for _, snap := range snaps {
			// Import the histogram from snapshot
			hist := hdrhistogram.Import(snap.Hist)

			// Calculate ops per second
			opsPerSec := 0.0
			if snap.Elapsed.Seconds() > 0 {
				opsPerSec = float64(hist.TotalCount()) / snap.Elapsed.Seconds()
			}

			// Convert nanoseconds to milliseconds
			nanoToMs := func(nanos int64) float64 {
				return float64(nanos) / 1_000_000.0
			}

			ts.Timestamps = append(ts.Timestamps, snap.Now.Format("2006-01-02T15:04:05.000Z07:00"))
			ts.OpsPerSec = append(ts.OpsPerSec, opsPerSec)
			ts.P50 = append(ts.P50, nanoToMs(hist.ValueAtQuantile(50)))
			ts.P95 = append(ts.P95, nanoToMs(hist.ValueAtQuantile(95)))
			ts.P99 = append(ts.P99, nanoToMs(hist.ValueAtQuantile(99)))
			ts.PMax = append(ts.PMax, nanoToMs(hist.Max()))
		}

		result[name] = ts
	}

	return result, nil
}

// MarkerConfig holds the raw marker specifications from command line
type MarkerConfig struct {
	VLineOffsets string
	HLineOps     string
	HLineLatency string
}

// VLineMarker represents a vertical line marker at a time offset
type VLineMarker struct {
	Offset time.Duration
	Label  string
}

// HLineMarker represents a horizontal line marker at a Y-axis value
type HLineMarker struct {
	Value float64
	Label string
}

// Markers holds all parsed markers
type Markers struct {
	VLines        []VLineMarker
	HLinesOps     []HLineMarker
	HLinesLatency []HLineMarker
}

// parseMarkers parses marker specifications from command line flags
func parseMarkers(config MarkerConfig, timeSeries map[string]*MetricTimeSeries) (*Markers, error) {
	markers := &Markers{}

	// Parse vertical line offsets
	if config.VLineOffsets != "" {
		vlines, err := parseVLines(config.VLineOffsets)
		if err != nil {
			return nil, fmt.Errorf("failed to parse vline: %w", err)
		}
		markers.VLines = vlines
	}

	// Parse horizontal lines for ops/sec
	if config.HLineOps != "" {
		hlines, err := parseHLines(config.HLineOps)
		if err != nil {
			return nil, fmt.Errorf("failed to parse hline-ops: %w", err)
		}
		markers.HLinesOps = hlines
	}

	// Parse horizontal lines for latency
	if config.HLineLatency != "" {
		hlines, err := parseHLines(config.HLineLatency)
		if err != nil {
			return nil, fmt.Errorf("failed to parse hline-latency: %w", err)
		}
		markers.HLinesLatency = hlines
	}

	return markers, nil
}

// parseVLines parses vertical line specifications like "5m:Start,10m:Event"
func parseVLines(spec string) ([]VLineMarker, error) {
	var markers []VLineMarker
	parts := strings.Split(spec, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		fields := strings.SplitN(part, ":", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid vline format '%s', expected 'duration:label'", part)
		}

		offset, err := time.ParseDuration(fields[0])
		if err != nil {
			return nil, fmt.Errorf("invalid duration '%s': %w", fields[0], err)
		}

		markers = append(markers, VLineMarker{
			Offset: offset,
			Label:  fields[1],
		})
	}

	return markers, nil
}

// parseHLines parses horizontal line specifications like "100:SLA,200:Limit"
func parseHLines(spec string) ([]HLineMarker, error) {
	var markers []HLineMarker
	parts := strings.Split(spec, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		fields := strings.SplitN(part, ":", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid hline format '%s', expected 'value:label'", part)
		}

		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value '%s': %w", fields[0], err)
		}

		markers = append(markers, HLineMarker{
			Value: value,
			Label: fields[1],
		})
	}

	return markers, nil
}
