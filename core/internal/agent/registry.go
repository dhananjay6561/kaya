// Package agent lets the model use tools: a registry of what it may call, and a loop that runs
// the calls it makes and gives it the results until it answers.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/dhananjay6561/kaya/core/internal/llm"
)

// Tool is something the model can ask Kaya to do. Parameters is a JSON Schema of the arguments,
// an object. Run gets the arguments once they fit it, with private details put back.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	// Outward is true for a tool that acts beyond Kaya: sends a message, spends money, changes
	// something DJ would see elsewhere.
	Outward bool
	Run     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Problem is a failure the model is told about in the tool's own words, so it can try another way.
// Any other error is reported to the model only as a failure.
type Problem struct{ Message string }

func (p *Problem) Error() string { return p.Message }

// Problemf returns a Problem.
func Problemf(format string, a ...any) error { return &Problem{Message: fmt.Sprintf(format, a...)} }

// ErrOutward refuses an outward tool: nothing asks DJ before it acts yet.
var ErrOutward = errors.New("an outward tool needs DJ to confirm each action, and nothing asks him yet")

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Registry holds the tools the model is offered.
type Registry struct {
	tools   map[string]Tool
	schemas map[string]*jsonschema.Schema
	specs   []llm.ToolSpec
}

// NewRegistry checks the tools and returns a registry of them.
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{tools: map[string]Tool{}, schemas: map[string]*jsonschema.Schema{}}
	for _, t := range tools {
		if err := r.add(t); err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}
	}
	return r, nil
}

func (r *Registry) add(t Tool) error {
	switch {
	case !toolName.MatchString(t.Name):
		return errors.New("a name is lowercase letters, digits and underscores")
	case r.tools[t.Name].Name != "":
		return errors.New("a tool already has that name")
	case strings.TrimSpace(t.Description) == "":
		return errors.New("a tool needs a description")
	case t.Run == nil:
		return errors.New("a tool needs Run")
	case t.Outward:
		return ErrOutward
	}
	var shape struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(t.Parameters, &shape); err != nil || shape.Type != "object" {
		return errors.New(`parameters must be a JSON Schema with "type": "object"`)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(t.Parameters))
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	url := "kaya:tool/" + t.Name
	if err := c.AddResource(url, doc); err != nil {
		return err
	}
	schema, err := c.Compile(url)
	if err != nil {
		return fmt.Errorf("parameters: %w", err)
	}
	r.tools[t.Name], r.schemas[t.Name] = t, schema
	r.specs = append(r.specs, llm.ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	return nil
}

// Specs are the tools as the model is offered them, in the order they were given.
func (r *Registry) Specs() []llm.ToolSpec { return slices.Clone(r.specs) }

// Names are the tools' names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Tokens is about what offering the tools adds to a request, counted with count.
func (r *Registry) Tokens(count func(string) int) int {
	if len(r.specs) == 0 {
		return 0
	}
	raw, err := json.Marshal(r.specs)
	if err != nil {
		return 0
	}
	return count(string(raw))
}

// check reads arguments the model wrote and says, in words for the model, why they do not fit.
func (r *Registry) check(name, arguments string) (any, string) {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(arguments))
	if err != nil {
		return nil, "the arguments are not valid JSON"
	}
	if err := r.schemas[name].Validate(doc); err != nil {
		return nil, "the arguments do not fit the parameters: " + strings.Join(problems(err), "; ")
	}
	var args any
	d := json.NewDecoder(strings.NewReader(arguments))
	d.UseNumber()
	if err := d.Decode(&args); err != nil {
		return nil, "the arguments are not valid JSON"
	}
	return args, ""
}

const maxProblems = 5

var english = message.NewPrinter(language.English)

func problems(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{"invalid"}
	}
	var out []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		at := "/" + strings.Join(e.InstanceLocation, "/")
		if p := at + ": " + e.ErrorKind.LocalizedString(english); !slices.Contains(out, p) && len(out) < maxProblems {
			out = append(out, p)
		}
	}
	walk(ve)
	sort.Strings(out)
	return out
}
