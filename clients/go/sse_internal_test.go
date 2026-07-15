package aichat

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadSSEEventJoinsMultiLineDataWithNewline(t *testing.T) {
	// Per the SSE spec, multiple data lines are concatenated with "\n".
	input := "event: delta\n" +
		"data: line1\n" +
		"data:line2\n" + // no space after the colon
		"data:  padded\n" + // exactly one leading space is stripped
		"data: \n" + // empty data line still contributes a "\n"
		"\n"
	ev, err := readSSEEvent(bufio.NewReader(strings.NewReader(input)))
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if ev.name != "delta" {
		t.Errorf("name = %q, want delta", ev.name)
	}
	if want := "line1\nline2\n padded\n"; ev.data != want {
		t.Errorf("data = %q, want %q", ev.data, want)
	}
}

func TestReadSSEEventSplitAcrossReads(t *testing.T) {
	// OneByteReader forces every frame to arrive one byte per read.
	input := "event: sql\r\ndata: {\"sql\":\"SELECT 1\"}\r\n\r\n"
	r := bufio.NewReader(iotest.OneByteReader(strings.NewReader(input)))
	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if ev.name != "sql" || ev.data != `{"sql":"SELECT 1"}` {
		t.Errorf("event = %+v", ev)
	}
}

func TestReadSSEEventSkipsCommentsAndEmptyFrames(t *testing.T) {
	input := ": keep-alive\n" +
		"\n" +
		"event: ignored-no-data\n" +
		"\n" +
		"id: 7\n" +
		"retry: 1000\n" +
		"data: hello\n" +
		"\n"
	ev, err := readSSEEvent(bufio.NewReader(strings.NewReader(input)))
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	// The comment-only and data-less frames are not dispatched, and the
	// stale event name from the data-less frame does not leak forward.
	if ev.name != "" || ev.data != "hello" {
		t.Errorf("event = %+v, want name %q data %q", ev, "", "hello")
	}
}

func TestReadSSEEventDiscardsIncompleteTrailingFrame(t *testing.T) {
	input := "event: delta\ndata: {\"text\":\"cut off"
	_, err := readSSEEvent(bufio.NewReader(strings.NewReader(input)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("readSSEEvent err = %v, want io.EOF", err)
	}
}
