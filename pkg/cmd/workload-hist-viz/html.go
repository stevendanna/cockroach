// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"maps"
	"os"
	"slices"
	"time"
)

//go:embed template.html
var htmlTemplate string

type templateData struct {
	Title             string
	Metrics           []templateMetric
	OriginTime        int64 // Unix timestamp in milliseconds
	VLinesJSON        template.JS
	HLinesOpsJSON     template.JS
	HLinesLatencyJSON template.JS
}

type templateMetric struct {
	Name           string
	ID             string // Safe ID for HTML element
	TimestampsJSON template.JS
	OpsPerSecJSON  template.JS
	P50JSON        template.JS
	P95JSON        template.JS
	P99JSON        template.JS
	PMaxJSON       template.JS
}

type jsVLineMarker struct {
	Offset float64 `json:"offset"` // milliseconds
	Label  string  `json:"label"`
}

type jsHLineMarker struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

func generateHTML(
	timeSeries map[string]*MetricTimeSeries, markers *Markers, outputPath, pageTitle string,
) error {
	names := slices.Sorted(maps.Keys(timeSeries))

	// Convert to template data
	var metrics []templateMetric
	for i, name := range names {
		ts := timeSeries[name]

		timestampsJSON, err := json.Marshal(ts.Timestamps)
		if err != nil {
			return fmt.Errorf("failed to marshal timestamps: %w", err)
		}

		opsPerSecJSON, err := json.Marshal(ts.OpsPerSec)
		if err != nil {
			return fmt.Errorf("failed to marshal ops/sec: %w", err)
		}

		p50JSON, err := json.Marshal(ts.P50)
		if err != nil {
			return fmt.Errorf("failed to marshal p50: %w", err)
		}

		p95JSON, err := json.Marshal(ts.P95)
		if err != nil {
			return fmt.Errorf("failed to marshal p95: %w", err)
		}

		p99JSON, err := json.Marshal(ts.P99)
		if err != nil {
			return fmt.Errorf("failed to marshal p99: %w", err)
		}

		pMaxJSON, err := json.Marshal(ts.PMax)
		if err != nil {
			return fmt.Errorf("failed to marshal pMax: %w", err)
		}

		metrics = append(metrics, templateMetric{
			Name:           name,
			ID:             fmt.Sprintf("metric-%d", i),
			TimestampsJSON: template.JS(timestampsJSON),
			OpsPerSecJSON:  template.JS(opsPerSecJSON),
			P50JSON:        template.JS(p50JSON),
			P95JSON:        template.JS(p95JSON),
			P99JSON:        template.JS(p99JSON),
			PMaxJSON:       template.JS(pMaxJSON),
		})
	}

	// Find origin time (earliest timestamp across all metrics)
	// Since timestamps are sorted, we just need the first one from each metric
	var originTime time.Time
	for _, name := range names {
		ts := timeSeries[name]
		if len(ts.Timestamps) > 0 {
			t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", ts.Timestamps[0])
			if err == nil && (originTime.IsZero() || t.Before(originTime)) {
				originTime = t
			}
		}
	}

	// Convert markers to JavaScript format
	// Initialize as empty slices (not nil) so JSON marshaling produces [] instead of null
	jsVLines := make([]jsVLineMarker, 0)
	for _, vline := range markers.VLines {
		jsVLines = append(jsVLines, jsVLineMarker{
			Offset: float64(vline.Offset.Milliseconds()),
			Label:  vline.Label,
		})
	}

	jsHLinesOps := make([]jsHLineMarker, 0)
	for _, hline := range markers.HLinesOps {
		jsHLinesOps = append(jsHLinesOps, jsHLineMarker{
			Value: hline.Value,
			Label: hline.Label,
		})
	}

	jsHLinesLatency := make([]jsHLineMarker, 0)
	for _, hline := range markers.HLinesLatency {
		jsHLinesLatency = append(jsHLinesLatency, jsHLineMarker{
			Value: hline.Value,
			Label: hline.Label,
		})
	}

	vLinesJSON, err := json.Marshal(jsVLines)
	if err != nil {
		return fmt.Errorf("failed to marshal vlines: %w", err)
	}

	hLinesOpsJSON, err := json.Marshal(jsHLinesOps)
	if err != nil {
		return fmt.Errorf("failed to marshal hlines ops: %w", err)
	}

	hLinesLatencyJSON, err := json.Marshal(jsHLinesLatency)
	if err != nil {
		return fmt.Errorf("failed to marshal hlines latency: %w", err)
	}

	data := templateData{
		Title:             pageTitle,
		Metrics:           metrics,
		OriginTime:        originTime.UnixMilli(),
		VLinesJSON:        template.JS(vLinesJSON),
		HLinesOpsJSON:     template.JS(hLinesOpsJSON),
		HLinesLatencyJSON: template.JS(hLinesLatencyJSON),
	}

	// Parse and execute template
	tmpl, err := template.New("html").Parse(htmlTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	return nil
}
