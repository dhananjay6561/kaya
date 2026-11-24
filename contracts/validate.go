package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const baseURL = "https://kaya.invalid/contracts/v1/"

// ErrInvalid wraps every payload that does not match its schema.
var ErrInvalid = errors.New("payload does not match the contract")

// ValidationError lists where a payload broke the contract and which rule it broke. It never includes the
// payload's values, which can be phone numbers or message text, so it is safe to log.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return ErrInvalid.Error() + ": " + strings.Join(e.Problems, "; ")
}

// Unwrap makes errors.Is(err, ErrInvalid) true.
func (e *ValidationError) Unwrap() error { return ErrInvalid }

const maxProblems = 10

// MaxSendTTL is the longest a send request may wait between created_at and expires_at.
const MaxSendTTL = 24 * time.Hour

func problems(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{"/ invalid"}
	}
	seen := map[string]bool{}
	var out []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		p := "/" + strings.Join(e.InstanceLocation, "/") + " " + strings.Join(e.ErrorKind.KeywordPath(), "/")
		if !seen[p] && len(out) < maxProblems {
			seen[p] = true
			out = append(out, p)
		}
	}
	walk(ve)
	sort.Strings(out)
	return out
}

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

	err := fs.WalkDir(Schemas, "schemas/v1", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".schema.json") {
			return err
		}
		raw, err := fs.ReadFile(Schemas, p)
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
		return &ValidationError{Problems: []string{"/ not JSON"}}
	}
	if err := s.Validate(doc); err != nil {
		return &ValidationError{Problems: problems(err)}
	}
	return nil
}

// ValidateEvent checks raw against the event schema.
func (v *Validator) ValidateEvent(raw []byte) error { return validate(v.event, raw) }

// ValidateSendRequest checks raw against the send-request schema.
func (v *Validator) ValidateSendRequest(raw []byte) error { return validate(v.sendRequest, raw) }

// EncodeEvent marshals e and refuses to return a payload that breaks the contract.
func (v *Validator) EncodeEvent(e Event) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if err := v.ValidateEvent(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

const expiryProblem = "/expires_at must be after created_at and at most 24 h later"

// EncodeSendRequest marshals r and refuses to return a payload that breaks the contract.
func (v *Validator) EncodeSendRequest(r SendRequest) ([]byte, error) {
	if ttl := r.ExpiresAt.Sub(r.CreatedAt.Time); ttl < time.Millisecond || ttl > MaxSendTTL {
		return nil, &ValidationError{Problems: []string{expiryProblem}}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if err := v.ValidateSendRequest(raw); err != nil {
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
		return Event{}, &ValidationError{Problems: []string{"/ does not fit the Go types"}}
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
		return SendRequest{}, &ValidationError{Problems: []string{"/ does not fit the Go types"}}
	}
	if ttl := r.ExpiresAt.Sub(r.CreatedAt.Time); ttl < time.Millisecond || ttl > MaxSendTTL {
		return SendRequest{}, &ValidationError{Problems: []string{expiryProblem}}
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
