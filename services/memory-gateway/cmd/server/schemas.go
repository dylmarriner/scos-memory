package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const eventEnvelopeSchemaURI = "https://schemas.scos.local/events/event-envelope.schema.json"

// Keep the fallback self-contained so the gateway validates events in its
// container image, where the repository checkout is not present.
//
//go:embed embedded_schemas/events/event-envelope.schema.json
var embeddedSchemas embed.FS

type eventValidator struct {
	schemas map[string]*jsonschema.Schema
}

func newEventValidator(schemaDir string) (*eventValidator, error) {
	if schemaDir == "" {
		return loadEventSchemas(embeddedSchemas)
	}
	return loadEventSchemas(os.DirFS(schemaDir))
}

func loadEventSchemas(files fs.FS) (*eventValidator, error) {
	compiler := jsonschema.NewCompiler()
	resources := make(map[string]struct{})
	err := fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".json") {
			return nil
		}
		document, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		var raw struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(document, &raw); err != nil {
			return fmt.Errorf("read schema %s: %w", path, err)
		}
		if raw.ID == "" {
			return nil
		}
		var resource any
		if err := json.Unmarshal(document, &resource); err != nil {
			return fmt.Errorf("decode schema %s: %w", path, err)
		}
		if err := compiler.AddResource(raw.ID, resource); err != nil {
			return fmt.Errorf("register schema %s: %w", raw.ID, err)
		}
		resources[raw.ID] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := &eventValidator{schemas: make(map[string]*jsonschema.Schema, len(resources))}
	for uri := range resources {
		schema, err := compiler.Compile(uri)
		if err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", uri, err)
		}
		result.schemas[uri] = schema
	}
	if _, ok := result.schemas[eventEnvelopeSchemaURI]; !ok {
		return nil, fmt.Errorf("schema %s is not configured", eventEnvelopeSchemaURI)
	}
	return result, nil
}

func (v *eventValidator) validate(envelope map[string]any) error {
	if v == nil {
		return errors.New("event schema validator is not configured")
	}
	dataSchema, ok := envelope["dataschema"].(string)
	if !ok || strings.TrimSpace(dataSchema) == "" {
		return errors.New("event dataschema must be a non-empty URI")
	}
	u, err := url.Parse(dataSchema)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("event dataschema is not an absolute URI: %q", dataSchema)
	}
	schema, ok := v.schemas[dataSchema]
	if !ok {
		return fmt.Errorf("event dataschema is not registered: %s", dataSchema)
	}
	if err := schema.Validate(envelope); err != nil {
		return fmt.Errorf("event envelope does not match %s: %w", dataSchema, err)
	}
	return nil
}
