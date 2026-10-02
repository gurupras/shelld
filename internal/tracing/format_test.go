package tracing

import (
	"bytes"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeHeader(&buf, Header{Version: 1, Pid: 42, Start: 7, Kinds: Kinds[:]}); err != nil {
		t.Fatal(err)
	}
	enc := make([]byte, RecordSize)
	want := Record{Time: 123, Kind: KindRender, Phase: PhaseEnd, CPU: 3, TID: 99, Obj: 5, Span: 6, A: -1, B: 1 << 40}
	encode(enc, &want)
	buf.Write(enc)
	// A name record followed by its padded text.
	encode(enc, &Record{Kind: KindName, Obj: 11, Span: 5})
	buf.Write(enc)
	buf.WriteString("hello\x00\x00\x00")
	encode(enc, &want)
	buf.Write(enc[:RecordSize-3]) // truncated tail from a killed writer

	tr, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Header.Pid != 42 || len(tr.Records) != 1 || tr.Records[0] != want || tr.Names[11] != "hello" {
		t.Fatalf("got header %+v records %+v names %v", tr.Header, tr.Records, tr.Names)
	}
	if len(Kinds) != int(kindCount) || Kinds[KindMetric].Name == "" {
		t.Fatal("every kind needs a description")
	}
}
