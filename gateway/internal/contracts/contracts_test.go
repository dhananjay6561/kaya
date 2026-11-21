package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	shared "github.com/dhananjay6561/kaya/contracts"
)

func validator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func samples(t *testing.T, schema, kind string) map[string][]byte {
	t.Helper()
	dir := path.Join("samples/v1", schema, kind)
	entries, err := fs.ReadDir(shared.Samples, dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		raw, err := fs.ReadFile(shared.Samples, path.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(e.Name(), ".json")] = raw
	}
	if len(out) == 0 {
		t.Fatalf("no samples in %s", dir)
	}
	return out
}

func sameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("round trip changed the payload\nwant %s\ngot  %s", want, got)
	}
}

func typedData(t EventType) any {
	switch t {
	case EventConnectionState:
		return &ConnectionState{}
	case EventMessageStored:
		return &MessageStored{}
	case EventHistorySynced:
		return &HistorySynced{}
	case EventSendResult:
		return &SendResult{}
	}
	return nil
}

func strict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func TestValidEventSamplesRoundTripThroughGoTypes(t *testing.T) {
	v := validator(t)
	for name, raw := range samples(t, "event", "valid") {
		t.Run(name, func(t *testing.T) {
			e, err := v.DecodeEvent(raw)
			if err != nil {
				t.Fatalf("valid sample rejected: %v", err)
			}
			data := typedData(e.Type)
			if data == nil {
				t.Fatalf("no Go type for %q", e.Type)
			}
			if err := e.DecodeData(data); err != nil {
				t.Fatal(err)
			}
			again, err := NewEvent(e.Type, data)
			if err != nil {
				t.Fatal(err)
			}
			again.ID, again.OccurredAt = e.ID, e.OccurredAt
			out, err := v.EncodeEvent(again)
			if err != nil {
				t.Fatalf("re-encoded event rejected: %v", err)
			}
			sameJSON(t, raw, out)
		})
	}
}

func TestInvalidEventSamplesAreRejected(t *testing.T) {
	v := validator(t)
	for name, raw := range samples(t, "event", "invalid") {
		t.Run(name, func(t *testing.T) {
			if _, err := v.DecodeEvent(raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid sample accepted (err=%v)", err)
			}
		})
	}
}

func TestValidSendRequestSamplesRoundTripThroughGoTypes(t *testing.T) {
	v := validator(t)
	for name, raw := range samples(t, "send-request", "valid") {
		t.Run(name, func(t *testing.T) {
			r, err := v.DecodeSendRequest(raw)
			if err != nil {
				t.Fatalf("valid sample rejected: %v", err)
			}
			var strictCopy SendRequest
			if err := strict(raw, &strictCopy); err != nil {
				t.Fatalf("sample has a field the Go type lacks: %v", err)
			}
			out, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.ValidateSendRequest(out); err != nil {
				t.Fatalf("re-encoded request rejected: %v", err)
			}
			sameJSON(t, raw, out)
		})
	}
}

func TestInvalidSendRequestSamplesAreRejected(t *testing.T) {
	v := validator(t)
	for name, raw := range samples(t, "send-request", "invalid") {
		t.Run(name, func(t *testing.T) {
			if _, err := v.DecodeSendRequest(raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid sample accepted (err=%v)", err)
			}
		})
	}
}

func TestEncodeEventRefusesBadData(t *testing.T) {
	v := validator(t)
	e, err := NewEvent(EventConnectionState, ConnectionState{State: "sleeping"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.EncodeEvent(e); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestNewEventUsesUTCAndAFreshUUID(t *testing.T) {
	v := validator(t)
	a, _ := NewEvent(EventHistorySynced, HistorySynced{Complete: true})
	b, _ := NewEvent(EventHistorySynced, HistorySynced{Complete: true})
	if a.ID == b.ID {
		t.Fatal("ids must differ")
	}
	if a.OccurredAt.Location() != time.UTC {
		t.Fatal("time must be UTC")
	}
	if _, err := v.EncodeEvent(a); err != nil {
		t.Fatalf("fresh event rejected: %v", err)
	}
}

func TestEncodeEventConvertsLocalTimeToUTC(t *testing.T) {
	v := validator(t)
	e, _ := NewEvent(EventHistorySynced, HistorySynced{Complete: true})
	e.OccurredAt = time.Date(2026, 11, 22, 15, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	raw, err := v.EncodeEvent(e)
	if err != nil {
		t.Fatalf("local time should be converted, got %v", err)
	}
	if !bytes.Contains(raw, []byte(`"occurred_at":"2026-11-22T09:30:00Z"`)) {
		t.Fatalf("expected UTC time in %s", raw)
	}
}

func TestDecodeDataRejectsUnknownFields(t *testing.T) {
	e := Event{Type: EventHistorySynced, Data: json.RawMessage(`{"chats":1,"messages":1,"complete":true,"x":1}`)}
	if err := e.DecodeData(&HistorySynced{}); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}
