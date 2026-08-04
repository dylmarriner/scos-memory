package main

import "testing"

func validEnvelope() map[string]any {
	return map[string]any{
		"specversion":     "1.0",
		"id":              "550e8400-e29b-41d4-a716-446655440000",
		"source":          "scos.memory-gateway",
		"type":            "scos.turn.completed.v1",
		"subject":         "turn/550e8400-e29b-41d4-a716-446655440000",
		"time":            "2026-07-27T12:00:00Z",
		"datacontenttype": "application/json",
		"dataschema":      eventEnvelopeSchemaURI,
		"tenantid":        "550e8400-e29b-41d4-a716-446655440001",
		"actorid":         "550e8400-e29b-41d4-a716-446655440002",
		"data":            map[string]any{"message_id": "550e8400-e29b-41d4-a716-446655440003"},
	}
}

func TestEventEnvelopeValidation(t *testing.T) {
	validator, err := newEventValidator("")
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.validate(validEnvelope()); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
}

func TestEventEnvelopeValidationRejectsInvalidEnvelope(t *testing.T) {
	validator, err := newEventValidator("")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"invalid type", func(event map[string]any) { event["type"] = "turn.completed" }},
		{"unknown schema URI", func(event map[string]any) { event["dataschema"] = "https://schemas.example.invalid/event.json" }},
		{"invalid payload", func(event map[string]any) { event["data"] = "not-an-object" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := validEnvelope()
			tt.edit(event)
			if err := validator.validate(event); err == nil {
				t.Fatal("invalid envelope was accepted")
			}
		})
	}
}
