// Copyright 2021 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package cdctest

import (
	"fmt"
	"os"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/util/fsm"
)

type NemesesEvent struct {
	Event   fsm.Event
	Payload fsm.EventPayload
}

func (e NemesesEvent) ShortString() string {
	buf := &strings.Builder{}
	if s, ok := e.Event.(fmt.Stringer); ok {
		fmt.Fprintf(buf, "{Event: %s", s.String())
	} else {
		fmt.Fprintf(buf, "{Event: %#v", e.Event)
	}
	if e.Payload == nil {
		fmt.Fprintf(buf, "}")
	} else {
		fmt.Fprintf(buf, ", Payload: %#v}", e.Payload)
	}
	return buf.String()
}

type NemesesRecording struct {
	RowCount           int
	MaxTestColumnCount int
	IsSinkless         bool

	PreStartEvents []NemesesEvent
	Events         []NemesesEvent

	TxnOpenBeforeInitialScan        bool
	TxnOpenBeforeInitialScanPayload openTxnPayload

	nextEventIdx int
}

func (r *NemesesRecording) String() string {
	buf := &strings.Builder{}
	fmt.Fprintf(buf,
		"NemesesRecording{RowCount: %d, MaxTestColumnCount: %d, IsSinkless: %t, TxnOpenBeforeInitialScan: %t, ",
		r.RowCount, r.MaxTestColumnCount, r.IsSinkless, r.TxnOpenBeforeInitialScan)
	if r.TxnOpenBeforeInitialScan {
		fmt.Fprintf(buf, "TxnOpenBeforeInitialScanPayload: %#v, ", r.TxnOpenBeforeInitialScanPayload)
	}
	fmt.Fprint(buf, "PreStartEvents: []NemesesEvent{")
	for i, e := range r.PreStartEvents {
		trailer := ","
		if i == len(r.Events)-1 {
			trailer = ""
		}
		fmt.Fprintf(buf, " %s%s", e.ShortString(), trailer)
	}
	fmt.Fprintf(buf, "}, Events: []NemesesEvent{")
	for i, e := range r.Events {
		trailer := ","
		if i == len(r.Events)-1 {
			trailer = ""
		}
		fmt.Fprintf(buf, " %s%s", e.ShortString(), trailer)
	}
	fmt.Fprintf(buf, "}}")
	s := buf.String()
	return strings.Replace(s, "cdctest.", "", -1)
}

func (r *NemesesRecording) writeToDisk(path string) error {
	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()
	_, err = fmt.Fprintln(outFile, r.String())
	return err
}

func (r *NemesesRecording) nextEvent() (fsm.Event, fsm.EventPayload, error) {
	if r.nextEventIdx >= len(r.Events) {
		return eventFinished{}, nil, nil
	}
	thisEvent := r.Events[r.nextEventIdx]
	r.nextEventIdx++
	return thisEvent.Event, thisEvent.Payload, nil
}
