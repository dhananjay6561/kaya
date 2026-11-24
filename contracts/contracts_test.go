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

	"github.com/dhananjay6561/kaya/contracts/samples"
)

func validator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func loadSamples(t *testing.T, schema, kind string) map[string][]byte {
	t.Helper()
	dir := path.Join("v1", schema, kind)
	entries, err := fs.ReadDir(samples.V1, dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		raw, err := fs.ReadFile(samples.V1, path.Join(dir, e.Name()))
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
	for name, raw := range loadSamples(t, "event", "valid") {
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
	for name, raw := range loadSamples(t, "event", "invalid") {
		t.Run(name, func(t *testing.T) {
			if _, err := v.DecodeEvent(raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid sample accepted (err=%v)", err)
			}
		})
	}
}

func TestValidSendRequestSamplesRoundTripThroughGoTypes(t *testing.T) {
	v := validator(t)
	for name, raw := range loadSamples(t, "send-request", "valid") {
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
	for name, raw := range loadSamples(t, "send-request", "invalid") {
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
	if _, err := v.EncodeEvent(a); err != nil {
		t.Fatalf("fresh event rejected: %v", err)
	}
}

func TestEveryTimeFieldEncodesAsUTC(t *testing.T) {
	v := validator(t)
	ist := time.FixedZone("IST", 5*3600+1800)
	local := time.Date(2026, 11, 22, 15, 0, 0, 123456789, ist)
	e, err := NewEvent(EventMessageStored, MessageStored{
		MessageID: 1, ChatID: 1, SenderID: 1, WAID: "X", Kind: "text", Source: "live", Change: "new",
		SentAt: At(local),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.OccurredAt = At(local)
	raw, err := v.EncodeEvent(e)
	if err != nil {
		t.Fatalf("a non-UTC time must still encode, got %v", err)
	}
	for _, field := range []string{`"occurred_at":"2026-11-22T09:30:00.123456Z"`, `"sent_at":"2026-11-22T09:30:00.123456Z"`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Errorf("expected %s in %s", field, raw)
		}
	}

	req, err := json.Marshal(SendRequest{
		ID: NewID(), Version: Version, CreatedAt: At(local), ExpiresAt: At(local.Add(time.Minute)),
		To: "919999999999@s.whatsapp.net", Authorization: Authorization{Kind: AuthSelfChat},
		Content: TextContent{Type: "text", Text: "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateSendRequest(req); err != nil {
		t.Fatalf("send request with local times rejected: %v", err)
	}
}

func TestValidationErrorsNeverContainPayloadValues(t *testing.T) {
	v := validator(t)
	secrets := map[string]string{
		"phone-number-not-jid": "+919999999999",
		"to-status-broadcast":  "status@broadcast",
	}
	for name, secret := range secrets {
		raw := loadSamples(t, "send-request", "invalid")[name]
		_, err := v.DecodeSendRequest(raw)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the value: %v", name, err)
		}
		if !strings.Contains(err.Error(), "/to ") {
			t.Errorf("%s: error should name the field: %v", name, err)
		}
	}

	e, _ := NewEvent(EventConnectionState, ConnectionState{State: StateConnected, OwnJID: "919999999999:12@s.whatsapp.net"})
	_, err := v.EncodeEvent(e)
	var ve *ValidationError
	if !errors.As(err, &ve) || strings.Contains(err.Error(), "919999999999") {
		t.Fatalf("expected a value-free ValidationError, got %v", err)
	}
	if len(ve.Problems) == 0 || len(ve.Problems) > maxProblems {
		t.Fatalf("unexpected problems %q", ve.Problems)
	}
}

func TestDecodeDataRejectsUnknownFields(t *testing.T) {
	e := Event{Type: EventHistorySynced, Data: json.RawMessage(`{"chats":1,"messages":1,"complete":true,"x":1}`)}
	if err := e.DecodeData(&HistorySynced{}); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}
