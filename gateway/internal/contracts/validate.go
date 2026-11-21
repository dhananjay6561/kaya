package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	shared "github.com/dhananjay6561/kaya/contracts"
)

const baseURL = "https://kaya.invalid/contracts/v1/"

// ErrInvalid wraps every payload that does not match its schema.
var ErrInvalid = errors.New("payload does not match the contract")

// Validator checks payloads against the shared JSON Schemas.
type Validator struct {
	event       *jsonschema.Schema
	sendRequest *jsonschema.Schema
}

// NewValidator compiles the embedded schemas.
func NewValidator() (*Validator, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()

	err := fs.WalkDir(shared.Schemas, "schemas/v1", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".schema.json") {
			return err
		}
		raw, err := fs.ReadFile(shared.Schemas, p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		return c.AddResource(baseURL+path.Base(p), doc)
	})
	if err != nil {
		return nil, fmt.Errorf("load contract schemas: %w", err)
	}

	v := &Validator{}
	if v.event, err = c.Compile(baseURL + "event.schema.json"); err != nil {
		return nil, fmt.Errorf("compile event schema: %w", err)
	}
	if v.sendRequest, err = c.Compile(baseURL + "send-request.schema.json"); err != nil {
		return nil, fmt.Errorf("compile send-request schema: %w", err)
	}
	return v, nil
}

func validate(s *jsonschema.Schema, raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%w: not JSON: %w", ErrInvalid, err)
	}
	if err := s.Validate(doc); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// ValidateEvent checks raw against the event schema.
func (v *Validator) ValidateEvent(raw []byte) error { return validate(v.event, raw) }

// ValidateSendRequest checks raw against the send-request schema.
func (v *Validator) ValidateSendRequest(raw []byte) error { return validate(v.sendRequest, raw) }

// EncodeEvent marshals e and refuses to return a payload that breaks the contract.
func (v *Validator) EncodeEvent(e Event) ([]byte, error) {
	e.OccurredAt = e.OccurredAt.UTC().Truncate(time.Microsecond)
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if err := v.ValidateEvent(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// DecodeEvent validates raw and returns the envelope; use DecodeData for the typed data.
func (v *Validator) DecodeEvent(raw []byte) (Event, error) {
	if err := v.ValidateEvent(raw); err != nil {
		return Event{}, err
	}
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return e, nil
}

// DecodeSendRequest validates raw and returns the typed request.
func (v *Validator) DecodeSendRequest(raw []byte) (SendRequest, error) {
	if err := v.ValidateSendRequest(raw); err != nil {
		return SendRequest{}, err
	}
	var r SendRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return SendRequest{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return r, nil
}

// DecodeData unmarshals the event's data into dst, rejecting fields dst does not know.
func (e Event) DecodeData(dst any) error {
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s data: %w", e.Type, err)
	}
	return nil
}
